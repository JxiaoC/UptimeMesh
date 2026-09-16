// Package client 实现 Agent 侧的 WebSocket 长连接(ADR-0001):
// hello 握手、心跳、断线指数退避重连、凭据持久化(票 03 完善)、执行并回传探测任务。
package agentclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	sharedprobe "github.com/uptimemesh/shared/probe"
	"github.com/uptimemesh/shared/protocol"
)

const (
	heartbeatEvery = 10 * time.Second
	minBackoff     = time.Second      // 断线后首次重连等待
	maxBackoff     = 30 * time.Second // 退避上限
)

// readIdle 读空闲上限:Dashboard 的 hub 每 hub.KeepaliveEvery(20s)下发一帧 ping,
// 所以只要链路是通的,本端不可能这么久收不到任何帧。超时即认为链路已死并返回,
// 由 Run 走正常的指数退避重连。
//
// 为什么必须有它:TCP 连接半开(对端已丢弃、本端不知道)时,本地写会被中转
// (frp)侧 ACK 掉、读又永久阻塞,进程会一直"以为自己在线",而 Dashboard 早已
// 判离线。此前只能靠人工重启 Agent 恢复(线上现象:节点日志停在"已接入",
// 节点页却显示离线)。
//
// 是变量而非常量:单测需要把它调小到毫秒级。
var readIdle = 60 * time.Second

// Version 是 Agent 自报版本,也是「一键升级」的比对基准。构建脚本用
// -ldflags -X github.com/uptimemesh/shared/agentclient.Version=... 打标
// (见 deploy/build-agent.sh、deploy/Dockerfile.dashboard);未打标时为源码默认值。
// 必须是变量:常量无法被 -X 覆盖。
var Version = "0.1.9"

// Options 是 Agent 运行参数。
type Options struct {
	ServerURL     string // ws://host:port/ws/agent
	EnrollmentKey string // 首次接入密钥
	Name          string
	CredentialDir string // 凭据持久化目录
}

// Client 一条 Agent↔Dashboard 逻辑连接(内部自动重连)。
type Client struct {
	opt Options

	cred   string // 已签发的专属凭据(批准后填充)
	credMu sync.RWMutex

	sendMu sync.Mutex
	ws     *websocket.Conn // 当前活动连接,写前需持 sendMu

	token tokenSource
	onLog func(format string, args ...any)
	probe ProbeFunc
}

// ProbeFunc 执行一次探测;票 08 前仅支持 http。
type ProbeFunc func(ctx context.Context, task *protocol.ProbeTaskPayload) (*protocol.ProbeResultPayload, error)

// token 单调递增,用于识别当前连接代次(重连后旧连接的回调作废)。
type tokenSource struct {
	mu  sync.Mutex
	cur int64
}

func (t *tokenSource) next() int64 { t.mu.Lock(); defer t.mu.Unlock(); t.cur++; return t.cur }
func (t *tokenSource) get() int64  { t.mu.Lock(); defer t.mu.Unlock(); return t.cur }

// New 创建客户端;探测执行委托 shared/probe(票 08 的 PING 执行器在其 init 注册)。
func New(opt Options, probe ProbeFunc) (*Client, error) {
	if opt.Name == "" || opt.ServerURL == "" {
		return nil, errors.New("server 与 name 为必填")
	}
	if opt.CredentialDir == "" {
		opt.CredentialDir = ".uptimemesh-agent"
	}
	c := &Client{opt: opt, probe: probe}
	if c.probe == nil {
		c.probe = func(ctx context.Context, task *protocol.ProbeTaskPayload) (*protocol.ProbeResultPayload, error) {
			return sharedprobe.Execute(ctx, task), nil
		}
	}
	if b, err := os.ReadFile(c.credPath()); err == nil {
		c.cred = strings.TrimSpace(string(b))
	}
	return c, nil
}

func (c *Client) credential() string {
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	return c.cred
}

func (c *Client) setCred(s string) { c.credMu.Lock(); c.cred = s; c.credMu.Unlock() }

func (c *Client) credPath() string { return filepath.Join(c.opt.CredentialDir, "credential") }

