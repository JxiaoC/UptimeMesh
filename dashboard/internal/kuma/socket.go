package kuma

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	socketclient "github.com/zishang520/socket.io/clients/socket/v3"
	"github.com/zishang520/socket.io/v3/pkg/types"
)

// 账号密码(Socket.IO)导入:
//
// UptimeKuma 的管理接口是 Socket.IO 事件,不是 REST。API 密钥只能读 /metrics,
// 想拿"关键词/请求方法/期望状态码/检测间隔"这类完整配置,只能像它的 Web UI 一样
// 用账号密码登录(见 louislam/uptime-kuma 的 server/server.js):
//
//	socket.emit("login", {username, password, token?}, cb)   // token 为两步验证码
//	  → cb({ok:true, token}) | cb({tokenRequired:true}) | cb({ok:false, msg})
//	  → 服务端 afterLogin() 随后推送 monitorList(Monitor.toJSON() 全量字段)
//
// 凭据只在本次调用内使用,不落库;取回监控列表后立即断开连接。

// 账号密码导入的错误分类,API 层据此给出可操作提示。
var (
	// ErrLoginFailed 账号或密码错误。
	ErrLoginFailed = errors.New("账号或密码错误")
	// ErrTwoFARequired 账号开启了两步验证,需要动态验证码。
	ErrTwoFARequired = errors.New("该账号开启了两步验证")
	// ErrTwoFAInvalid 动态验证码不正确。
	ErrTwoFAInvalid = errors.New("两步验证码不正确")
	// ErrLoginTimeout 连接成功但对 login 事件没有响应(常见于去掉了该事件、
	// 改用 better-auth 会话的新版;也可能是反代把 socket 消息吞了)。
	ErrLoginTimeout = errors.New("等待 UptimeKuma 登录响应超时")
	// ErrNoMonitorList 登录成功但没有收到监控列表。
	ErrNoMonitorList = errors.New("登录成功但没有收到监控列表")
	// errUseBetterAuth 内部信号:服务端提示需要登录(loginRequired)却不认 login 事件,
	// 说明应当改走 better-auth 会话登录。不会返回给调用方。
	errUseBetterAuth = errors.New("改用 better-auth 会话登录")
)

// 各阶段超时:设置页是同步等待,不宜过长。
const (
	socketOverallTimeout = 40 * time.Second
	socketConnectTimeout = 10 * time.Second
	// socketLoginTimeout 等 login 事件的应答。正常是毫秒级;留 8s 既能容忍慢反代,
	// 又能在"服务端没有 login 事件"(新版)时较快切到 better-auth 会话登录。
	socketLoginTimeout = 8 * time.Second
)

// PasswordLogin 是一次性使用的 UptimeKuma 账号凭据(不持久化)。
type PasswordLogin struct {
	Username string
	Password string
	// TwoFACode 账号开启两步验证时必填(TOTP 6 位)。
	TwoFACode string
}

// FullTag 是 UptimeKuma 监控上的一个标签。
type FullTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// IDText 兼容 UptimeKuma 的 ID 形态:monitorList 的键是字符串,而值里的 id 字段
// 在 JSON 里是数字(例:{"100":{"id":100,…}});/metrics 的标签里又是字符串 "100"。
type IDText string

// UnmarshalJSON 同时接受数字、字符串与 null。
func (v *IDText) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	switch {
	case raw == "" || raw == "null":
		*v = ""
		return nil
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = IDText(s)
		return nil
	default:
		*v = IDText(raw)
		return nil
	}
}

// String 返回 ID 的文本形态。
func (v IDText) String() string { return string(v) }

