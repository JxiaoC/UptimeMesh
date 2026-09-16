package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/protocol"
)

// ErrAuthFailed 接入密钥与凭据均无效。
var ErrAuthFailed = errors.New("接入凭据无效")

// Authenticator 是 session 对存储层的最小依赖,便于单测注入。
type Authenticator interface {
	CheckEnrollmentKey(ctx context.Context, key string) (bool, error)
	FindAgentByCredentialHash(ctx context.Context, hash string) (*store.Agent, error)
	UpsertPendingAgent(ctx context.Context, a *store.Agent) (string, error)
	FindAgentByID(ctx context.Context, id store.ID) (*store.Agent, error)
	ClearCredPlain(ctx context.Context, id store.ID) error
	TouchAgentLastSeen(ctx context.Context, id store.ID, at time.Time) error
	// RefreshAgentHello 用重连 hello 上报的版本、系统信息、能力列表与网络族可用性
	// 刷新记录(一键升级后版本变化必须落库,否则节点页会一直提示可升级)。
	RefreshAgentHello(ctx context.Context, id store.ID, version, goos, arch string,
		capabilities []string, ipv4, ipv6 *bool) error
	// SetAgentIPAvailability 记录心跳上报的网络族可用性(取值变化时才调用)。
	SetAgentIPAvailability(ctx context.Context, id store.ID, ipv4, ipv6 *bool) error
}

// ServeWS 接管一条已升级的节点连接:等待 hello、鉴权、注册、收发循环。
// sourceIP 由 HTTP 层提供(节点身份去重键的一部分)。
func (h *Hub) ServeWS(ctx context.Context, ws WsConn, auth Authenticator, sourceIP string) {
	hello, err := readHello(ws)
	if err != nil {
		sendError(ws, protocol.ErrProtocol, "握手失败")
		_ = ws.Close()
		return
	}
	agent, status, err := authenticate(ctx, auth, hello, sourceIP)
	if err != nil {
		sendError(ws, protocol.ErrAuthFailed, "接入凭据无效")
		_ = ws.Close()
		return
	}
	c := newConn(ws, h, agent.ID, agent.Name, status)
	// 让心跳的"取值是否变化"从当前库内值开始比较,避免连上后第一帧心跳白写一次。
	c.ipv4Available, c.ipv6Available = agent.IPv4Available, agent.IPv6Available
	h.register(c)
	defer h.unregister(c)

	ack, _ := protocol.NewEnvelope(protocol.FrameHelloAck, protocol.HelloAckPayload{
		Status: status, AgentID: agent.ID.Hex(),
	})
	if err := c.Send(ack); err != nil {
		return
	}
	// 批准时欠发的凭据(离线批准/凭据送达前掉线)在连接建立后立即补发。
	if status == store.AgentApproved {
		h.deliverPendingCredential(ctx, auth, c)
	}
	go c.writePump()
	// 连接就绪(hello_ack 已入队、写循环已起)之后才通知外部:补发在这里做,
	// 顺序上任务帧必须排在 hello_ack 之后,否则节点会先收到探测任务再收到握手应答。
	if cb := h.OnAgentConnected; cb != nil {
		cb(agent.ID, status)
	}
	c.readLoop(ctx, auth)
}

// deliverPendingCredential 向 approved 节点补发暂存凭据;每次 approved 连接都补发,
// 直到节点用该凭据认证成功(ClearCredPlain)为止。覆盖“送达前掉线”场景。
func (h *Hub) deliverPendingCredential(ctx context.Context, auth Authenticator, c *Conn) {
	ag, err := auth.FindAgentByID(ctx, c.AgentID)
	if err != nil || ag.CredPlain == "" {
		return
	}
	env, err := protocol.NewEnvelope(protocol.FrameCredential, protocol.CredentialPayload{
		AgentID: c.AgentID.Hex(), Credential: ag.CredPlain,
	})
	if err == nil {
		_ = c.Send(env)
	}
}