// Run 阻塞运行直到 ctx 取消:连接→收发→断线退避重连。
func (c *Client) Run(ctx context.Context, logf func(format string, args ...any)) error {
	c.onLog = logf
	bo := &backoffPolicy{cur: minBackoff}
	for {
		established, err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 会话真正建立过(收到 hello_ack)说明服务已恢复,退避归零;
		// 否则继续指数增长——避免一次抖动后永久卡在长退避(节点迟迟不回)。
		if established {
			bo.reset()
		}
		wait := bo.next()
		logf("连接中断(%v),%s 后重连", err, wait.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoffPolicy 断线重连退避:失败翻倍、封顶 maxBackoff;会话建立成功即归位 minBackoff。
type backoffPolicy struct {
	cur time.Duration
}

func (b *backoffPolicy) reset() { b.cur = minBackoff }

// next 返回本次重连等待时长(带抖动,避免多节点同时涌回),并推进到下一档。
func (b *backoffPolicy) next() time.Duration {
	if b.cur < minBackoff {
		b.cur = minBackoff
	}
	d := withJitter(b.cur)
	if b.cur < maxBackoff {
		b.cur *= 2
		if b.cur > maxBackoff {
			b.cur = maxBackoff
		}
	}
	return d
}

// withJitter 在 [0.8d, 1.2d] 内随机抖动,封顶 maxBackoff。
func withJitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	j := d + time.Duration((rand.Float64()*2-1)*delta)
	if j > maxBackoff {
		j = maxBackoff
	}
	return j
}

// session 建立一条连接并完成收发,返回时连接已断开。
// established 表示本次是否真正握手成功(收到 hello_ack),供 Run 决定是否重置退避。
func (c *Client) session(ctx context.Context) (established bool, err error) {
	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: false},
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, c.opt.ServerURL, nil)
	if err != nil {
		return false, err
	}
	myToken := c.token.next()
	c.setWS(conn)
	defer func() {
		if c.token.get() == myToken {
			c.setWS(nil)
		}
		_ = conn.Close()
	}()

	hello := protocol.HelloPayload{
		EnrollmentKey: c.opt.EnrollmentKey,
		Credential:    c.credential(),
		AgentName:     c.opt.Name,
		AgentVersion:  Version,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Capabilities:  capabilities(),
	}
	// 本机网络族可用性随 hello 上报一次:节点页据此解释"这条 IPv6 监控为什么在它身上
	// 测不出来"。之后每次心跳都重新探测一遍(网卡/路由变化不必等重连)。
	hello.IPv4Available, hello.IPv6Available = ipFamilyPtrs()
	if c.credential() != "" {
		hello.EnrollmentKey = "" // 已批准节点只凭凭据重连
	}
	if err := c.writeFrame(ctx, myToken, protocol.FrameHello, hello); err != nil {
		return false, err
	}

	// 读循环
	hbTicker := time.NewTicker(heartbeatEvery)
	defer hbTicker.Stop()
	// ready 在收到 hello_ack 时关闭一次,标记握手成功(session 返回 established=true)。
	// readyCh 是循环内的通道视图:首次触发后置 nil 停用该 case;闭包只关 ready,不做重赋值,故无竞争。
	ready := make(chan struct{})
	var readyOnce sync.Once
	readyCh := ready
	readDone := make(chan error, 1)
	go func() {
		readDone <- c.readPump(ctx, myToken, func() { readyOnce.Do(func() { close(ready) }) })
	}()

	for {
		select {
		case <-ctx.Done():
			return established, ctx.Err()
		case err := <-readDone:
			return established, err
		case <-readyCh:
			established = true
			readyCh = nil
		case <-hbTicker.C:
			v4, v6 := ipFamilyPtrs()
			if err := c.writeFrame(ctx, myToken, protocol.FrameHeartbeat,
				protocol.HeartbeatPayload{
					Unix: time.Now().Unix(), IPv4Available: v4, IPv6Available: v6,
				}); err != nil {
				return established, err
			}
		}
	}
}

