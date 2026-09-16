// Package kuma 提供从 UptimeKuma 一次性导入监控配置所需的最小客户端:
// 拉取其 Prometheus /metrics 端点并解析其中的监控列表。
//
// 为什么是 /metrics:UptimeKuma 的「API 密钥」就是为这个端点准备的。
// 见 louislam/uptime-kuma 的 server/server.js(app.get("/metrics", apiAuth, ...))、
// server/auth.js(apiAuth 校验 API 密钥)与 server/prometheus.js(monitor_* 指标),
// 指标自带 monitor_id / monitor_name / monitor_type / monitor_url /
// monitor_hostname / monitor_port 等标签,足以还原监控配置。
//
// 与 ADR-0002(Dashboard 永不执行检测)的关系:这里的 HTTP 拉取是管理员在设置页
// 主动触发的一次性配置读取,结果只用于生成监控配置,绝不进入轮次/结果/可用率等
// 统计口径,因此不构成"Dashboard 执行检测"。
package kuma

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 分类错误:API 层据此给出可操作的中文提示。
var (
	// ErrUnauthorized API 密钥无效,或 UptimeKuma 未启用 API 密钥。
	ErrUnauthorized = errors.New("认证失败")
	// ErrNotFound /metrics 端点不存在(地址写成了别的服务的根路径等)。
	ErrNotFound = errors.New("未找到 /metrics 端点")
	// ErrNoMonitors 拉取成功但指标里没有任何监控(版本过旧/尚无检测数据)。
	ErrNoMonitors = errors.New("指标中没有监控数据")
)

// maxMetricsBytes 单次拉取的响应体上限,避免异常端点吃满内存。
const maxMetricsBytes = 8 << 20

// fetchTimeout 单次拉取的超时;设置页是同步等待,不宜过长。
const fetchTimeout = 15 * time.Second

// UptimeMesh 的周期/超时边界(与 monitorReq.validate 一致),供导入映射做越界提示。
const (
	meshMinPeriod = 10
	meshMaxPeriod = 3600
)

// UptimeMesh 的监控类型取值(与 shared/checkconfig 保持一致)。
const (
	MeshTypeHTTP = "http"
	MeshTypePing = "ping"
	MeshTypeTCP  = "tcp"
	MeshTypePush = "push"
)

// 监控状态口径,与 UptimeKuma 的 src/util.js 一致。
const (
	StatusDown        = 0
	StatusUp          = 1
	StatusPending     = 2
	StatusMaintenance = 3
)

// Client 是 UptimeKuma /metrics 的只读客户端。
type Client struct {
	// BaseURL 用户填的 UptimeKuma 地址,可带端口与子路径,也可直接写 .../metrics。
	BaseURL string
	// APIKey UptimeKuma「设置 → API 密钥」创建的密钥,作为 Basic Auth 的密码。
	APIKey string
	// InsecureTLS 跳过证书校验(自签证书的 UptimeKuma 常见)。
	InsecureTLS bool
	// HTTPClient 仅测试注入;nil 时按 InsecureTLS 构造。
	HTTPClient *http.Client
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.InsecureTLS},
	}
	return &http.Client{Timeout: fetchTimeout, Transport: tr}
}

// BaseURL 是归一化后的 UptimeKuma 接入地址:源(协议 + 主机端口)与子路径分开,
// 便于同时拼出 /metrics(Prometheus)与 /socket.io(Socket.IO 管理接口)。
type BaseURL struct {
	Scheme string
	Host   string
	// Subpath 是反代子路径前缀(形如 "/kuma");根路径部署为空串。
	Subpath string
}

// Origin 返回 scheme://host[:port]。
func (b *BaseURL) Origin() string { return b.Scheme + "://" + b.Host }

// RESTURL 拼出子路径下的 HTTP 接口地址。
func (b *BaseURL) RESTURL(path string) string { return b.Origin() + b.Subpath + path }

// MetricsURL 返回 Prometheus 指标端点。
func (b *BaseURL) MetricsURL() string { return b.RESTURL("/metrics") }

// SocketPath 返回 Socket.IO 的 Engine.IO 路径(子路径部署时位于前缀之下)。
func (b *BaseURL) SocketPath() string { return b.Subpath + "/socket.io" }

// ParseBaseURL 归一化用户填写的地址:可省略 scheme、允许结尾 /、允许直接粘贴
// .../metrics;仅接受 http/https。
func ParseBaseURL(raw string) (*BaseURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("UptimeKuma 地址必填")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("UptimeKuma 地址须为合法的 http(s) 地址")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("UptimeKuma 地址仅支持 http 或 https")
	}
	sub := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/metrics")
	return &BaseURL{Scheme: u.Scheme, Host: u.Host, Subpath: sub}, nil
}

