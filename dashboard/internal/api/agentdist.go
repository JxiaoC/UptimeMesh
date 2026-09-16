package api

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/shared/agentclient"
)

// install.sh 由「节点」页的一键安装命令拉取;随 Dashboard 二进制一起分发。
//
//go:embed install.sh
var agentInstallScript string

// agentArchs 允许下载的架构白名单,与 deploy/build-agent.sh 的交叉编译目标一致。
var agentArchs = map[string]bool{"amd64": true, "arm64": true}

// registerAgentDistRoutes 节点安装包分发。刻意放在 JWT 之外:
// 目标机器用 curl 直接拉取,无法携带登录令牌;真正的准入控制仍是全局接入密钥。
func (a *API) registerAgentDistRoutes(group *ghttp.RouterGroup) {
	group.GET("/agent/install.sh", a.agentInstallScriptHandler)
	group.GET("/agent/download/linux-{arch}", a.agentDownload)
}

func (a *API) agentInstallScriptHandler(r *ghttp.Request) {
	r.Response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	r.Response.Header().Set("Cache-Control", "no-cache")
	r.Response.Write(agentInstallScript)
}

// agentDownload 返回某个架构的 Agent 二进制。镜像内由 Dockerfile 交叉编译后放到
// AGENT_DIST_DIR;未内置时返回 503,提示改用源码构建或 Docker 方式。
func (a *API) agentDownload(r *ghttp.Request) {
	arch := r.Get("arch").String()
	if !agentArchs[arch] {
		r.Response.WriteStatus(404)
		return
	}
	name := agentBinaryName(arch)
	path := filepath.Join(agentDistDir(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		g.Log().Warningf(r.Context(), "节点安装包缺失: %s", path)
		r.Response.WriteStatus(503)
		r.Response.Write("节点安装包未内置,请检查仪表盘镜像构建或 AGENT_DIST_DIR 配置")
		return
	}
	r.Response.Header().Set("Content-Type", "application/octet-stream")
	r.Response.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	r.Response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	r.Response.Write(data)
}

// agentDistDir 定位内置安装包目录:环境变量优先,其次容器内固定路径,
// 最后回退本地交叉编译产物 bin/(deploy/build-agent.sh 的输出)。
func agentDistDir() string {
	if d := os.Getenv("AGENT_DIST_DIR"); d != "" {
		return d
	}
	for _, d := range []string{"/app/agents", "../bin", "bin"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "/app/agents"
}

// agentDistVersionFile 是安装包目录内的版本标记文件,由 deploy/build-agent.sh 与
// deploy/Dockerfile.dashboard 的 agentbuild 阶段写入,且必须与二进制内 -ldflags
// 打标的版本完全一致:「一键升级」以它判定节点是否落后,两者不一致会导致升级后
// 仍反复提示可升级。
const agentDistVersionFile = "VERSION"

func agentBinaryName(arch string) string { return "uptimemesh-agent-linux-" + arch }

// agentDist 描述仪表盘可分发给某一平台节点的一份 Agent 安装包。
type agentDist struct {
	Version string // 分发包版本,同时用于「是否需要升级」的判定
	Path    string
	SHA256  string // 内容校验和:随 upgrade 帧下发,节点校验通过才允许替换自身
}

// findAgentDist 定位某平台的安装包。分发目录只放 Linux 二进制,故非 linux 节点
// 一律不提供升级——否则会把 Linux 包推给 Windows 开发机上的 Agent。
func findAgentDist(ctx context.Context, goos, arch string) (agentDist, bool) {
	if goos != "linux" || !agentArchs[arch] {
		return agentDist{}, false
	}
	path := filepath.Join(agentDistDir(), agentBinaryName(arch))
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return agentDist{}, false
	}
	sum, err := fileSHA256(path, st)
	if err != nil {
		g.Log().Warningf(ctx, "计算节点安装包校验和失败(%s): %v", path, err)
		return agentDist{}, false
	}
	return agentDist{Version: agentDistVersion(), Path: path, SHA256: sum}, true
}

// agentDistVersion 读取安装包目录里的版本标记;缺失时回退到本仓编译期版本
// (手工往目录里放二进制就会走这条路,总比「没有版本」可用)。
func agentDistVersion() string {
	if b, err := os.ReadFile(filepath.Join(agentDistDir(), agentDistVersionFile)); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	return agentclient.Version
}

// distSumCache 缓存安装包校验和(path → 大小 + 修改时间 + 摘要):批量一键升级
// 时同一个大文件只需完整读一次;安装包被替换后大小/时间变化,缓存自然失效。
var distSumCache sync.Map

type cachedSum struct {
	size    int64
	modTime time.Time
	sum     string
}

func fileSHA256(path string, st os.FileInfo) (string, error) {
	if v, ok := distSumCache.Load(path); ok {
		if c := v.(cachedSum); c.size == st.Size() && c.modTime.Equal(st.ModTime()) {
			return c.sum, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	distSumCache.Store(path, cachedSum{size: st.Size(), modTime: st.ModTime(), sum: sum})
	return sum, nil
}