// readPump 收帧并处理:ack/credential/task/ping。onEstablished 在收到 hello_ack 时回调一次。
func (c *Client) readPump(ctx context.Context, myToken int64, onEstablished func()) error {
	for {
		conn := c.currentConn()
		if conn == nil {
			return errors.New("未连接")
		}
		// 每轮读之前重设空闲上限:每收到一帧(含 Dashboard 的周期 ping)就续期,
		// 所以它只用来发现"链路已死",不会限制正常流量。
		_ = conn.SetReadDeadline(time.Now().Add(readIdle))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.FrameHelloAck:
			var p protocol.HelloAckPayload
			_ = protocol.Decode(env.Payload, &p)
			if onEstablished != nil {
				onEstablished()
			}
			if c.onLog != nil {
				c.onLog("已接入 Dashboard,身份=%s 节点ID=%s", p.Status, p.AgentID)
			}
		case protocol.FrameCredential:
			var p protocol.CredentialPayload
			if protocol.Decode(env.Payload, &p) == nil && p.Credential != "" {
				if err := c.persistCredential(p.Credential); err != nil {
					return err
				}
				c.setCred(p.Credential)
				if c.onLog != nil {
					c.onLog("已持久化专属凭据,后续重连免审批")
				}
			}
		case protocol.FrameError:
			var p protocol.ErrorPayload
			_ = protocol.Decode(env.Payload, &p)
			return errors.New("Dashboard 拒绝: " + p.Message)
		case protocol.FramePing:
			var p protocol.PingPayload
			if protocol.Decode(env.Payload, &p) == nil {
				_ = c.writeFrame(ctx, myToken, protocol.FramePong, protocol.PongPayload{PingID: p.PingID})
			}
		case protocol.FrameProbeTask:
			var task protocol.ProbeTaskPayload
			if protocol.Decode(env.Payload, &task) == nil {
				go c.handleTask(ctx, myToken, &task)
			}
		case protocol.FrameProbeTest:
			var p protocol.ProbeTestPayload
			if protocol.Decode(env.Payload, &p) == nil {
				go c.handleProbeTest(ctx, myToken, &p)
			}
		case protocol.FrameUpgrade:
			var up protocol.UpgradePayload
			if protocol.Decode(env.Payload, &up) == nil {
				go c.handleUpgrade(ctx, myToken, &up)
			}
		}
	}
}

// handleTask 执行探测并回传结果(尽力而为:超时/失败也要回传,不能沉默)。
func (c *Client) handleTask(ctx context.Context, myToken int64, task *protocol.ProbeTaskPayload) {
	res, err := c.probe(ctx, task)
	if err != nil || res == nil {
		res = &protocol.ProbeResultPayload{
			OK: false, Error: errString(err),
			StartedAtUnix: time.Now().Unix(), FinishedAtUnix: time.Now().Unix(),
		}
	}
	// 身份字段由任务权威回填,探测实现不得遗漏(否则 Dashboard 无法归属结果)。
	res.RoundID = task.RoundID
	res.MonitorID = task.MonitorID
	env, merr := protocol.NewEnvelope(protocol.FrameProbeResult, res)
	if merr != nil {
		return
	}
	c.writeFrameEnv(ctx, myToken, env)
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// handleProbeTest 执行一次「测试」并回传结果与明细(弹窗里的测试按钮)。
//
// 与 handleTask 的区别:不写轮次/监控身份(测试不属于任何轮次、不入库),
// 只把 TestID 原样带回供 Dashboard 配对;明细(请求/响应)由 shared/probe 采集。
// 执行耗时可能到探测超时,故调用方是 go 出去的,不阻塞收帧循环。
func (c *Client) handleProbeTest(ctx context.Context, myToken int64, p *protocol.ProbeTestPayload) {
	res := sharedprobe.ExecuteTest(ctx, p.MonitorType, p.Check)
	if res == nil {
		res = &protocol.ProbeTestResultPayload{OK: false, Error: "测试执行失败"}
	}
	res.TestID = p.TestID
	env, err := protocol.NewEnvelope(protocol.FrameProbeTestResult, res)
	if err != nil {
		return
	}
	c.writeFrameEnv(ctx, myToken, env)
}

func (c *Client) persistCredential(cred string) error {
	if err := os.MkdirAll(c.opt.CredentialDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.credPath(), []byte(cred), 0o600)
}

// ---- 连接与写帧 ----

func (c *Client) setWS(conn *websocket.Conn) {
	c.sendMu.Lock()
	c.ws = conn
	c.sendMu.Unlock()
}

func (c *Client) currentConn() *websocket.Conn {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.ws
}

// writeFrame 在当前连接代次内写一帧;代次过期或无连接返回错误。
func (c *Client) writeFrameEnv(ctx context.Context, myToken int64, env protocol.Envelope) error {
	if c.token.get() != myToken {
		return errors.New("连接代次已过期")
	}
	data, err := protocol.Marshal(env)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.ws == nil {
		return errors.New("未连接")
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) writeFrame(ctx context.Context, myToken int64, typ string, payload any) error {
	env, err := protocol.NewEnvelope(typ, payload)
	if err != nil {
		return err
	}
	return c.writeFrameEnv(ctx, myToken, env)
}

// wsURLHost 校验 ServerURL 使用 ws/wss(供 main 早失败)。
func WsURLValid(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "ws" || u.Scheme == "wss") && u.Host != ""
}
