package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/store"
)

// pushPathPrefix 是外部上报地址的前缀,与 UptimeKuma 的 /api/push/<token> 一致:
// 从 Kuma 迁移过来的脚本只要把域名换成 UptimeMesh 的地址即可继续用。
const pushPathPrefix = "/api/push/"

// NewPushToken 生成一个上报令牌(16 字节随机数的十六进制,32 字符)。
// 令牌是免登录上报端点的唯一凭据,必须不可猜。
func NewPushToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// PushSink 接收外部上报的组件(由调度器实现):上报要落成轮次、驱动统计与告警,
// 这些都在调度器里(Dashboard 的轮次只有一个写入者,见 ADR-0003)。
type PushSink interface {
	ReportPush(ctx context.Context, m *store.Monitor, ok bool, latencyMs float64, note string)
}

// registerPushRoutes 挂载外部上报端点。它必须免鉴权(调用方是路由器脚本之类的
// 外部系统,拿不到也不会带 JWT),凭据就是 URL 里的令牌。
func (a *API) registerPushRoutes(s *ghttp.Server) {
	s.BindHandler(pushPathPrefix+"{token}", a.receivePush)
}

// receivePush 处理一次外部上报,参数与 UptimeKuma 的 push 监控一致:
//
//	GET|POST /api/push/{token}?status=up|down&msg=...&ping=123
//
// status 缺省为 up(与 Kuma 相同);ping 是上报方给出的耗时(毫秒,可选)。
// 返回 {"ok":true},失败返回 404 + {"ok":false,"msg":...}(也是 Kuma 的形态)。
func (a *API) receivePush(r *ghttp.Request) {
	token := strings.TrimSpace(r.Get("token").String())
	reply := func(code int, ok bool, msg string) {
		r.Response.ClearBuffer()
		r.Response.WriteHeader(code)
		r.Response.WriteJson(g.Map{"ok": ok, "msg": msg})
		r.Exit()
	}
	if token == "" {
		reply(http.StatusNotFound, false, "上报令牌为空")
		return
	}
	m, err := a.Store.FindMonitorByPushToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			reply(http.StatusNotFound, false, "上报令牌无效:监控不存在或不是 push 类型")
			return
		}
		reply(http.StatusInternalServerError, false, "查询监控失败")
		return
	}
	if !m.Enabled {
		reply(http.StatusNotFound, false, "监控已暂停,忽略本次上报")
		return
	}

	status := strings.ToLower(strings.TrimSpace(r.Get("status").String()))
	if status == "" {
		status = "up" // 与 UptimeKuma 一致:不传 status 视为正常
	}
	ok := status != "down"
	msg := strings.TrimSpace(r.Get("msg").String())
	latency := parsePushPing(r.Get("ping").String())

	note := msg
	if !ok && note == "" {
		note = "外部上报 status=down"
	}
	if a.PushSink == nil {
		reply(http.StatusServiceUnavailable, false, "上报接收器未就绪,请稍后重试")
		return
	}
	a.PushSink.ReportPush(r.Context(), m, ok, latency, note)
	g.Log().Debugf(r.Context(), "收到外部上报:监控 %s(status=%s, ping=%s)", m.Name, status, r.Get("ping").String())
	reply(http.StatusOK, true, "")
}

// parsePushPing 解析上报的耗时(毫秒);非法或负数按 0(不记录耗时)处理。
// 上限 1e11 毫秒,与 UptimeKuma 的校验一致(防止把浮点/时间戳误当耗时)。
func parsePushPing(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1e11 {
		return 0
	}
	return v
}

// pushURLOf 返回某监控的相对上报地址(前端补上自己的 origin 展示/复制)。
func pushURLOf(m *store.Monitor) string {
	if m.Type != "push" || m.PushToken == "" {
		return ""
	}
	return pushPathPrefix + m.PushToken
}
