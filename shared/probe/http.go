package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// maxRedirects 是跟随重定向的上限(与 Go 默认一致;UptimeKuma 的 axios 是 5 跳)。
const maxRedirects = 10

// vhostTarget 处理"URL 用字面 IP + 配了 Host 请求头"的监控(IP 直连选源:虚拟主机、
// CDN 回源,从 UptimeKuma 导入的监控里常见)。返回应写到 URL 上的主机名,以及真正
// 要连接的 `IP:端口`;不需要改写时两者都是空串(保持既有行为)。
//
// 为什么非要改 URL:Host 头可以走 req.Host 解决,但 HTTPS 的 SNI 与证书校验用的是
// URL 主机名 —— 直连 IP 时证书不会有该 IP 的 SAN,握手必然失败。按 curl --resolve 的
// 语义把请求整体按虚拟主机名走,Host 头、SNI、证书校验就都对了,跟随到别处的跳转也
// 会正常解析到那个域名(见 DialContext 里的同名判断)。
//
// 端口:沿用 URL 自己的(与 curl 一致);Host 头里带的端口只体现在请求头里。
// 只认"URL 主机是字面 IP":URL 本来就是域名时,Host 头只是 HTTP 层路由,
// 顺手改掉 SNI/证书校验会改变既有监控的行为。
func vhostTarget(u *url.URL, hostHeader string) (urlHost, dialAddr string) {
	ip := u.Hostname()
	if hostHeader == "" || ip == "" || net.ParseIP(ip) == nil {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	if port == "" {
		return "", ""
	}
	return hostNoPort(hostHeader), net.JoinHostPort(ip, port)
}

// defaultPort 常见协议的默认端口(URL 里没写端口时用)。
func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// hostNoPort 取主机名部分(去掉可能带的端口);没有端口时原样返回。
// net.SplitHostPort 对不带端口的输入会报错,故这里忽略错误。
func hostNoPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func executeHTTP(ctx context.Context, raw json.RawMessage) (*protocol.ProbeResultPayload, error) {
	run, err := runHTTP(ctx, raw, false)
	if err != nil {
		return nil, err
	}
	return run.Result, nil
}

// maxTestBodyExcerpt 是测试结果里回传的响应体上限(8KB):弹窗里只是给人眼看一眼,
// 但"正文被截断"本身也是信息,所以另外回报总字节数。
const maxTestBodyExcerpt = 8 << 10

// httpRun 一次 HTTP 探测的完整产物:Result 是给轮次用的判定结果;Detail 只在
// "测试"路径采集(轮次路径传 capture=false,连分配都不用做)。
type httpRun struct {
	Result *protocol.ProbeResultPayload
	Detail *protocol.ProbeTestDetail
}

// runHTTP 执行一次 HTTP 探测。capture=true 时额外采集请求与响应明细
// (测试按钮用:失败时要把请求和返回内容摆给用户看)。
func runHTTP(ctx context.Context, raw json.RawMessage, capture bool) (*httpRun, error) {
	var cfg checkconfig.HTTP
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析 HTTP 探测配置失败: %w", err)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, cfg.URL, body)
	if err != nil {
		return nil, err
	}
	// 请求头:Host 是唯一一个不能走 Header 的 —— net/http 写请求时用的是 req.Host
	// (没设才回落到 URL.Host),Header 里的 "Host" 会被直接丢弃。不单独处理的话,
	// 靠 Host 选源的监控(IP 直连 + 虚拟主机/CDN,UptimeKuma 导入里常见)配了也发不出去,
	// 目标只看得到 IP,于是稳定 404。键名大小写不敏感,与 HTTP 头一致。
	hostHeader := ""
	for k, v := range cfg.Headers {
		if strings.EqualFold(k, "Host") {
			hostHeader = v
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	// IP 直连 + Host 选源时还要让 TLS 也对上(SNI/证书校验用的是 URL 主机名,
	// 直连 IP 的证书不会有该 IP 的 SAN,握手必然失败):按 curl --resolve 的语义,
	// 请求按虚拟主机名走,只有真正建连时才换成配置里的 IP。
	// vhost/network 进连接池的缓存键(httpclient.go):按"拨号参数"共用 Transport,
	// 让 keep-alive 连接跨轮复用 —— 此前每次探测新建 Transport 且无人回收,
	// 连接数随探测次数线性增长(见 .scratch/http-conn-reuse/spec.md)。
	vhostName, connectAddr := "", ""
	if vhost, dialAddr := vhostTarget(req.URL, hostHeader); vhost != "" {
		req.URL.Host = vhost
		vhostName, connectAddr = vhost, dialAddr
	}

	transport, cached := transportFor(transportParams{
		insecure: cfg.AllowInsecureTLS,
		network:  checkconfig.Network(cfg.IPVersion, "tcp"),
		vhost:    vhostName,
		dialAddr: connectAddr,
	})
	// 缓存已满时拿到的是一次性实例:用完必须收掉空闲连接,否则退化回连接堆积。
	if !cached {
		defer transport.CloseIdleConnections()
	}

	// 跟随重定向(与 UptimeKuma 一致:它用 axios 的默认行为、最多 5 跳):
	// 目标做了 301/302/307 跳转时按最终响应判定,而不是把 302 当成"状态码不符合期望"。
	// 上限 10 跳,超过即判失败并说明是重定向过多(避免跳转环把超时耗光)。
	// redirects 记下跳转链,测试结果里展示"最终落在哪"。
	var redirects []string
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("重定向超过 %d 次", maxRedirects)
			}
			redirects = append(redirects, r.URL.String())
			return nil
		},
	}

	// 请求明细在真正发出前采集:经过 Host 选源的改写后,这里就是实际请求的样子。
	var detail *protocol.ProbeTestDetail
	if capture {
		detail = &protocol.ProbeTestDetail{
			Method: method, URL: req.URL.String(), Headers: cfg.Headers, Body: cfg.Body,
		}
		if connectAddr != "" {
			detail.Target = connectAddr
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := float64(time.Since(start).Nanoseconds()) / 1e6
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw2, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 关键字匹配上限 1MB

	ok := httpStatusOK(resp.StatusCode, cfg.ExpectStatusCodes)
	if ok {
		// 关键字在两个文本视图上匹配:原始响应体,以及合法 JSON 的反转义文本
		// (目标把中文写成 \uXXXX 时,只有后者能命中)。见 keyword.go。
		ok = keywordOK(raw2, cfg.ExpectContains, cfg.ExpectNotContains)
	}
	// JSON 断言(与 UptimeKuma 的 JSON 查询同语义):表达式错误或取值类型不合法都算失败。
	var assert *jsonAssertResult
	if ok && cfg.JSONAssertEnabled() {
		res, _ := evalJSONAssert(raw2, cfg.JSONPath, cfg.JSONPathOperator, cfg.ExpectedValue)
		assert = &res
		ok = res.Err == nil && res.OK
	}
	var errMsg string
	if !ok {
		errMsg = describeHTTPFail(resp.StatusCode, cfg, raw2, assert)
	}
	if detail != nil {
		detail.Status = resp.StatusCode
		detail.StatusText = resp.Status
		detail.RespHeaders = headerMap(resp.Header)
		detail.FinalURL = resp.Request.URL.String()
		detail.Redirects = redirects
		detail.BodyBytes = int64(len(raw2))
		detail.BodyExcerpt, detail.BodyTruncated, detail.BodyNote = bodyExcerpt(raw2, resp.Header.Get("Content-Type"))
	}
	return &httpRun{
		Result: &protocol.ProbeResultPayload{
			OK:             ok,
			LatencyMs:      latency,
			HTTPStatus:     resp.StatusCode,
			Error:          errMsg,
			StartedAtUnix:  start.Unix(),
			FinishedAtUnix: time.Now().Unix(),
		},
		Detail: detail,
	}, nil
}

// headerMap 把响应头摊平成可 JSON 序列化的字符串表(多值合并,顺序保持原样)。
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// bodyExcerpt 截取响应体片段用于展示。二进制内容不回传正文:塞进 JSON 只会在页面上
// 变成一屏乱码,一句"二进制内容(N 字节)"对排障更有用。
func bodyExcerpt(body []byte, contentType string) (excerpt string, truncated bool, note string) {
	if len(body) == 0 {
		return "", false, ""
	}
	cut := body
	if len(cut) > maxTestBodyExcerpt {
		cut = cut[:maxTestBodyExcerpt]
		truncated = true
	}
	if !utf8.Valid(cut) || binaryContentType(contentType) {
		return "", truncated, fmt.Sprintf("响应体未展示(%s,%d 字节)", describeContentType(contentType), len(body))
	}
	return string(cut), truncated, ""
}

// binaryContentType 明显不是给人看的响应类型(图片/音视频/压缩包/PDF/字体)。
func binaryContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	for _, p := range []string{"image/", "audio/", "video/", "font/", "application/octet-stream",
		"application/zip", "application/gzip", "application/pdf", "application/x-tar", "application/x-7z"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func describeContentType(ct string) string {
	if strings.TrimSpace(ct) == "" {
		return "无 Content-Type"
	}
	return "Content-Type: " + ct
}

func httpStatusOK(status int, expected []int) bool {
	if len(expected) == 0 {
		return status >= 200 && status < 300
	}
	for _, e := range expected {
		if status == e {
			return true
		}
	}
	return false
}

func describeHTTPFail(status int, cfg checkconfig.HTTP, body []byte, assert *jsonAssertResult) string {
	if !httpStatusOK(status, cfg.ExpectStatusCodes) {
		return fmt.Sprintf("状态码 %d 不符合期望", status)
	}
	// 与 keywordOK 同一套文本视图:关键字只在反转义文本里命中时,这里也不该报"缺少"。
	cands := keywordCandidates(body)
	for _, kw := range cfg.ExpectContains {
		if kw != "" && !containsKeyword(cands, kw) {
			return fmt.Sprintf("响应体缺少关键字 %q", truncate(kw, 40))
		}
	}
	for _, kw := range cfg.ExpectNotContains {
		if kw != "" && containsKeyword(cands, kw) {
			return fmt.Sprintf("响应体包含禁止关键字 %q", truncate(kw, 40))
		}
	}
	if assert != nil {
		if assert.Err != nil {
			return assert.Err.Error()
		}
		op := normalizeOperator(cfg.JSONPathOperator)
		return fmt.Sprintf("JSON 查询结果 %q 不满足 %s %q(表达式 %s)",
			truncate(assert.Value, 40), op, truncate(cfg.ExpectedValue, 40), cfg.JSONPath)
	}
	return "检测失败"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