// MetricsURL 归一化地址并拼出 /metrics 端点。
func (c *Client) MetricsURL() (string, error) {
	base, err := ParseBaseURL(c.BaseURL)
	if err != nil {
		return "", err
	}
	return base.MetricsURL(), nil
}

// Fetch 拉取 /metrics 并返回其中的监控列表(按 UptimeKuma 监控 ID 升序)。
// 注意:只有至少产生过一次检测数据的监控才会出现在指标里,暂停中的监控通常不在。
func (c *Client) Fetch(ctx context.Context) ([]Monitor, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("API 密钥必填")
	}
	endpoint, err := c.MetricsURL()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("UptimeKuma 地址不合法")
	}
	// UptimeKuma 的 apiAuth 用 Basic Auth 校验密钥:用户名不参与校验,密码即 API 密钥。
	req.SetBasicAuth("uptimemesh", c.APIKey)
	req.Header.Set("Accept", "text/plain")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 UptimeKuma: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("UptimeKuma 返回 HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricsBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 UptimeKuma 指标失败: %w", err)
	}
	monitors := MonitorsFromSamples(ParseSamples(string(body)))
	if len(monitors) == 0 {
		return nil, ErrNoMonitors
	}
	return monitors, nil
}

// ---- Prometheus 文本格式解析 ----

// Sample 是一条 Prometheus 文本格式的样本。
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParseSamples 解析 prom-client 文本格式;无法识别的行直接跳过(格式兼容优先)。
func ParseSamples(text string) []Sample {
	out := make([]Sample, 0, 64)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if s, ok := parseLine(line); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseLine 解析 `name{k="v",...} value [timestamp]` 或 `name value`。
func parseLine(line string) (Sample, bool) {
	labels := map[string]string{}
	if idx := strings.IndexByte(line, '{'); idx >= 0 {
		end := findLabelEnd(line, idx)
		if end < 0 {
			return Sample{}, false
		}
		labels = parseLabels(line[idx+1 : end])
		line = line[:idx] + " " + line[end+1:]
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Sample{}, false
	}
	value, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return Sample{}, false
	}
	return Sample{Name: fields[0], Labels: labels, Value: value}, true
}

// findLabelEnd 找标签段的右花括号;引号内的 } 属于标签值。
func findLabelEnd(s string, start int) int {
	inQuote := false
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inQuote {
				i++ // 跳过被转义的下一个字符
			}
		case '"':
			inQuote = !inQuote
		case '}':
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

// parseLabels 解析 k="v",k2="v2";引号内的逗号不分割。
func parseLabels(raw string) map[string]string {
	labels := map[string]string{}
	for _, kv := range splitLabels(raw) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(kv[:eq])
		val := strings.TrimSpace(kv[eq+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = unescapeLabel(val[1 : len(val)-1])
		}
		if key != "" {
			labels[key] = val
		}
	}
	return labels
}

func splitLabels(raw string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
	)
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case ch == '\\' && inQuote && i+1 < len(raw):
			cur.WriteByte(ch)
			i++
			cur.WriteByte(raw[i])
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if s := cur.String(); strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}

