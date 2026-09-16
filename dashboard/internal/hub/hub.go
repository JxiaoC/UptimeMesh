// Package hub 维护节点(Agent)WebSocket 长连接注册表(ADR-0001)。
// 鉴权与收帧处理见 session.go;向节点下发帧见 SendToAgent/Ping。
package hub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/protocol"
)

// OfflineAfter 心跳 10s×丢失 3 次 ⇒ 30s 判离线(spec)。
const OfflineAfter = 30 * time.Second

// KeepaliveEvery 向节点周期下发 ping 帧的间隔。它与 Agent 侧的读空闲上限(readIdle)
// 是成对的:Agent 靠「长时间一帧都收不到」判定链路已死(半开连接)并重连,
// 所以必须显著小于 readIdle(shared/agentclient),否则 Agent 会误判掉线。
const KeepaliveEvery = 20 * time.Second

// ErrSendBufferFull 节点消费过慢,写队列已满。
var ErrSendBufferFull = errors.New("节点发送队列已满")

// ErrAgentOffline 节点当前没有活动连接。
var ErrAgentOffline = errors.New("节点不在线")

// ErrTestTimeout 节点没有在超时时间内回测试结果(版本过旧不认识该帧、或链路卡住)。
var ErrTestTimeout = errors.New("节点未返回测试结果")

const sendBufferSize = 64

// WsConn 抽象 websocket 连接,便于测试注入。
type WsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetWriteDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// Conn 是一条已鉴权的节点连接。写只经由 send 通道,由单个 writer 串行执行。
type Conn struct {
	AgentID store.ID
	// name 是节点的展示名,与 LastSeen 一样受 mu 保护:后台改名会就地更新在线连接
	// 里缓存的名字(Hub.RenameConn),而上下线广播要读它(见 ConnName)。
	name     string
	Status   string // pending | approved
	LastSeen time.Time

	ws   WsConn
	hub  *Hub
	send chan protocol.Envelope
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	pongs map[string]chan struct{}
	// ipv4Available / ipv6Available 是节点最近一次上报的本机网络族可用性(三态:
	// nil = 未上报),与 LastSeen 同受 mu 保护。心跳每次都会带,只在取值变化时才落库
	// (见 Conn.refreshIPFamilies)。
	ipv4Available *bool
	ipv6Available *bool
	// tests 是"测试"请求的应答等待表:test_id -> 结果通道。与 pongs 同构
	// (都是请求-应答),但结果有内容,所以通道带类型;通道容量 1,
	// 投递方永不阻塞,等待方超时后自己收摊。
	tests map[string]chan *protocol.ProbeTestResultPayload
}

func newConn(ws WsConn, h *Hub, id store.ID, name, status string) *Conn {
	return &Conn{
		AgentID:  id,
		name:     name,
		Status:   status,
		LastSeen: time.Now(),
		ws:       ws,
		hub:      h,
		send:     make(chan protocol.Envelope, sendBufferSize),
		done:     make(chan struct{}),
		pongs:    make(map[string]chan struct{}),
		tests:    make(map[string]chan *protocol.ProbeTestResultPayload),
	}
}

