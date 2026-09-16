// Package webhub 维护浏览器 WebSocket 连接并广播实时事件(票 10):
// 轮次定稿、监控状态翻转、节点上下线 → 推给所有已鉴权浏览器。
package webhub

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event 浏览器推送帧。
type Event struct {
	Type string `json:"type"` // round_finalized | monitor_flipped | agent_changed | hello
	Data any    `json:"data,omitempty"`
}

const (
	// browserReadWait 浏览器连接静默上限:这么久没收到任何帧(含 pong)就断开。
	browserReadWait = 90 * time.Second
	// browserPingEvery 服务端 ping 间隔。浏览器不会主动发任何帧,只有收到 ping 才会
	// 自动回 pong —— 不发 ping 的话每条连接都会在 browserReadWait 后被自己的读截止
	// 掐断(每 90 秒一次),断开的窗口里 agent_changed 等事件直接丢失,
	// 节点页因此会长期停在过期的在线状态上。
	browserPingEvery = 30 * time.Second
	// browserWriteWait 单帧写超时。
	browserWriteWait = 10 * time.Second
)

// Conn 是一条浏览器推送连接。由 Hub 持有;api 层在应答浏览器的请求帧时直接往
// **这一条**连接上回帧 —— Hub 的广播是发给所有连接的,请求-响应必须点对点。
type Conn struct {
	ws   *websocket.Conn
	send chan Event
	done chan struct{}
	once sync.Once
	// throttle 是连接级请求节流的状态(见 Throttle)。
	throttleMu sync.Mutex
	throttled  map[string]time.Time
}

// Throttle 是连接级请求节流:同一个 key 在 minGap 内只放行一次,返回是否放行。
//
// 为什么需要它:WS 端点按设计**免鉴权**(只读推送),而"要一次状态条快照"是一次
// 上万行级别的查询 —— 不节流的话,任何人只要能连上这个端点,就能用几十字节的请求
// 换来上百 KB 的响应与一次全表扫描。正常页面每条连接只要一次(手工刷新会再要一次),
// 所以一个很短的间隔就够挡刷请求,不会误伤真实操作(多标签页各有一条连接,互不影响)。
func (c *Conn) Throttle(key string, minGap time.Duration) bool {
	now := time.Now()
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	if c.throttled == nil {
		c.throttled = map[string]time.Time{}
	}
	if last, ok := c.throttled[key]; ok && now.Sub(last) < minGap {
		return false
	}
	c.throttled[key] = now
	return true
}

func (c *Conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// ClientMessage 是浏览器发来的请求帧。浏览器平时不发帧,只有列表页要一次
// 「最近状态」快照时才发(见 events.go 的 ReqMonitorStrips)。
type ClientMessage struct {
	Type string `json:"type"`
	// Rounds 是这次要的状态条格数;<=0 表示按后台设置「最近状态格数」。
	Rounds int `json:"rounds"`
}

// RequestHandler 处理浏览器请求帧。由 api 层注入(cmd 与测试都经 api.Register 装配);
// 处理器在连接的读循环里被**同步**调用,同一条连接上的多次请求天然串行,
// 不会出现两批快照交错推送。实现里可以直接用 c.SendWait 分块回帧。
type RequestHandler func(c *Conn, msg ClientMessage)

// Hub 浏览器连接注册表(并发安全,广播非阻塞)。
type Hub struct {
	mu    sync.RWMutex
	conns map[*Conn]struct{}
	// handler 是请求帧处理器,由 SetRequestHandler 注入;为 nil 时请求帧被忽略
	// (未接 api 的装配路径与部分测试)。
	handler RequestHandler
}

func New() *Hub {
	return &Hub{conns: map[*Conn]struct{}{}}
}

// SetRequestHandler 注入请求帧处理器(启动时调用一次;重复调用以最后一次为准)。
func (h *Hub) SetRequestHandler(fn RequestHandler) {
	h.mu.Lock()
	h.handler = fn
	h.mu.Unlock()
}

// handle 把请求帧交给处理器。取处理器时只做一次短暂的读锁、调用前就释放:
// 处理器会查库、分块发帧(可能调用 Broadcast),持锁调用迟早会撞上写锁。
func (h *Hub) handle(c *Conn, msg ClientMessage) {
	h.mu.RLock()
	fn := h.handler
	h.mu.RUnlock()
	if fn != nil {
		fn(c, msg)
	}
}

// Serve 接管一条已升级的浏览器连接,先推 hello。
func (h *Hub) Serve(ws *websocket.Conn) {
	c := &Conn{ws: ws, send: make(chan Event, 32), done: make(chan struct{})}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		c.close()
	}()

	go c.writePump()
	// 浏览器侧只会发小的请求帧(见 ClientMessage);读循环负责感知断开、续期读截止,
	// 并把请求帧交给注入的处理器。
	ws.SetReadLimit(4096)
	// 续期必须在 pong 处理器里做:控制帧(pong)不会让 ReadMessage 返回,
	// 循环里那次 SetReadDeadline 只在"收到数据帧之后"才会重新执行,靠它续不了期。
	_ = ws.SetReadDeadline(time.Now().Add(browserReadWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(browserReadWait))
	})
	for {
		msgType, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var msg ClientMessage
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Type == "" {
			continue // 坏帧/未知帧:静默忽略,与"慢客户端丢帧"同款,不牵连整条连接
		}
		h.handle(c, msg)
	}
}

func (c *Conn) writePump() {
	// 写端一旦退出(连接坏了/被关)就顺手关掉整条连接:否则读循环要等到
	// browserReadWait 才会发现写不出去,这期间广播还在往一个死连接上堆帧。
	defer c.close()
	// 首帧 hello:浏览器据此知道连接建立、可停止回退轮询。
	_ = c.Send(Event{Type: "hello"})
	ping := time.NewTicker(browserPingEvery)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ping.C:
			// 控制帧 ping:浏览器自动回 pong,读循环据此续期(见 Serve)。
			// 走同一个写循环,避免与文本帧交错。
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(browserWriteWait)); err != nil {
				return
			}
		case ev := <-c.send:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(browserWriteWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}
}

func (c *Conn) Send(ev Event) error {
	select {
	case <-c.done:
		return websocket.ErrCloseSent
	default:
	}
	select {
	case c.send <- ev:
		return nil
	default:
		return nil // 浏览器太慢:丢帧(前端另有轮询兜底)
	}
}

// SendWait 与 Send 同义,但会等写队列腾出位置(最多 timeout 后放弃)。
//
// 广播可以随手丢帧(前端有兜底对齐),请求-响应的分块推送不能:少一块就是一片
// 监控的色块永远不出现,而前端不知道 "应该有第 7 块"。故应答路径用这个。
func (c *Conn) SendWait(ev Event, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return websocket.ErrCloseSent
	default:
	}
	select {
	case c.send <- ev:
		return nil
	case <-c.done:
		return websocket.ErrCloseSent
	case <-timer.C:
		return errors.New("浏览器连接写队列长时间无空闲")
	}
}

// Broadcast 向全部浏览器连接推送事件。
func (h *Hub) Broadcast(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		_ = c.Send(ev)
	}
}

// Count 在线浏览器连接数(测试/排障用)。
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}