// unescapeLabel 还原 prom-client 的 \\ \" \n 转义。
func unescapeLabel(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- 监控提取 ----

// Monitor 是 /metrics 中的一个 UptimeKuma 监控(标签 + 最新状态)。
type Monitor struct {
	ID       string
	Name     string
	Type     string
	URL      string
	Hostname string
	Port     string
	// Status 取自 monitor_status:1=UP、0=DOWN、2=PENDING、3=MAINTENANCE。
	Status float64
	// Tags 是内置标签之外的附加标签(即 UptimeKuma 的标签;名称已被其
	// sanitizeForPrometheus 去除非字母数字字符,中文标签会退化为空而被丢弃)。
	Tags map[string]string
}

// StatusText 状态的中文展示。
func (m Monitor) StatusText() string {
	switch int(m.Status) {
	case StatusUp:
		return "正常"
	case StatusDown:
		return "故障"
	case StatusPending:
		return "重试中"
	case StatusMaintenance:
		return "维护中"
	default:
		return "未知"
	}
}

// MonitorMetricNames 是携带监控标签的指标名集合。
var MonitorMetricNames = map[string]bool{
	"monitor_status":                true,
	"monitor_response_time":         true,
	"monitor_uptime_ratio":          true,
	"monitor_response_time_seconds": true,
	"monitor_cert_days_remaining":   true,
	"monitor_cert_is_valid":         true,
}

// builtinLabels 是 UptimeKuma 内置标签,其余标签视为用户标签。
var builtinLabels = map[string]bool{
	"monitor_id":       true,
	"monitor_name":     true,
	"monitor_type":     true,
	"monitor_url":      true,
	"monitor_hostname": true,
	"monitor_port":     true,
	"window":           true, // monitor_uptime_ratio / *_seconds 的滑动窗口
}

// MonitorsFromSamples 从指标样本里还原监控列表(按监控 ID 升序,保证展示稳定)。
func MonitorsFromSamples(samples []Sample) []Monitor {
	byKey := map[string]*Monitor{}
	keys := make([]string, 0, 16)
	for _, s := range samples {
		if !MonitorMetricNames[s.Name] {
			continue
		}
		key := s.Labels["monitor_id"]
		if key == "" {
			key = strings.Join([]string{
				s.Labels["monitor_name"], s.Labels["monitor_type"], s.Labels["monitor_url"],
				s.Labels["monitor_hostname"], s.Labels["monitor_port"],
			}, "|")
		}
		m, ok := byKey[key]
		if !ok {
			m = &Monitor{
				ID:       s.Labels["monitor_id"],
				Name:     s.Labels["monitor_name"],
				Type:     s.Labels["monitor_type"],
				URL:      s.Labels["monitor_url"],
				Hostname: s.Labels["monitor_hostname"],
				Port:     s.Labels["monitor_port"],
				Status:   math.NaN(),
				Tags:     map[string]string{},
			}
			for k, v := range s.Labels {
				if builtinLabels[k] || v == "" {
					continue
				}
				m.Tags[k] = v
			}
			byKey[key] = m
			keys = append(keys, key)
		}
		if s.Name == "monitor_status" && !math.IsNaN(s.Value) {
			m.Status = s.Value
		}
	}
	out := make([]Monitor, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return lessMonitor(out[i].ID, out[j].ID) })
	return out
}

// lessMonitor 按数字 ID 排序(非数字退化为文本比较),让预览列表与 UptimeKuma 一致。
func lessMonitor(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}

// ---- 类型映射 ----

// Candidate 是一条待导入的候选监控。两种数据源共用它:
// 默认的 /metrics(API 密钥)只填前几个字段;账号密码经 Socket.IO 取回的完整
// 配置还会填满下半部分(方法/请求头/请求体/期望状态码/关键词/周期等)。
type Candidate struct {
	Monitor
	// MeshType 映射后的 UptimeMesh 类型(http/ping/tcp/push);Reason 非空时无意义。
	MeshType string
	// Target http 为 URL,ping/tcp 为主机名,push 为空(它没有探测目标)。
	Target string
	// Port 是 tcp 的目标端口。
	Port int
	// Warnings 可导入但语义有损的提示(如 JSON 断言无法还原)。
	Warnings []string
	// Reason 非空表示不支持导入及原因。
	Reason string

	// ---- 以下字段仅账号密码(Socket.IO)全量导入时填充 ----
	// Method 请求方法(空表示未取到,由导入方回落 GET)。
	Method string
	// Headers 请求头;Body 请求体。
	Headers map[string]string
	Body    string
	// ExpectStatusSpecs 期望状态码(UptimeKuma accepted_statuscodes,形如 "200"、"200-299")。
	ExpectStatusSpecs []string
	// Contains/NotContains 关键词校验(来自 keyword + invertKeyword)。
	Contains    []string
	NotContains []string
	// AllowInsecureTLS 来自 UptimeKuma 的 ignoreTls。
	AllowInsecureTLS bool
	// InvertMode 来自 UptimeKuma 的 upsideDown(反转判定:探测失败算正常)。
	InvertMode bool
	// JSONPath/JSONPathOperator/ExpectedValue 来自 UptimeKuma 的 json-query 断言
	// (JSONata 表达式取值后与期望值比较),语义与之完全一致。
	JSONPath         string
	JSONPathOperator string
	ExpectedValue    string
	// Group 是 UptimeKuma 侧的分组容器名(嵌套时拼成"父 / 子");空表示未分组。
	// 与 UptimeMesh 的「分组」同义,导入时落为该监控的分组标签。
	Group string
	// Paused 表示该监控在 UptimeKuma 里处于暂停状态;导入后同样保持暂停。
	Paused bool
	// Period/Timeout 是 UptimeKuma 的检测间隔与超时(秒);0 表示未取到。
	Period  int
	Timeout int
}

