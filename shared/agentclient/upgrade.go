package agentclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/uptimemesh/shared/protocol"
)

// 自升级的边界:最大下载体积与整体超时(Dashboard 分发的 Agent 约 10MB)。
const (
	maxUpgradeBytes = 256 << 20
	upgradeTimeout  = 3 * time.Minute
)

// capabilities 声明本进程支持的可选能力,随 hello 上报。平台不支持自升级
// (Windows:运行中的可执行文件被系统占用)就不声明,Dashboard 据此不提供一键升级;
// 「测试」所有平台都支持,一律声明。老节点不带这个能力,页面就不给它发 probe_test
// (它不认识该帧,只会静默忽略,页面只能干等超时)。
func capabilities() []string {
	caps := []string{protocol.CapProbeTest}
	if selfUpgradeSupported() {
		caps = append(caps, protocol.CapSelfUpgrade)
	}
	return caps
}

// upgrading 保证同一时刻只跑一个升级:页面重复点击、多端同时下发都只会有一个生效,
// 避免两次替换互相踩,也避免把安装包下载两遍。
var upgrading atomic.Bool

// handleUpgrade 处理 Dashboard 下发的 upgrade 帧:下载 → 校验 → 原子替换 → 重启自身。
// 无论成败都回一帧 upgrade_result(操作者据此看到失败原因);成功后进程映像被换成
// 新二进制,连接随之中断并由新版本重新 hello,Dashboard 侧因此看到版本变化。
func (c *Client) handleUpgrade(ctx context.Context, myToken int64, p *protocol.UpgradePayload) {
	if !selfUpgradeSupported() {
		c.replyUpgrade(ctx, myToken, protocol.UpgradeResultPayload{
			OK: false, Version: Version,
			Error: "当前平台不支持一键升级,请在目标机上重新执行安装脚本",
		})
		return
	}
	exe, err := os.Executable()
	if err != nil {
		c.replyUpgrade(ctx, myToken, protocol.UpgradeResultPayload{
			OK: false, Version: Version, Error: "定位自身可执行文件失败: " + err.Error(),
		})
		return
	}
	res, newExe := c.applyUpgrade(ctx, p, filepath.Clean(exe))
	c.replyUpgrade(ctx, myToken, res)
	if !res.OK || newExe == "" {
		return
	}
	// 先让结果帧落进内核发送缓冲,再替换进程映像(exec 之后本进程的写路径不复存在)。
	time.Sleep(200 * time.Millisecond)
	if err := restartSelf(newExe); err != nil {
		if c.onLog != nil {
			c.onLog("重启自身失败(新版本已就位,重启服务即可生效): %v", err)
		}
	}
}

func (c *Client) replyUpgrade(ctx context.Context, myToken int64, res protocol.UpgradeResultPayload) {
	if c.onLog != nil {
		if res.OK {
			c.onLog("一键升级完成,准备以新版本重启(当前版本 %s)", res.Version)
		} else {
			c.onLog("一键升级失败: %s", res.Error)
		}
	}
	env, err := protocol.NewEnvelope(protocol.FrameUpgradeResult, res)
	if err != nil {
		return
	}
	_ = c.writeFrameEnv(ctx, myToken, env)
}

// applyUpgrade 执行升级的实质步骤,返回结果与(成功时)新的可执行文件路径。
// target 是被替换的可执行文件路径:生产为自身(exe),单测为临时文件。
// 平台能力判断在 handleUpgrade 里做,这里只关心「这次升级能不能落地」。
func (c *Client) applyUpgrade(ctx context.Context, p *protocol.UpgradePayload, target string) (protocol.UpgradeResultPayload, string) {
	fail := func(format string, args ...any) (protocol.UpgradeResultPayload, string) {
		return protocol.UpgradeResultPayload{OK: false, Version: Version, Error: fmt.Sprintf(format, args...)}, ""
	}
	if p.TargetVersion == "" || p.SHA256 == "" {
		// 没有校验和的安装包一律不执行:它无法证明内容就是 Dashboard 分发的那一份。
		return fail("升级指令不完整(缺少目标版本或校验和),已拒绝执行")
	}
	if p.TargetVersion == Version {
		return protocol.UpgradeResultPayload{OK: true, Version: Version}, ""
	}
	if !upgrading.CompareAndSwap(false, true) {
		return fail("已有一次升级正在进行,请稍后再试")
	}
	defer upgrading.Store(false)

	url := upgradeURL(c.opt.ServerURL, runtime.GOARCH)
	if url == "" {
		return fail("无法从接入地址 %q 推导安装包下载地址", c.opt.ServerURL)
	}
	if c.onLog != nil {
		c.onLog("开始升级到 %s:下载 %s", p.TargetVersion, url)
	}
	// 临时文件放在目标可执行文件同目录:替换是同一文件系统内的 rename,才是原子的。
	tmp, err := downloadAgentBinary(ctx, url, p.SHA256, filepath.Dir(target))
	if err != nil {
		return fail("下载或校验安装包失败: %v", err)
	}
	if err := replaceExecutable(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fail("替换可执行文件失败(需对该文件所在目录有写权限): %v", err)
	}
	return protocol.UpgradeResultPayload{OK: true, Version: p.TargetVersion}, target
}

// downloadAgentBinary 下载安装包到 targetDir,校验 sha256 后返回临时文件路径。
// 校验失败或下载中断都会删除临时文件,不在目标目录留下残骸。
func downloadAgentBinary(ctx context.Context, rawURL, wantSHA, targetDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载端点返回 HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(targetDir, ".uptimemesh-agent-upgrade-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	sum := sha256.New()
	// 多读 1 字节用于判断是否超限,避免无上限地把磁盘写满。
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, maxUpgradeBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxUpgradeBytes {
		return "", fmt.Errorf("安装包超过 %d MB 上限", maxUpgradeBytes>>20)
	}
	if err := f.Chmod(0o755); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, wantSHA) {
		return "", fmt.Errorf("校验和不符(期望 %s,实际 %s)", wantSHA, got)
	}
	keep = true
	return tmp, nil
}

// replaceExecutable 用新下载的文件原子覆盖目标路径:对正在运行的可执行文件做
// rename 在 Linux/Darwin 上是允许的(直接写文件才会 ETXTBSY),旧 inode 仍被本进程
// 占用,所以替换后旧进程能一直跑到 exec 为止。仅当 selfUpgradeSupported() 为真
// 时才会走到这里(单测用临时文件调用,与平台无关)。
func replaceExecutable(newPath, target string) error { return os.Rename(newPath, target) }

// upgradeURL 由接入地址推导安装包下载地址:ws→http / wss→https,只取 host[:port],
// 与 install.sh 的推导规则一致。分发端点免鉴权(准入靠全局接入密钥),不参与本流程。
func upgradeURL(serverURL, arch string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "http"
	if u.Scheme == "wss" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/api/v1/agent/download/linux-%s", scheme, u.Host, arch)
}