func (c *Conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// Hub 以节点 ID 为键的连接注册表。
type Hub struct {
	mu    sync.RWMutex
	conns map[store.ID]*Conn

	// OnProbeResult 由调度器在票 05 接入;此前仅记日志。
	OnProbeResult func(ctx context.Context, agentID store.ID, p protocol.ProbeResultPayload)

	// OnAgentChange 节点连接建立/断开时回调(票 10 浏览器广播)。
	// 在锁外调用,online=false 表示该 agentID 此刻已无活动连接。
	OnAgentChange func(agentID store.ID, name string, online bool)

	// OnAgentConnected 节点(重新)建立连接后回调(session 在发完 hello_ack、
	// 启动写循环之后调用)。与 OnAgentChange 的分工:后者只做浏览器广播,
	// 前者给需要"趁节点刚回来补发在途任务"的调度器用(见 scheduler.AgentOnline)。
	// 在锁外调用,实现方会做一次 SQLite 查询,不应阻塞太久。
	OnAgentConnected func(agentID store.ID, status string)

	// OnUpgradeResult 节点回传「一键升级」结果时回调(成败都回调,仅用于日志)。
	// 在连接读循环里调用,实现方不应阻塞。
	OnUpgradeResult func(ctx context.Context, agentID store.ID, p protocol.UpgradeResultPayload)
}

func New() *Hub {
	return &Hub{conns: make(map[store.ID]*Conn)}
}

func (h *Hub) register(c *Conn) {
	h.mu.Lock()
	if old, ok := h.conns[c.AgentID]; ok && old != c {
		// 同一节点重复连接:旧连接让位(票 03 的“连接安全替换”)。
		old.close()
	}
	h.conns[c.AgentID] = c
	cb := h.OnAgentChange
	h.mu.Unlock()
	if cb != nil {
		cb(c.AgentID, c.ConnName(), true)
	}
}

// ConnName 返回连接当前缓存的展示名。后台改名会更新它(见 RenameConn),
// 故读要走这里,不能直接取字段。
func (c *Conn) ConnName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name
}

// RenameConn 后台改名后就地刷新在线连接里缓存的名字。
//
// 不刷新的话,该连接之后参与上下线广播时带的还是旧名 —— 前端收到事件会重拉列表、
// 界面上看不出问题,但事件载荷本身是过期值,谁按载荷渲染就会显示改名前的老名字,
// 一直持续到节点重连。节点不在线时是空操作(下次连上会从库里取到新名)。
func (h *Hub) RenameConn(agentID store.ID, name string) {
	c, ok := h.Get(agentID)
	if !ok {
		return
	}
	c.mu.Lock()
	c.name = name
	c.mu.Unlock()
}

func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	removed := false
	if cur, ok := h.conns[c.AgentID]; ok && cur == c {
		delete(h.conns, c.AgentID)
		removed = true
	}
	cb := h.OnAgentChange
	h.mu.Unlock()
	c.close()
	if removed && cb != nil {
		cb(c.AgentID, c.ConnName(), false)
	}
}

// Get 返回节点的当前连接(未连接则 ok=false)。
func (h *Hub) Get(agentID store.ID) (*Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[agentID]
	return c, ok
}

// IsOnline 在线判定:存在活动连接且最近心跳未超过 OfflineAfter(10s×3)。
// 半开连接(TCP 未断但心跳停)也在此被判离线,即 spec 的“心跳丢失判离线”。
func (h *Hub) IsOnline(agentID store.ID) bool {
	c, ok := h.Get(agentID)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.LastSeen) < OfflineAfter
}

// ForceDisconnect 吊销/拒绝后立即掐断该节点连接,使其不能赖在已批准的会话里。
func (h *Hub) ForceDisconnect(agentID store.ID) {
	if c, ok := h.Get(agentID); ok {
		c.close()
	}
}

// OnlineAgentIDs 返回当前在线节点集合(含 pending,调用方按需过滤)。
func (h *Hub) OnlineAgentIDs() []store.ID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]store.ID, 0, len(h.conns))
	for id := range h.conns {
		ids = append(ids, id)
	}
	return ids
}

// snapshot 复制当前全部连接(含 pending),供周期任务在锁外逐条发送。
func (h *Hub) snapshot() []*Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	return conns
}

// StartKeepalive 周期向每条已注册连接下发一帧 ping(不等 pong)。
//
// 为什么需要:空闲时服务端一帧都不发给 Agent,于是"链路已死但进程不知道"这种
// 半开连接在 Agent 看来与正常连接完全一样 —— 本地写有中转(frp)侧 ACK,读又永久
// 阻塞,只能靠人工重启恢复;而 Dashboard 侧 30s 收不到心跳就判离线并掐断连接,
// 两边状态就此长期背离(节点页显示离线、节点日志显示已接入)。有了这帧 ping,
// Agent 侧就能用读空闲上限把"死链路"变成一次断线重连(见 shared/agentclient)。
//
// pong 回包由 readLoop 的 deliverPong 丢弃(没有等待方),不占任何连接状态。
func (h *Hub) StartKeepalive(ctx context.Context) {
	h.keepalive(ctx, KeepaliveEvery)
}