func readHello(ws WsConn) (*protocol.HelloPayload, error) {
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, data, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if msgType != 1 {
		return nil, errors.New("首帧必须为文本")
	}
	var env protocol.Envelope
	if err = json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Type != protocol.FrameHello {
		return nil, errors.New("首帧必须为 hello")
	}
	var hello protocol.HelloPayload
	if err = protocol.Decode(env.Payload, &hello); err != nil {
		return nil, err
	}
	return &hello, nil
}

func authenticate(ctx context.Context, auth Authenticator, hello *protocol.HelloPayload, sourceIP string) (*store.Agent, string, error) {
	// 已批准节点凭据重连:认证成功即证明其持有凭据,清除待发明文。
	if hello.Credential != "" {
		if a, err := auth.FindAgentByCredentialHash(ctx, store.HashSecret(hello.Credential)); err == nil {
			_ = auth.ClearCredPlain(ctx, a.ID)
			refreshAgentHello(ctx, auth, a, hello)
			return a, store.AgentApproved, nil
		}
	}
	// 接入密钥路径:pending 新建/刷新,或离线批准后的节点重连(取回凭据)。
	if hello.EnrollmentKey != "" && hello.AgentName != "" {
		ok, err := auth.CheckEnrollmentKey(ctx, hello.EnrollmentKey)
		if err != nil {
			return nil, "", err
		}
		if ok {
			a := &store.Agent{
				Name: hello.AgentName, Version: hello.AgentVersion,
				OS: hello.OS, Arch: hello.Arch,
				Capabilities: hello.Capabilities,
				// 网络族可用性(三态)随首连一起落库,节点页才能解释"这条 IPv6 监控
				// 为什么在它身上测不出来"。
				IPv4Available: hello.IPv4Available, IPv6Available: hello.IPv6Available,
				SourceIP: sourceIP, Status: store.AgentPending,
			}
			status, err := auth.UpsertPendingAgent(ctx, a)
			if err != nil {
				if errors.Is(err, store.ErrAgentRevoked) {
					return nil, "", err // 已吊销:拒绝
				}
				return nil, "", err
			}
			return a, status, nil
		}
	}
	return nil, "", ErrAuthFailed
}

// refreshAgentHello 把本次 hello 上报的版本/系统信息/能力/网络族刷进库:一键升级后节点以
// 凭据重连,只有落库才能让节点页看到新版本与新的能力。空值(未上报)不覆盖原值,
// 兼容旧版客户端(它们既不认识 upgrade 帧,也不上报能力与网络族)。
func refreshAgentHello(ctx context.Context, auth Authenticator, a *store.Agent, hello *protocol.HelloPayload) {
	version, goos, arch := prefer(hello.AgentVersion, a.Version), prefer(hello.OS, a.OS), prefer(hello.Arch, a.Arch)
	caps := hello.Capabilities
	if len(caps) == 0 {
		caps = a.Capabilities
	}
	// 网络族用三态:节点没上报(nil)时保留库里的旧值,不能把"未知"写成"不支持"。
	ipv4, ipv6 := hello.IPv4Available, hello.IPv6Available
	if ipv4 == nil {
		ipv4 = a.IPv4Available
	}
	if ipv6 == nil {
		ipv6 = a.IPv6Available
	}
	if version == a.Version && goos == a.OS && arch == a.Arch &&
		sameCapabilities(caps, a.Capabilities) &&
		sameBoolPtr(ipv4, a.IPv4Available) && sameBoolPtr(ipv6, a.IPv6Available) {
		return
	}
	if err := auth.RefreshAgentHello(ctx, a.ID, version, goos, arch, caps, ipv4, ipv6); err != nil {
		return
	}
	a.Version, a.OS, a.Arch, a.Capabilities = version, goos, arch, caps
	a.IPv4Available, a.IPv6Available = ipv4, ipv6
}

// sameBoolPtr 与 store 的同名辅助同语义(这里不导入 store 的未导出函数)。
func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func prefer(incoming, current string) string {
	if incoming != "" {
		return incoming
	}
	return current
}

