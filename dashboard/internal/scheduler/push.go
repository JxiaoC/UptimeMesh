package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/store"
)

// PushAgentID 是 push(外部上报)监控在结果表里的"执行者"占位标识。
// 这类监控没有 Agent,结果是外部系统调用 /api/push/{token} 报上来的;
// 用一个固定标识区分,前端据此显示为「外部上报」。
const PushAgentID = "external"

// ReportPush 处理一次外部上报:先记下上报时间(静默看门狗的基准),再立即生成一条
// 单样本轮次并走既有定稿/告警路径 —— push 监控因此与其它类型共用同一套
// 本次成功率、可用率、连续破线与通知口径。
//
// ok=false 表示上报方报告故障(status=down);latencyMs 来自上报的 ping 参数(可为 0)。
func (s *Scheduler) ReportPush(ctx context.Context, m *store.Monitor, ok bool, latencyMs float64, note string) {
	at := time.Now()
	if err := s.st.RecordPushReport(ctx, m.ID, at); err != nil {
		g.Log().Errorf(ctx, "记录外部上报时间失败(监控 %s): %v", m.Name, err)
	}
	// 反转模式只翻转**上报的判定**(上报故障算正常):上报本身什么时候来,与反转无关。
	s.pushRound(ctx, m, ok, latencyMs, at, note, m.InvertMode)
}

// cyclePush 静默看门狗:push 监控超过一个周期没有任何上报时,按周期补一条失败轮次。
// 连续 N 轮静默(监控配置的连续破线轮数)后告警状态机自然翻转为 DOWN,
// 因此"多久算失联"= 周期 × 连续破线轮数。
//
// 从未上报过的监控以创建时间为基准:创建后就该有上报,否则同样会逐渐判 DOWN。
func (s *Scheduler) cyclePush(ctx context.Context, m *store.Monitor, now time.Time) {
	baseline := m.CreatedAt
	if !m.LastPushAt.IsZero() {
		baseline = m.LastPushAt
	}
	if baseline.IsZero() {
		return
	}
	period := time.Duration(m.Period) * time.Second
	if period <= 0 || now.Sub(baseline) < period {
		return
	}
	// 按周期节流:调度器每 tick(1s)跑一次,不节流会把一次静默判成一堆重复轮次。
	// 槽位与探测轮共用,但这里的 due 只表示"上一轮静默轮的时刻"(push 监控走不到
	// planRound 那条路),静默时长本身按"上次上报时间"算,没有漂移问题。
	s.mu.Lock()
	slot, seen := s.monitors[m.ID]
	s.mu.Unlock()
	if seen && now.Sub(slot.due) < period {
		return
	}
	s.mu.Lock()
	s.monitors[m.ID] = monitorSlot{due: now, period: m.Period}
	s.mu.Unlock()

	silence := now.Sub(baseline)
	// 反转模式**不**作用于静默轮:反转的是"上报说了什么"(故障算正常),不是
	// "有没有上报"。跟着反转的话,反转监控会静默到天荒地老也不告警 —— 而
	// 「外部系统还在不在上报」正是这条监控要回答的问题之一。
	s.pushRound(ctx, m, false, 0, now,
		fmt.Sprintf("静默:%s 没有收到外部上报(周期 %d 秒)", humanSeconds(silence), m.Period), false)
}

// pushRound 生成一条单样本轮次并立即定稿。AssignedAgentIds 留空(没有节点参与),
// 但聚合的样本键用 PushAgentID,使轮次照常有 success/valid 与成功率。
//
// invert 是本轮的反转快照:上报轮取监控配置,静默轮恒为 false(见 cyclePush)。
func (s *Scheduler) pushRound(ctx context.Context, m *store.Monitor, ok bool, latencyMs float64, at time.Time, note string, invert bool) {
	round := &store.Round{
		MonitorID: m.ID, AssignedAgentIds: []string{},
		ScheduledAt: at, Deadline: at, State: store.RoundStateOpen, TotalAgents: 0,
	}
	if err := s.st.InsertRound(ctx, round); err != nil {
		g.Log().Errorf(ctx, "push 轮次落库失败(监控 %s): %v", m.Name, err)
		return
	}
	// 阈值恒定(store.PushThreshold):push 每轮只有一个样本,成功率非 0% 即 100%,
	// 阈值填多少判定都一样,故不再读监控上的那个数字(见该常量的说明)。
	// 反转模式与阈值/连续轮数在本轮快照里固定(与探测轮同口径)。
	spec := RoundSpec{
		RoundID: round.ID.Hex(), MonitorID: m.ID.Hex(),
		AgentIDs: []string{PushAgentID}, Deadline: at, ScheduledAt: at,
		Threshold: store.PushThreshold, Consecutive: m.Consecutive, Invert: invert,
	}
	flight := NewRound(spec)
	s.mu.Lock()
	s.rounds[round.ID] = flight
	s.mu.Unlock()

	// 失败原因同时进样本(推送的节点明细要显示它)与结果表(排障要看它),两处同源。
	pushErr := pushNote(ok, note)
	// 外部上报没有"下发"这一步:样本由上报方直接送来,等价于任务已交到手上
	// (不标这一下,定稿会把它当成"从未下发"的缺样)。
	flight.MarkDispatched(PushAgentID)
	if !flight.AddResult(PushAgentID, ok, latencyMs, 0, pushErr) {
		return
	}
	res := &store.CheckResult{
		RoundID: round.ID, MonitorID: m.ID, AgentID: store.ID(PushAgentID),
		OK: spec.EffOK(ok), LatencyMs: latencyMs, Error: pushErr,
		ScheduledAt: at,
	}
	if err := s.st.InsertResult(ctx, res); err != nil {
		g.Log().Errorf(ctx, "push 结果落库失败(监控 %s): %v", m.Name, err)
	}
	s.finalize(ctx, flight)
}

// pushNote 上报成功不留错误信息;失败时把上报原因(或静默说明)记进结果,便于排障。
func pushNote(ok bool, note string) string {
	if ok {
		return ""
	}
	if note == "" {
		return "外部上报为故障状态"
	}
	return note
}

// humanSeconds 把时长渲染成人类可读的中文描述(用于静默说明)。
func humanSeconds(d time.Duration) string {
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d 小时 %d 分", sec/3600, (sec%3600)/60)
	}
}
