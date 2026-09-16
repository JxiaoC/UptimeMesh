// Package alert 实现监控告警状态机(spec Q12/Q17):
// 轮次判定值低于阈值 ⇒ 破线轮;连续 N 个破线轮 ⇒ DOWN(翻转触发);
// DOWN 之后任一轮达标 ⇒ 恢复 UP(翻转触发);UNKNOWN 轮既不算破线也不恢复。
// 本包为纯函数,持久化与通知由调用方(dashboard/scheduler)负责。
//
// 判定值是**与监控阈值同单位的那个数**:成功率监控是本次成功率(0~100),
// 下载速度监控是本轮平均速度(KB/s)——两者都是"越低越差",故状态机不必知道监控类型
// (见 .scratch/download-speed-monitor/spec.md)。
package alert

import "time"

// RoundOutcome 定稿轮次对状态机的输入。
type RoundOutcome struct {
	Valid bool // false = UNKNOWN 轮(无有效样本),被状态机忽略
	// Value 是本轮判定值(成功率 % 或平均速度 KB/s),与监控阈值同单位比较。
	Value float64
	// SuccessRate 始终是本次成功率(成功样本 ÷ 有效样本):通知模板与展示要用它;
	// 下载速度监控里它表示"下载成功率(可下载率)",不参与阈值判定。
	SuccessRate float64
}

// Breached 轮次是否破线(判定值低于阈值)。
func (ro RoundOutcome) Breached(threshold float64) bool {
	return ro.Valid && ro.Value < threshold
}

// State 状态机持久化部分。
type State struct {
	AlertState  string // UP | DOWN
	Consecutive int    // 当前连续破线轮数
}

const (
	Up   = "UP"
	Down = "DOWN"
)

// Event 状态翻转事件(仅翻转时产生)。
type Event struct {
	Type string // DOWN | UP
	// RoundID 是触发翻转的那一轮:通知要据此列出各节点明细(本次结果按轮次关联)。
	// 由调用方(调度器)在 Apply 之后回填,alert 包自身不认识轮次。
	RoundID string
	// Value 是触发翻转那一轮的判定值(与监控阈值同单位):通知模板按监控类型
	// 把它展示成"本次成功率 x%"或"平均下载速度 x MB/s"。
	Value float64
	// SuccessRate 是触发翻转那一轮的本次成功率(见 RoundOutcome.SuccessRate)。
	SuccessRate float64
	// FromState / ChangedAt 是这次翻转留下的那条「状态变动记录」的字段
	// (翻转前的告警态、记录写入时间):与 RoundID 同款 —— 由调用方(调度器)在落记录
	// 时回填,alert 包自身不认识存储。总览页的变动流水要靠它们插一行。
	FromState string
	ChangedAt time.Time
	// DurationSec 是本次报警的持续秒数:仅恢复(UP)事件有值,由调用方(调度器)
	// 回查上一条 DOWN 记录算出 —— 与 FromState/ChangedAt 同款,alert 包不认识存储。
	// 0 = 没有可配对的报错记录(展示时按"无持续时长"处理)。
	DurationSec int64
}

const (
	EventDown = "DOWN"
	EventUp   = "UP"
)

// Apply 输入一轮定稿结果,返回新状态与翻转事件(无翻转则 event=nil)。
// consecutiveN 须 >=1(监控配置校验保证)。
func Apply(s State, outcome RoundOutcome, threshold float64, consecutiveN int) (State, *Event) {
	next := s
	if next.AlertState == "" {
		next.AlertState = Up
	}
	if !outcome.Valid {
		return next, nil // UNKNOWN:冻结计数与状态
	}
	if outcome.Breached(threshold) {
		next.Consecutive++
		if next.AlertState == Up && next.Consecutive >= consecutiveN {
			next.AlertState = Down
			return next, &Event{Type: EventDown, Value: outcome.Value, SuccessRate: outcome.SuccessRate}
		}
		return next, nil
	}
	next.Consecutive = 0
	if next.AlertState == Down {
		next.AlertState = Up
		return next, &Event{Type: EventUp, Value: outcome.Value, SuccessRate: outcome.SuccessRate}
	}
	return next, nil
}
