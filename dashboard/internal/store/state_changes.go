package store

import (
	"context"
	"time"
)

// MonitorStateChange 是监控告警状态的一次变动(UP↔DOWN 翻转)。
//
// 只在状态机真的翻转时写入一条:连续破线期间每一轮都不记,恢复也只记一条;
// UNKNOWN 轮(整轮无有效样本)冻结状态机,不产生记录。
// 展示所需的计划时间/成功率/节点明细不在本表里,而是按 RoundID 关联回 rounds ——
// 详情页的「最近状态变动记录」因此与「最近轮次时间线」是同一份数据。
type MonitorStateChange struct {
	ID        ID     `json:"id"`
	MonitorID ID     `json:"monitorId"`
	RoundID   ID     `json:"roundId"`
	FromState string `json:"fromState"` // 变动前状态(UP | DOWN)
	ToState   string `json:"toState"`   // 变动后状态(UP | DOWN)
	// SuccessRate 是触发这次变动的那一轮的成功率(变动的直接原因)。
	SuccessRate float64 `json:"successRate"`
	// SpeedKbps 是触发变动那一轮的平均下载速度(KB/s,仅下载速度监控;其余类型为 0)。
	SpeedKbps float64 `json:"speedKbps,omitempty"`
	// DurationSec 是本次报警的持续秒数:只在这条记录是**恢复**(DOWN→UP)时有意义,
	// 等于上一条 UP→DOWN 到本条之间的间隔(见 Scheduler.applyAlert 的写入处)。
	// 0 表示"没有可配对的报错记录"——这本身就是报错记录、或恢复前的那条报错记录
	// 已被级联删除/超出保留期。展示时按"无持续时长"处理,不编造。
	DurationSec int64     `json:"durationSec,omitempty"`
	ChangedAt   time.Time `json:"changedAt"`
}

const stateChangeCols = `id, monitor_id, round_id, from_state, to_state, success_rate,
	speed_kbps, duration_sec, changed_at`

func scanStateChange(row interface{ Scan(...any) error }) (*MonitorStateChange, error) {
	var (
		sc        MonitorStateChange
		changedAt int64
	)
	err := row.Scan(&sc.ID, &sc.MonitorID, &sc.RoundID, &sc.FromState, &sc.ToState,
		&sc.SuccessRate, &sc.SpeedKbps, &sc.DurationSec, &changedAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	sc.ChangedAt = fromUnixSec(changedAt)
	return &sc, nil
}

// InsertStateChange 追加一条状态变动记录。
func (s *Store) InsertStateChange(ctx context.Context, sc *MonitorStateChange) error {
	if sc.ID == "" {
		sc.ID = NewID()
	}
	if sc.ChangedAt.IsZero() {
		sc.ChangedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitor_state_changes (`+stateChangeCols+`)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		sc.ID, sc.MonitorID, sc.RoundID, sc.FromState, sc.ToState,
		sc.SuccessRate, sc.SpeedKbps, sc.DurationSec, unixSec(sc.ChangedAt))
	return normalizeErr(err)
}

// FindLastStateChangeByToState 取某监控**最近一条**指定变动方向的记录(新→旧取第一条),
// 没有则返回 ErrNotFound。
//
// 只有一个调用场景:恢复(DOWN→UP)落库前回查上一条报错记录,两者相隔多久就是本次
// 报警的持续时长(见 Scheduler.applyAlert)。按 changed_at DESC, rowid DESC 与列表
// 查询同序,保证"读到的就是页面上那条报错记录"。
func (s *Store) FindLastStateChangeByToState(ctx context.Context, monitorID ID, toState string) (*MonitorStateChange, error) {
	row := s.dbRead.QueryRowContext(ctx, `SELECT `+stateChangeCols+` FROM monitor_state_changes
		WHERE monitor_id=? AND to_state=? ORDER BY changed_at DESC, rowid DESC LIMIT 1`,
		monitorID, toState)
	return scanStateChange(row)
}

// ListStateChangesByMonitor 某监控的状态变动记录(新→旧)。
// 同一秒内的多条用 rowid 兜底排序,保证顺序就是写入顺序(与轮次时间线一致)。
func (s *Store) ListStateChangesByMonitor(ctx context.Context, monitorID ID, limit int) ([]*MonitorStateChange, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.dbRead.QueryContext(ctx, `SELECT `+stateChangeCols+` FROM monitor_state_changes
		WHERE monitor_id=? ORDER BY changed_at DESC, rowid DESC LIMIT ?`, monitorID, limit)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*MonitorStateChange{}
	for rows.Next() {
		sc, err := scanStateChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, normalizeErr(rows.Err())
}

// ListStateChanges 全局最近的状态变动记录(新→旧),总览页的「最近状态变动记录」用。
// 与按监控查询的区别只有一个:这里跨所有监控,调用方按 monitor_id 自行关联监控身份。
// 同一秒内的多条同样用 rowid 兜底排序,保证顺序就是写入顺序的倒序。
func (s *Store) ListStateChanges(ctx context.Context, limit int) ([]*MonitorStateChange, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.dbRead.QueryContext(ctx, `SELECT `+stateChangeCols+` FROM monitor_state_changes
		ORDER BY changed_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*MonitorStateChange{}
	for rows.Next() {
		sc, err := scanStateChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, normalizeErr(rows.Err())
}

// CountStateChanges 某监控的变动记录条数(测试与诊断用)。
func (s *Store) CountStateChanges(ctx context.Context, monitorID ID) (int64, error) {
	var n int64
	err := s.dbRead.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM monitor_state_changes WHERE monitor_id=?`, monitorID).Scan(&n)
	return n, normalizeErr(err)
}

// DeleteStateChangesByMonitors 删除若干监控的变动记录(监控级联删除用)。
// 与 rounds / results 一样按 monitor_id 清理,不存在不算失败。
func (s *Store) DeleteStateChangesByMonitors(ctx context.Context, ids []ID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM monitor_state_changes WHERE monitor_id IN (`+inPlaceholders(len(ids))+`)`,
		idArgs(ids)...)
	return normalizeErr(err)
}

// PruneOldStateChanges 删除 cutoff 之前的状态变动记录(changed_at 口径)。
// 变动记录按 RoundID 关联回 rounds 取展示明细(rounds.go 的 listMonitorStateChanges
// 会静默跳过查不到的轮次),轮次纳入保留期后,被清轮次引用的变动记录就成了永远
// 渲染不出来的孤儿行 —— 跟随同一条保留期清理,维持"变动记录都能配到轮次"的不变量。
// 表量级很小(只在状态翻转时写),单条 DELETE 不分批。返回删除条数。
func (s *Store) PruneOldStateChanges(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM monitor_state_changes WHERE changed_at < ?`, unixSec(cutoff))
	if err != nil {
		return 0, normalizeErr(err)
	}
	return res.RowsAffected()
}