// FullMonitor 是 UptimeKuma monitorList 里的一个监控(Monitor.toJSON())。
// 只声明导入用得到的字段,其余(密码类字段、不通用的探测参数)一律忽略。
type FullMonitor struct {
	ID   IDText `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	URL  string `json:"url"`
	// Method/Headers/Body 为 HTTP 请求配置。Headers 在 SQLite 后端是 JSON 文本,
	// 别的后端可能直接把该列存成 JSON 对象,故按原始 JSON 收下再解析。
	Method  string          `json:"method"`
	Headers json.RawMessage `json:"headers"`
	Body    string          `json:"body"`
	// Hostname/Port 供 ping/port 类监控使用。
	Hostname string `json:"hostname"`
	Port     any    `json:"port"`
	// Interval/Timeout 单位秒;Timeout 可能为 null(UptimeKuma 运行时按 interval*0.8 兜底)。
	Interval int `json:"interval"`
	Timeout  int `json:"timeout"`
	// Keyword/InvertKeyword 是关键词校验(InvertKeyword 表示"不得出现")。
	Keyword       string `json:"keyword"`
	InvertKeyword bool   `json:"invertKeyword"`
	// AcceptedStatusCodes 形如 ["200-299"] 或 ["200","201"]。
	AcceptedStatusCodes []string `json:"accepted_statuscodes"`
	// JsonPath/ExpectedValue 是 json-query 监控的断言(JSONata 表达式 + 期望值);
	// JsonPathOperator 为比较方式(==/!=/contains/>/>=/</<=)。
	JsonPath         string `json:"jsonPath"`
	ExpectedValue    string `json:"expectedValue"`
	JsonPathOperator string `json:"jsonPathOperator"`
	// IgnoreTLS 跳过证书校验;UpsideDown 反转状态判定。
	IgnoreTLS  bool `json:"ignoreTls"`
	UpsideDown bool `json:"upsideDown"`
	// Active 为 false 表示该监控在 UptimeKuma 里处于暂停。
	Active any `json:"active"`
	// Parent 是所属分组容器(type=group 的监控)的 ID;Kuma 的"分组"是一棵容器树,
	// 监控通过 parent 指向它,故这里要解析出来才能还原分组标签。
	Parent IDText `json:"parent"`
	// Tags 是监控标签(展示维度)。
	Tags []FullTag `json:"tags"`
	// Description 监控描述,导入后拼进告警无对应字段,仅用于提示。
	Description string `json:"description"`
}

// PortText 返回端口的文本形态(JSON 里可能是数字、字符串或 null)。
func (f FullMonitor) PortText() string {
	switch v := f.Port.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(int(v))
	case int:
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// Paused 表示该监控在 UptimeKuma 侧是暂停状态。
// Kuma 的 active 在不同后端/版本里可能是布尔、0/1 数字或 "0"/"1" 字符串。
func (f FullMonitor) Paused() bool {
	switch v := f.Active.(type) {
	case nil:
		return false
	case bool:
		return !v
	case float64:
		return v == 0
	case int:
		return v == 0
	case int64:
		return v == 0
	case string:
		return v == "0" || strings.EqualFold(v, "false")
	default:
		return false
	}
}

// FetchFullViaPassword 用账号密码取回 UptimeKuma 的完整监控配置。
//
// 登录方式按服务端实际能力自动选择:
//   - 绝大多数版本(1.x 以及 2.x 的正式发布,例:2.5.4)都保留 Socket.IO 的 login 事件,
//     且未登录时会先发一个 loginRequired —— 那只是"请登录"的提示,不是版本标志;
//   - 只有去掉了 login 事件、改用 better-auth 的新版,才会对 login 事件毫无响应;
//     此时改走 HTTP 登录 better-auth 拿会话 Cookie,再带 Cookie 连 Socket.IO。
//
// 连接握手偶尔会失败(反代冷启动等)。这类失败且端口确实开着时重试一次;
// 端口根本不通(地址/端口写错)不重试,让用户尽快看到"无法连接"。
func FetchFullViaPassword(ctx context.Context, baseURL string, login PasswordLogin, insecureTLS bool) ([]FullMonitor, error) {
	if strings.TrimSpace(login.Username) == "" || login.Password == "" {
		return nil, errors.New("账号密码方式须填写 UptimeKuma 用户名与密码")
	}
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	monitors, err := fetchFullByEventOrSession(ctx, base, login, insecureTLS)
	if err != nil && isHandshakeFlake(base, err) {
		monitors, err = fetchFullByEventOrSession(ctx, base, login, insecureTLS)
	}
	return monitors, err
}

// fetchFullByEventOrSession 选路并登录:
//   - 目标有 better-auth 登录接口(去掉了 login 事件的新版)⇒ 直接走会话登录;
//   - 否则按 login 事件登录(1.x 与 2.x 正式发布都支持,例:2.5.4);
//   - 若 login 事件在服务端明确要求登录的前提下一声不吭,再兜底切到会话登录。
func fetchFullByEventOrSession(ctx context.Context, base *BaseURL, login PasswordLogin, insecureTLS bool) ([]FullMonitor, error) {
	if BetterAuthAvailable(ctx, base, insecureTLS) {
		monitors, err := fetchFullViaSession(ctx, base, login, insecureTLS)
		if err == nil || !errors.Is(err, ErrNoBetterAuth) {
			return monitors, err
		}
		// 探测与实际不一致(例如 /api/auth/ok 通了但登录端点被反代拦掉):继续按事件登录。
	}
	monitors, err := fetchFullSession(ctx, base, login, insecureTLS, "")
	if !errors.Is(err, errUseBetterAuth) {
		return monitors, err
	}
	sessionMonitors, sessionErr := fetchFullViaSession(ctx, base, login, insecureTLS)
	if sessionErr == nil {
		return sessionMonitors, nil
	}
	if errors.Is(sessionErr, ErrNoBetterAuth) {
		// 两条路都不通:login 事件无人应答,也没有 better-auth 登录接口。
		return nil, fmt.Errorf("%w(且未找到账号密码登录接口 %s)",
			ErrLoginTimeout, base.RESTURL(betterAuthSignInPath))
	}
	return nil, sessionErr
}

// fetchFullViaSession 去掉了 login 事件的新版:先 HTTP 登录 better-auth 拿会话 Cookie,
// 再带 Cookie 连 Socket.IO。
func fetchFullViaSession(ctx context.Context, base *BaseURL, login PasswordLogin, insecureTLS bool) ([]FullMonitor, error) {
	cookie, err := LoginViaBetterAuth(ctx, base, login, insecureTLS)
	if err != nil {
		return nil, err
	}
	monitors, err := fetchFullSession(ctx, base, login, insecureTLS, cookie)
	if err != nil && isHandshakeFlake(base, err) {
		monitors, err = fetchFullSession(ctx, base, login, insecureTLS, cookie)
	}
	return monitors, err
}

// isHandshakeFlake 判断是否为"端口通但握手失败"的连接类抖动。认证类结论(密码错误、
// 需要验证码、登录无人应答、没有登录接口)都是服务端给出的确定答复,不重试。
func isHandshakeFlake(base *BaseURL, err error) bool {
	if errors.Is(err, ErrLoginFailed) || errors.Is(err, ErrTwoFARequired) ||
		errors.Is(err, ErrTwoFAInvalid) || errors.Is(err, errUseBetterAuth) ||
		errors.Is(err, ErrSessionRejected) || errors.Is(err, ErrNoMonitorList) ||
		errors.Is(err, ErrLoginTimeout) || errors.Is(err, ErrNoBetterAuth) {
		return false
	}
	return dialBase(base) == nil
}

// dialBase 探一次 TCP,返回 nil 表示端口可以连接。
func dialBase(base *BaseURL) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.Dial("tcp", base.Host)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

// fetchFullSession 建立一次 Socket.IO 会话,取回 monitorList。
//
// cookie 为空 = 1.x 事件登录(连接后 emit login);cookie 非空 = 2.x 会话登录
// (把 httpOnly 的会话 Cookie 放进握手头,服务端认会话后直接推送列表)。
func fetchFullSession(ctx context.Context, base *BaseURL, login PasswordLogin, insecureTLS bool, cookie string) ([]FullMonitor, error) {
	sessionMode := cookie != ""

	opts := socketclient.DefaultOptions()
	// 与官方 JS 客户端一致:polling 起手、可升级为 websocket;不启用 webtransport(需 HTTP/3)。
	opts.SetTransports(types.NewSet(socketclient.Polling, socketclient.WebSocket))
	// 一次性导入:不重连、不复用全局连接缓存(避免串用上一次的连接/会话),
	// 并关闭自动连接,等事件监听器都注册好之后再显式连接,避免漏掉 connect 事件。
	opts.SetReconnection(false)
	opts.SetForceNew(true)
	opts.SetAutoConnect(false)
	// 连接尝试的兜底超时:客户端把"连不上"统一报成 timeout,设短一点让失败更快露出来。
	opts.SetTimeout(socketConnectTimeout)
	opts.SetPath(base.SocketPath())
	if sessionMode {
		// 2.x 靠握手请求头里的会话 Cookie 认身份(polling 与 websocket 两条传输都会带上)。
		opts.SetExtraHeaders(http.Header{"Cookie": []string{cookie}})
	}
	if insecureTLS {
		opts.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	}

	sock, err := socketclient.Connect(base.Origin(), opts)
	if err != nil {
		return nil, fmt.Errorf("无法连接 UptimeKuma(%s): %w", base.Origin(), err)
	}
	defer sock.Close()

	listCh := make(chan []FullMonitor, 1)
	errCh := make(chan error, 1)
	var loggedIn atomic.Bool

	// monitorList 由服务端在登录成功后推送(afterLogin),必须在 emit 之前注册监听。
	_ = sock.On("monitorList", func(args ...any) {
		monitors, err := DecodeMonitorList(firstArg(args))
		if err != nil {
			reportOnce(errCh, err)
			return
		}
		select {
		case listCh <- monitors:
		default:
		}
	})
	_ = sock.On("connect_error", func(args ...any) {
		reportOnce(errCh, fmt.Errorf("无法连接 UptimeKuma: %v", firstArg(args)))
	})
	// 关闭了自动重连:连接一旦断开就不会回来了,直接失败,不干等整体超时。
	_ = sock.On("disconnect", func(args ...any) {
		reportOnce(errCh, fmt.Errorf("与 UptimeKuma 的连接已断开: %v", firstArg(args)))
	})
	if sessionMode {
		// 带会话连接时,服务端会回 session(随后推 monitorList);若仍要求登录,
		// 说明会话没被接受(密码/验证码问题或会话过期)。
		_ = sock.On("session", func(...any) { loggedIn.Store(true) })
		_ = sock.On("loginRequired", func(...any) { reportOnce(errCh, ErrSessionRejected) })
	} else {
		// loginRequired 只是"请登录"的提示(1.x 与 2.x 正式发布都会发),不是版本标志:
		// 记下它,只在 login 事件确实没人应答时才认为该走 better-auth 会话。
		var loginRequiredSeen atomic.Bool
		_ = sock.On("loginRequired", func(...any) { loginRequiredSeen.Store(true) })
		_ = sock.On("connect", func(...any) {
			go func() {
				err := performLogin(sock, login)
				if errors.Is(err, ErrLoginTimeout) && loginRequiredSeen.Load() {
					// 要求登录却不认 login 事件 → 改用 better-auth 会话。
					reportOnce(errCh, errUseBetterAuth)
					return
				}
				if err != nil {
					reportOnce(errCh, err)
					return
				}
				loggedIn.Store(true)
			}()
		})
	}
	sock.Connect()

	timer := time.NewTimer(socketOverallTimeout)
	defer timer.Stop()

	select {
	case monitors := <-listCh:
		return monitors, nil
	case err := <-errCh:
		// 监控列表与断线错误可能同时就绪(服务端推完列表就断开),列表优先。
		select {
		case monitors := <-listCh:
			return monitors, nil
		default:
		}
		return nil, connectHint(base, err)
	case <-timer.C:
		if loggedIn.Load() {
			return nil, ErrNoMonitorList
		}
		return nil, connectHint(base, fmt.Errorf("连接或登录 UptimeKuma 超时(%s)", base.Origin()))
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connectHint 连接类失败的补充诊断:Socket.IO 客户端会把"端口没人监听"这类错误
// 统一报成含糊的 timeout,这里再探一次端口,能对上就直说"无法连接"。
func connectHint(base *BaseURL, err error) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, dialErr := dialer.Dial("tcp", base.Host)
	if dialErr == nil {
		_ = conn.Close()
		return err
	}
	return fmt.Errorf("无法连接 UptimeKuma(%s): %v", base.Origin(), dialErr)
}

// performLogin 完成一次(必要时两次)login 事件交互;monitorList 由事件监听器接收。
func performLogin(sock *socketclient.Socket, login PasswordLogin) error {
	res, err := loginAttempt(sock, login, "")
	if err != nil {
		return err
	}
	if res.twoFARequired {
		if strings.TrimSpace(login.TwoFACode) == "" {
			return ErrTwoFARequired
		}
		res, err = loginAttempt(sock, login, login.TwoFACode)
		if err != nil {
			return err
		}
		if res.twoFARequired {
			return ErrTwoFAInvalid
		}
	}
	if !res.ok {
		msg := strings.TrimSpace(res.msg)
		if msg == "" {
			return ErrLoginFailed
		}
		return fmt.Errorf("%w(%s)", ErrLoginFailed, msg)
	}
	return nil
}

// loginAck 是 login 事件的回调结果。
type loginAck struct {
	ok            bool
	msg           string
	twoFARequired bool
}

// loginAttempt 发一次 login 并等待回调。
func loginAttempt(sock *socketclient.Socket, login PasswordLogin, twoFACode string) (loginAck, error) {
	payload := map[string]any{"username": login.Username, "password": login.Password}
	if twoFACode != "" {
		payload["token"] = twoFACode
	}
	ackCh := make(chan loginAck, 1)
	emitErr := make(chan error, 1)
	// ack 回调类型为 func([]any, error)(zishang520 的 socket.Ack 是它的别名)。
	if err := sock.Emit("login", payload, func(res []any, err error) {
		if err != nil {
			emitErr <- err
			return
		}
		select {
		case ackCh <- parseLoginAck(res):
		default:
		}
	}); err != nil {
		return loginAck{}, fmt.Errorf("发送登录请求失败: %w", err)
	}

	timer := time.NewTimer(socketLoginTimeout)
	defer timer.Stop()
	select {
	case ack := <-ackCh:
		return ack, nil
	case err := <-emitErr:
		return loginAck{}, err
	case <-timer.C:
		return loginAck{}, ErrLoginTimeout
	}
}

// parseLoginAck 解析 login 回调参数:UptimeKuma 回一个对象,
// 形如 {ok:true, token} / {tokenRequired:true} / {ok:false, msg}。
func parseLoginAck(res []any) loginAck {
	var out loginAck
	m := asMap(firstArg(res))
	if m == nil {
		return out
	}
	out.ok = asBool(m["ok"])
	out.twoFARequired = asBool(m["tokenRequired"])
	out.msg = asString(m["msg"])
	return out
}

// DecodeMonitorList 把 monitorList 事件负载(以监控 ID 为键的对象)解成切片。
func DecodeMonitorList(v any) ([]FullMonitor, error) {
	if v == nil {
		return nil, ErrNoMonitorList
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("解析监控列表失败: %w", err)
	}
	byID := map[string]FullMonitor{}
	if err := json.Unmarshal(raw, &byID); err != nil {
		return nil, fmt.Errorf("解析监控列表失败: %w", err)
	}
	out := make([]FullMonitor, 0, len(byID))
	for id, m := range byID {
		// 键就是监控 ID(值为 {"<id>": {…}}),值里的 id 缺失时以键为准。
		if m.ID == "" {
			m.ID = IDText(id)
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return lessMonitor(out[i].ID.String(), out[j].ID.String())
	})
	if len(out) == 0 {
		return nil, ErrNoMonitorList
	}
	return out, nil
}

// headersOf 解析 UptimeKuma 的 headers 字段:可能是 JSON 文本(字符串),
// 也可能已经是 JSON 对象;返回 ok=false 表示内容无法解析。
func headersOf(raw json.RawMessage) (map[string]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, false
		}
		trimmed = bytes.TrimSpace([]byte(text))
		if len(trimmed) == 0 {
			return nil, true
		}
	}
	out := map[string]string{}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, false
	}
	return out, true
}

// MapFull 把单个监控的完整配置映射为导入候选(不带所属分组;批量映射请用 MapFullAll)。
func MapFull(f FullMonitor) Candidate {
	return mapFull(f, "")
}

// IsGroupContainer 判断该监控是否是 UptimeKuma 的"分组"容器(type=group)。
// 它本身不是被探测的目标,只是给子监控分类用。
func IsGroupContainer(f FullMonitor) bool {
	return strings.EqualFold(strings.TrimSpace(f.Type), "group")
}

// GroupSeparator 是嵌套分组的层级分隔符,与 UptimeKuma 面包屑(pathName)一致。
const GroupSeparator = " / "

// CountGroupContainers 统计监控列表里有多少个「分组」容器。
// 这些容器会被 MapFullAll 剔除、只把名字贡献给子监控的分组标签,
// 预览页需要单独说明它们,避免"Kuma 有 N 个、这里只列出 N-M 个"的疑惑。
func CountGroupContainers(monitors []FullMonitor) int {
	n := 0
	for _, m := range monitors {
		if IsGroupContainer(m) {
			n++
		}
	}
	return n
}

// MapFullAll 批量映射:先建立分组容器索引,再逐个映射。
//   - type=group 的容器本身**不导入**(它不是探测目标);
//   - 其余监控按 parent 链还原出分组标签(嵌套分组拼成"父 / 子"),供导入时作为分组落库,
//     从而与 UptimeMesh 的「分组」对应起来。
//
// 返回的候选已过滤掉分组容器,顺序与原列表一致。
func MapFullAll(monitors []FullMonitor) []Candidate {
	byID := make(map[string]FullMonitor, len(monitors))
	for _, m := range monitors {
		if id := m.ID.String(); id != "" {
			byID[id] = m
		}
	}
	out := make([]Candidate, 0, len(monitors))
	for _, m := range monitors {
		if IsGroupContainer(m) {
			continue
		}
		out = append(out, mapFull(m, groupLabelOf(byID, m)))
	}
	return out
}

// groupLabelOf 沿 parent 链把监控所属的分组容器名拼成分组标签。
// 只有 type=group 的容器才算分组;父链上遇到不存在的 ID(例如容器已被删除)、
// 指向普通监控、或自身没有名字时即停止(按未分组处理);环路用 seen 兜底。
func groupLabelOf(byID map[string]FullMonitor, f FullMonitor) string {
	var names []string
	seen := map[string]bool{}
	for parent := f.Parent.String(); parent != "" && !seen[parent]; {
		seen[parent] = true
		container, ok := byID[parent]
		if !ok || !IsGroupContainer(container) {
			break
		}
		if name := strings.TrimSpace(container.Name); name != "" {
			names = append(names, name)
		}
		parent = container.Parent.String()
	}
	// 自底向上收集,反转成"父 / 子"的展示顺序。
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	return strings.Join(names, GroupSeparator)
}

// mapFull 单条映射;group 为解析好的分组标签(来自 Kuma 的分组容器)。
func mapFull(f FullMonitor, group string) Candidate {
	c := Candidate{
		Monitor: Monitor{
			ID: f.ID.String(), Name: strings.TrimSpace(f.Name), Type: f.Type,
			URL: strings.TrimSpace(f.URL), Hostname: strings.TrimSpace(f.Hostname),
			Port: f.PortText(), Status: statusUnknown, Tags: map[string]string{},
		},
		Group:            group,
		Method:           strings.ToUpper(strings.TrimSpace(f.Method)),
		AllowInsecureTLS: f.IgnoreTLS,
		// UptimeKuma 的 Upside Down Mode 就是 UptimeMesh 的反转模式,直接映射。
		InvertMode: f.UpsideDown,
		Period:     f.Interval,
		Timeout:    f.Timeout,
		// Kuma 侧暂停的监控导入后同样保持暂停(见 api.kumaMonitor)。
		Paused: f.Paused(),
	}
	for _, t := range f.Tags {
		if name := strings.TrimSpace(t.Name); name != "" {
			c.Tags[name] = t.Value
		}
	}

	if specs := normalizeStatusSpecs(f.AcceptedStatusCodes); len(specs) > 0 {
		c.ExpectStatusSpecs = specs
	}

	typ := strings.ToLower(strings.TrimSpace(f.Type))
	url := strings.TrimSpace(f.URL)
	host := strings.TrimSpace(f.Hostname)
	if host == "" {
		host = hostFromURL(url)
	}
	switch typ {
	case "http":
		c.MeshType, c.Target = MeshTypeHTTP, url
	case "keyword":
		c.MeshType, c.Target = MeshTypeHTTP, url
		// 关键词只在 Kuma 的 keyword 类型上生效:其它类型(如 http)虽然数据库里
		// 可能残留 keyword 字段,但 Kuma 的检测逻辑根本不看它,照搬会凭空多出一条
		// 关键词校验(真实实例里就有 http 监控带着陈旧的关键词配置)。
		if kw := strings.TrimSpace(f.Keyword); kw != "" {
			if f.InvertKeyword {
				c.NotContains = []string{kw}
			} else {
				c.Contains = []string{kw}
			}
		}
	case "json-query":
		c.MeshType, c.Target = MeshTypeHTTP, url
		// 同理:JSON 断言只在 json-query 类型上生效。Kuma 的 jsonPath 默认是 "$",
		// 若对普通 http 监控照搬,会变成"响应体必须等于空串"而永远判失败。
		if path := strings.TrimSpace(f.JsonPath); path != "" {
			c.JSONPath = path
			c.JSONPathOperator = strings.TrimSpace(f.JsonPathOperator)
			c.ExpectedValue = f.ExpectedValue
		}
	case "ping":
		c.MeshType, c.Target = MeshTypePing, host
		c.Method, c.Headers, c.Body = "", nil, ""
		c.ExpectStatusSpecs = nil
		c.Contains, c.NotContains = nil, nil
	case "port", "tcp":
		port, ok := portFromText(f.PortText())
		if !ok {
			c.Reason = "该 TCP 端口监控没有可用端口号,无法导入"
			return c
		}
		c.MeshType, c.Target, c.Port = MeshTypeTCP, host, port
		// 端口监控没有 HTTP 语义,清掉从字段默认值带过来的东西。
		c.Method, c.Headers, c.Body = "", nil, ""
		c.ExpectStatusSpecs = nil
		c.Contains, c.NotContains = nil, nil
	case "push":
		c.MeshType = MeshTypePush
		// push 没有探测目标:导入后由 UptimeMesh 生成新的上报地址。
		c.Method, c.Headers, c.Body = "", nil, ""
		c.ExpectStatusSpecs = nil
		c.Contains, c.NotContains = nil, nil
		c.Warnings = append(c.Warnings,
			"外部上报监控:导入后会生成新的上报地址,需把上报方指向 UptimeMesh")
	default:
		label := f.Type
		if label == "" {
			label = "未知"
		}
		c.Reason = "UptimeMesh 暂不支持 UptimeKuma 的 " + label + " 类型"
		return c
	}

	if c.MeshType == MeshTypePush {
		if c.Name == "" {
			c.Reason = "该监控没有名称,无法导入"
			return c
		}
	} else if c.Target == "" {
		if c.MeshType == MeshTypeHTTP {
			c.Reason = "该监控没有 URL,无法导入"
		} else {
			c.Reason = "该监控没有主机名,无法导入"
		}
		return c
	}
	if c.Name == "" {
		c.Name = c.Target
	}
	// HTTP 请求头:SQLite 后端存 JSON 文本,其它后端可能是 JSON 对象;
	// 解析失败时只丢请求头并提示,不影响其余字段导入。
	if c.MeshType == MeshTypeHTTP && len(f.Headers) > 0 {
		headers, ok := headersOf(f.Headers)
		if !ok {
			c.Warnings = append(c.Warnings, "请求头无法解析,未导入")
		} else if len(headers) > 0 {
			c.Headers = headers
		}
	}
	if c.MeshType == MeshTypeHTTP {
		c.Body = f.Body
	}
	// 周期/超时越界时,入库前会回落到导入参数(见 api.kumaMonitor),这里先讲清楚,
	// 避免"悄悄改了配置"的观感。UptimeKuma 允许 86400s 级别的间隔;0 表示字段缺失,不提示。
	if f.Interval > 0 && (f.Interval < meshMinPeriod || f.Interval > meshMaxPeriod) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"UptimeKuma 的检测间隔 %ds 超出 UptimeMesh 的 %d~%ds,导入时将改用导入参数",
			f.Interval, meshMinPeriod, meshMaxPeriod))
	}
	if f.Timeout > meshMaxPeriod {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"UptimeKuma 的超时 %ds 超出 UptimeMesh 的 %ds 上限,导入时将改用导入参数",
			f.Timeout, meshMaxPeriod))
	}
	if f.Paused() {
		c.Warnings = append(c.Warnings, "该监控在 UptimeKuma 里处于暂停状态")
	}
	return c
}

// normalizeStatusSpecs 把 UptimeKuma 的 accepted_statuscodes 归一化为 UptimeMesh 的
// 编辑态 spec:区间写法 "200-299" 会被 checkconfig 接受(内部归一为 200~299),
// 非法项直接丢弃(避免整条监控因一个坏状态码而失败)。
func normalizeStatusSpecs(codes []string) []string {
	out := make([]string, 0, len(codes))
	seen := map[string]bool{}
	for _, raw := range codes {
		s := strings.TrimSpace(raw)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// statusUnknown 与 Monitor.Status 的语义一致:NaN 表示指标里没有状态。
var statusUnknown = math.NaN()

// ---- 事件负载取值辅助(JSON 解析后可能是多种具体类型) ----

func firstArg(args []any) any {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out
	default:
		return nil
	}
}

func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1"
	case float64:
		return b != 0
	case int:
		return b != 0
	default:
		return false
	}
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// reportOnce 只投递第一个错误,避免超时/断线等多个事件重复上报。
func reportOnce(ch chan error, err error) {
	select {
	case ch <- err:
	default:
	}
}