// sameCapabilities 按集合语义比较(顺序无关):能力列表来自 JSON,顺序不稳定。
func sameCapabilities(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, c := range a {
		set[c] = true
	}
	for _, c := range b {
		if !set[c] {
			return false
		}
	}
	return true
}

// readLoop 处理心跳、探测结果回传与 pong。
func (c *Conn) readLoop(ctx context.Context, auth Authenticator) {
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.FrameHeartbeat:
			c.mu.Lock()
			c.LastSeen = time.Now()
			c.mu.Unlock()
			_ = auth.TouchAgentLastSeen(ctx, c.AgentID, time.Now())
			// 网络族可用性随心跳刷新(网卡/路由变化不必等重连);取值没变就不写库,
			// 否则每 10s 一次的写会变成 agents 表的写热点。
			var hb protocol.HeartbeatPayload
			if protocol.Decode(env.Payload, &hb) == nil {
				c.refreshIPFamilies(ctx, auth, hb.IPv4Available, hb.IPv6Available)
			}
		case protocol.FrameProbeResult:
			var p protocol.ProbeResultPayload
			if protocol.Decode(env.Payload, &p) == nil && c.hub.OnProbeResult != nil {
				c.hub.OnProbeResult(ctx, c.AgentID, p)
			}
		case protocol.FramePong:
			var p protocol.PongPayload
			if protocol.Decode(env.Payload, &p) == nil {
				c.deliverPong(p.PingID)
			}
		case protocol.FrameProbeTestResult:
			// 测试结果:交给正等着的那次请求(不入库、不建轮次)。
			var p protocol.ProbeTestResultPayload
			if protocol.Decode(env.Payload, &p) == nil {
				c.deliverTestResult(&p)
			}
		case protocol.FrameUpgradeResult:
			// 节点回传自升级结果:仅用于日志/排障——成功时节点随即重启并以新版本
			// 重连,版本变化由那次 hello 落库体现。
			var p protocol.UpgradeResultPayload
			if protocol.Decode(env.Payload, &p) == nil && c.hub.OnUpgradeResult != nil {
				c.hub.OnUpgradeResult(ctx, c.AgentID, p)
			}
		case protocol.FrameHello:
			// 重复 hello:忽略。
		}
	}
}

// refreshIPFamilies 记录心跳上报的本机网络族可用性:取值与上次相同则不落库
// (节点每 10s 上报一次,无脑写会把 agents 表变成写热点)。
// nil 表示该族本次未上报(老版本客户端),保留已知值而不是清成"未知"。
func (c *Conn) refreshIPFamilies(ctx context.Context, auth Authenticator, ipv4, ipv6 *bool) {
	c.mu.Lock()
	next4, next6 := ipv4, ipv6
	if next4 == nil {
		next4 = c.ipv4Available
	}
	if next6 == nil {
		next6 = c.ipv6Available
	}
	changed := !sameBoolPtr(next4, c.ipv4Available) || !sameBoolPtr(next6, c.ipv6Available)
	if changed {
		c.ipv4Available, c.ipv6Available = next4, next6
	}
	c.mu.Unlock()
	if !changed {
		return
	}
	// 落库失败不重试:下一帧心跳(10s 后)的取值与缓存一致,会认为"没变化"。
	// 与 cred_plain 补发等路径同款取舍——这里不值得为一次瞬时写失败引入重试状态。
	_ = auth.SetAgentIPAvailability(ctx, c.AgentID, next4, next6)
}

func sendError(ws WsConn, code, msg string) {
	env, _ := protocol.NewEnvelope(protocol.FrameError, protocol.ErrorPayload{Code: code, Message: msg})
	if data, err := protocol.Marshal(env); err == nil {
		_ = ws.WriteMessage(1, data)
	}
}

// NewCredential 生成节点专属凭据明文(哈希存库)。
func NewCredential() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "agt_" + hex.EncodeToString(b)
}