// keepalive 是 StartKeepalive 的实现,间隔可注入以便单测用毫秒级节拍。
func (h *Hub) keepalive(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, c := range h.snapshot() {
					env, err := protocol.NewEnvelope(protocol.FramePing,
						protocol.PingPayload{PingID: store.NewID().Hex()})
					if err != nil {
						continue
					}
					// 队列满/连接已关:丢弃即可,下个周期还会再来一次。
					_ = c.Send(env)
				}
			}
		}
	}()
}

// SendToAgent 向指定在线节点下发帧;离线或队列满返回错误。
func (h *Hub) SendToAgent(agentID store.ID, env protocol.Envelope) error {
	c, ok := h.Get(agentID)
	if !ok {
		return ErrAgentOffline
	}
	return c.Send(env)
}

// Send 非阻塞投递一帧。
func (c *Conn) Send(env protocol.Envelope) error {
	select {
	case <-c.done:
		return ErrAgentOffline
	default:
	}
	select {
	case c.send <- env:
		return nil
	default:
		return ErrSendBufferFull
	}
}

// deliverPong 由收帧循环在收到 pong 时调用。
func (c *Conn) deliverPong(pingID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.pongs[pingID]; ok {
		close(ch)
		delete(c.pongs, pingID)
	}
}

// deliverTestResult 由收帧循环在收到 probe_test_result 时调用。通道容量 1 且只在
// 等待方仍挂着时存在,故这里用非阻塞投递:结果迟到(等待方已超时收摊)就丢弃。
func (c *Conn) deliverTestResult(res *protocol.ProbeTestResultPayload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.tests[res.TestID]; ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// RunProbeTest 向节点下发一次 probe_test 并等待结果(监控弹窗的"测试"按钮)。
// 与 Ping 同一套请求-应答机制;区别是等待的是带内容的测试结果。
func (h *Hub) RunProbeTest(ctx context.Context, agentID store.ID, payload protocol.ProbeTestPayload,
	timeout time.Duration) (*protocol.ProbeTestResultPayload, error) {
	c, ok := h.Get(agentID)
	if !ok {
		return nil, ErrAgentOffline
	}
	env, err := protocol.NewEnvelope(protocol.FrameProbeTest, payload)
	if err != nil {
		return nil, err
	}
	ch := make(chan *protocol.ProbeTestResultPayload, 1)
	c.mu.Lock()
	c.tests[payload.TestID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.tests, payload.TestID)
		c.mu.Unlock()
	}()
	if err = c.Send(env); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-c.done:
		return nil, ErrAgentOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, ErrTestTimeout
	}
}

// Ping 向节点发送探活帧并等待 pong(轮次缺样决策表使用,见票 06)。
// 票 01 仅提供能力,尚无调用方。
func (h *Hub) Ping(ctx context.Context, agentID store.ID, timeout time.Duration) bool {
	c, ok := h.Get(agentID)
	if !ok {
		return false
	}
	pingID := store.NewID().Hex()
	env, err := protocol.NewEnvelope(protocol.FramePing, protocol.PingPayload{PingID: pingID})
	if err != nil {
		return false
	}
	ch := make(chan struct{})
	c.mu.Lock()
	c.pongs[pingID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pongs, pingID)
		c.mu.Unlock()
	}()
	if err = c.Send(env); err != nil {
		return false
	}
	select {
	case <-ch:
		return true
	case <-c.done:
		return false
	case <-ctx.Done():
		return false
	case <-time.After(timeout):
		return false
	}
}

// writePump 是 Conn 唯一的写者。
func (c *Conn) writePump() {
	for {
		select {
		case <-c.done:
			return
		case env := <-c.send:
			data, err := protocol.Marshal(env)
			if err != nil {
				continue
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(1, data); err != nil {
				return // 连接已坏,读循环负责注销
			}
		}
	}
}