// Importable 是否可以导入。
func (c Candidate) Importable() bool { return c.Reason == "" }

// DedupeKey 返回判重键(类型 + 目标),口径见 DedupeKeyOf。
func (c Candidate) DedupeKey() string {
	return DedupeKeyOf(c.MeshType, c.Target, c.Port, c.Name)
}

// DedupeKeyOf 由"类型 + 目标 + 端口 + 名称"组装判重键。候选与已存在监控共用它,
// 保证导入判重两边的口径完全一致:
//
//	http → URL;ping → 主机;tcp → "主机:端口";push → 监控名
//	(push 没有探测目标,名称是它唯一的身份)。
func DedupeKeyOf(typ, target string, port int, name string) string {
	switch typ {
	case MeshTypeTCP:
		target = net.JoinHostPort(strings.TrimSpace(target), strconv.Itoa(port))
	case MeshTypePush:
		target = name
	}
	return TargetKey(typ, target)
}

// MapToMesh 把 Prometheus 指标还原的监控映射为 UptimeMesh 候选:
//   - http / keyword / json-query → HTTP(后两者的内容校验无法从指标还原,给出提示);
//   - ping → PING;
//   - port/tcp → TCP(指标里带 monitor_hostname 与 monitor_port);
//   - push → PUSH(外部上报;令牌由导入时生成,脚本要改指向 UptimeMesh);
//   - 其余类型(dns/docker/group/grpc/db 等)暂不支持,返回原因。
func MapToMesh(m Monitor) Candidate {
	c := Candidate{Monitor: m}
	typ := strings.ToLower(strings.TrimSpace(m.Type))
	url := strings.TrimSpace(m.URL)
	host := strings.TrimSpace(m.Hostname)
	if host == "" {
		host = hostFromURL(url)
	}
	switch typ {
	case "http":
		c.MeshType = MeshTypeHTTP
		c.Target = url
	case "keyword":
		c.MeshType = MeshTypeHTTP
		c.Target = url
		c.Warnings = append(c.Warnings, "关键词校验无法从指标还原,请导入后补充「包含关键字」")
	case "json-query":
		c.MeshType = MeshTypeHTTP
		c.Target = url
		c.Warnings = append(c.Warnings, "JSON 断言无法从指标还原,已按普通 HTTP 监控导入")
	case "ping":
		c.MeshType = MeshTypePing
		c.Target = host
	case "port", "tcp":
		port, ok := portFromText(strings.TrimSpace(m.Port))
		if !ok {
			c.Reason = "该 TCP 端口监控没有可用端口号,无法导入"
			return c
		}
		c.MeshType, c.Target, c.Port = MeshTypeTCP, host, port
	case "push":
		c.MeshType = MeshTypePush
		c.Warnings = append(c.Warnings,
			"外部上报监控:导入后会生成新的上报地址,需把上报方指向 UptimeMesh")
	default:
		label := m.Type
		if label == "" {
			label = "未知"
		}
		c.Reason = "UptimeMesh 暂不支持 UptimeKuma 的 " + label + " 类型"
		return c
	}
	return finishCandidate(c)
}

// finishCandidate 做目标必填校验并补默认名称(各种类型的规则一致)。
func finishCandidate(c Candidate) Candidate {
	if c.MeshType == MeshTypePush {
		// push 没有目标,名称是它的身份,必须有。
		if strings.TrimSpace(c.Name) == "" {
			c.Reason = "该监控没有名称,无法导入"
		}
		return c
	}
	if c.Target == "" {
		if c.MeshType == MeshTypeHTTP {
			c.Reason = "该监控没有 URL,无法导入"
		} else {
			c.Reason = "该监控没有主机名,无法导入"
		}
		return c
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name = c.Target
	}
	return c
}

// portFromText 把端口文本解析为 1~65535 的整数;非法或缺失返回 ok=false。
func portFromText(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// hostFromURL 从 http(s) URL 中取主机名(不带端口),供 PING 兜底。
func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ---- 去重 ----

// TargetKey 是判重用的归一化键:类型 + 目标(http 忽略大小写与首尾空白,
// ping/tcp 忽略主机名大小写)。目标串由 Candidate.DedupeTarget() 或
// api 层的 dedupeTargetOf 提供(tcp 含端口,push 用监控名)。
func TargetKey(typ, target string) string {
	t := strings.ToLower(strings.TrimSpace(target))
	if typ == MeshTypeHTTP {
		t = strings.TrimRight(t, "/")
	}
	return strings.ToLower(strings.TrimSpace(typ)) + "|" + t
}
