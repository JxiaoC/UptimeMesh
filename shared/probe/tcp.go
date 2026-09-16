package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// TCP 端口探测(对应 UptimeKuma 的「TCP Port」):与目标建立 TCP 连接即算成功,
// 不做任何应用层交互(拿到 banner 也不解析),连接后立即关闭。
func init() { Register(checkconfig.TypeTCP, executeTCP) }

func executeTCP(ctx context.Context, raw json.RawMessage) (*protocol.ProbeResultPayload, error) {
	var cfg checkconfig.TCP
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析 TCP 探测配置失败: %w", err)
	}
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("TCP 探测缺少目标主机")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("TCP 探测端口非法: %d", cfg.Port)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: timeout}
	start := time.Now()
	// IP 协议族:auto 走系统默认("tcp");ipv4/ipv6 收窄成 tcp4/tcp6,
	// 于是域名只会被解析成该族地址,目标没有该族地址即失败。
	conn, err := dialer.DialContext(callCtx, checkconfig.Network(cfg.IPVersion, "tcp"), address)
	latency := float64(time.Since(start).Nanoseconds()) / 1e6
	if err != nil {
		return nil, fmt.Errorf("TCP 连接 %s 失败: %s", address, tcpDialReason(err))
	}
	// 连接成功即成功;不读不写,直接关闭(探测不改变对端状态)。
	_ = conn.Close()

	return &protocol.ProbeResultPayload{
		OK: true, LatencyMs: latency,
		StartedAtUnix: start.Unix(), FinishedAtUnix: time.Now().Unix(),
	}, nil
}

// tcpDialReason 把底层拨号错误翻译成可读原因(超时/拒绝/解析失败等)。
func tcpDialReason(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "无法解析主机名: " + dnsErr.Error()
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return "连接超时"
	}
	if isConnRefused(err.Error()) {
		return "连接被拒绝(目标未监听该端口,或被防火墙拦截)"
	}
	return err.Error()
}

// isConnRefused 按错误文本识别"连接被拒绝":
// Windows 是 "No connection could be made because the target machine actively refused it",
// Unix 是 "connect: connection refused",中文系统另有译法。
func isConnRefused(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "actively refused") ||
		strings.Contains(msg, "拒绝")
}
