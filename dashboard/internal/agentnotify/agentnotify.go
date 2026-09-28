// Package agentnotify 在节点掉线/恢复上线时向勾选了「节点通知」的渠道发推送
// (AGENT_DOWN/AGENT_UP 事件,见 notifier.EmitAgentEvent)。
//
// 为什么要延迟确认:节点闪断几秒就重连(重连退避 1s→30s,见 shared/agentclient)
// 是常态,掉线事件一到就发通知会把网络抖动变成误报轰炸。因此掉线后先等
// 「延迟确认秒数」(设置页可配,默认 60s),到点仍未上线才真正发 AGENT_DOWN;
// 等待期间重连则取消,一条通知都不发。恢复(AGENT_UP)只在掉线通知**实际发出过**
// 之后才发,并带上本次离线时长 —— 防抖期内闪断恢复的通知纯属噪音。
//
// 状态存内存(pending 定时器表):Dashboard 重启即丢,重启期间的掉线不补发 ——
// 掉线推送是实时提示而非账目,重启后节点要么重连(OnAgentConnected 正常流转),
// 要么仍离线但没有任何"翻转"可发,补发反而要在重启路径上回查全部节点,不值得。
package agentnotify

import (
	"context"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/notifier"
	"github.com/uptimemesh/dashboard/internal/store"
)

// Service 处理节点上下线事件的防抖与推送。由 main 在 hub.OnAgentChange 里调用
// OnAgentChange(浏览器广播之后),与 hub 的连接生命周期串在同一条回调链上。
type Service struct {
	st   *store.Store
	nf   *notifier.Notifier
	// isOnline 判定节点此刻是否在线(注入 hub.IsOnline,便于单测替换)。
	isOnline func(agentID store.ID) bool

	mu      sync.Mutex
	pending map[store.ID]*agentPending
}

// agentPending 是一个节点的在途掉线确认状态。
type agentPending struct {
	// timer 到点仍未上线则发 AGENT_DOWN;重连时 Stop 掉。
	timer *time.Timer
	// notified = AGENT_DOWN 已实际发出。恢复通知只在它为 true 时才发,
	// offlineSince 是发出掉线通知的时刻(离线时长的起点)。
	notified     bool
	offlineSince time.Time
}

// New 创建服务。delay 由调用方在事件到来时现查(设置页可随时改,不缓存旧值)。
func New(st *store.Store, nf *notifier.Notifier, isOnline func(store.ID) bool) *Service {
	return &Service{
		st:       st,
		nf:       nf,
		isOnline: isOnline,
		pending:  make(map[store.ID]*agentPending),
	}
}

// OnAgentChange 处理节点的上线/下线事件(hub.OnAgentChange 的追加消费者)。
// 事件由 hub 在锁外逐个调用,本方法内部有自己的锁,不会与 hub 死锁。
func (s *Service) OnAgentChange(ctx context.Context, agentID store.ID, online bool) {
	if online {
		s.onConnected(ctx, agentID)
		return
	}
	s.onDisconnected(ctx, agentID)
}

// onDisconnected 掉线:查节点(是否还配得上通知)与延迟秒数,起一个到点重查的定时器。
// 查库失败只记日志 —— 通知是尽力而为的旁路,不能反过来影响连接事件的处理。
func (s *Service) onDisconnected(ctx context.Context, agentID store.ID) {
	ag, err := s.st.FindAgentByID(ctx, agentID)
	if err != nil {
		if err != store.ErrNotFound {
			g.Log().Warningf(ctx, "节点 %s 掉线推送:读取节点失败: %v", agentID.Hex(), err)
		}
		return
	}
	// 只有已批准且开关打开的节点才值得等:pending/revoked/deleted 的连接断开是正常流转。
	if ag.Status != store.AgentApproved || !ag.OfflineNotify {
		return
	}
	delay := time.Duration(s.st.AgentOfflineNotifySeconds(ctx)) * time.Second

	s.mu.Lock()
	defer s.mu.Unlock()
	// 已有在途确认(同 ID 旧连接被新连接替换又断开的场景)不重复起表项:
	// 保留原 timer 即可,它到点会按最新状态重查。
	if _, ok := s.pending[agentID]; ok {
		return
	}
	p := &agentPending{}
	p.timer = time.AfterFunc(delay, func() { s.confirmOffline(agentID) })
	s.pending[agentID] = p
}

// confirmOffline 延迟确认到点:重查「仍未上线 ∧ 仍是 approved ∧ 开关仍开」,
// 全部满足才真正发 AGENT_DOWN。等待期间用户关了开关/吊销了节点、或节点已悄悄
// 重连(有这种竞态:重连事件还在路上),都从这里拦下。
func (s *Service) confirmOffline(agentID store.ID) {
	ctx := context.Background()
	ag, err := s.st.FindAgentByID(ctx, agentID)
	if err != nil {
		s.clear(agentID)
		return
	}
	if s.isOnline(agentID) || ag.Status != store.AgentApproved || !ag.OfflineNotify {
		s.clear(agentID)
		return
	}
	s.mu.Lock()
	p, ok := s.pending[agentID]
	if !ok {
		s.mu.Unlock()
		return
	}
	p.notified = true
	p.offlineSince = time.Now()
	s.mu.Unlock()

	g.Log().Infof(ctx, "节点 %s(%s)掉线超过延迟确认时间,发送离线通知", ag.Name, agentID.Hex())
	s.nf.EmitAgentEvent(ctx, ag, notifytmpl.EventAgentDown, 0)
}

// onConnected 上线:取消在途确认;掉线通知已发出过则发恢复通知(带离线时长)。
func (s *Service) onConnected(ctx context.Context, agentID store.ID) {
	s.mu.Lock()
	p, ok := s.pending[agentID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// 先摘表项再发通知:AGEN_UP 的投递是异步的,摘掉后即便投递中又来一轮
	// 掉线/上线,也会走全新的确认流程,不会复用旧的 notified 状态。
	delete(s.pending, agentID)
	notified, offlineSince := p.notified, p.offlineSince
	p.timer.Stop()
	s.mu.Unlock()

	if !notified {
		return // 防抖期内就恢复了:闪断,一条通知都不发
	}
	ag, err := s.st.FindAgentByID(ctx, agentID)
	if err != nil {
		g.Log().Warningf(ctx, "节点 %s 恢复上线但读取节点失败,恢复通知未发送: %v", agentID.Hex(), err)
		return
	}
	// 只讲「掉了多久」:起点是发出掉线通知那一刻(延迟确认之后),不是连接断开那一刻,
	// 与通知内容自洽 —— 收到"离线 2 分"时,前面那条掉线通知正是约 2 分前收到的。
	duration := int64(time.Since(offlineSince).Seconds())
	g.Log().Infof(ctx, "节点 %s(%s)恢复上线,离线 %d 秒,发送恢复通知", ag.Name, agentID.Hex(), duration)
	s.nf.EmitAgentEvent(ctx, ag, notifytmpl.EventAgentUp, duration)
}

// clear 取消在途确认(节点被删除/吊销、或状态已不值得通知时)。
func (s *Service) clear(agentID store.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[agentID]; ok {
		p.timer.Stop()
		delete(s.pending, agentID)
	}
}
