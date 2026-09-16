package store

import (
	"context"
	"errors"
	"time"
)

// 监控告警状态(alertState)。
const (
	MonitorUP   = "UP"
	MonitorDOWN = "DOWN"
)

// MonitorState 是监控的告警状态机持久化。ID 用 monitor 的 hex。
// LastRoundState 记录最近定稿轮次状态(CLOSED|UNKNOWN),供 UI 派生展示:
// 暂停→灰、最近轮 UNKNOWN→UNKNOWN、否则 UP/DOWN。
type MonitorState struct {
	ID              string    `json:"id"` // monitorID hex
	AlertState      string    `json:"alertState"`
	Consecutive     int       `json:"consecutive"` // 连续破线轮计数
	LastRoundState  string    `json:"lastRoundState"`
	LastSuccessRate float64   `json:"lastSuccessRate"`
	// LastSpeedKbps 是最近一轮的平均下载速度(KB/s,仅下载速度监控;其余类型为 0)。
	LastSpeedKbps float64   `json:"lastSpeedKbps"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

const monitorStateCols = `id, alert_state, consecutive, last_round_state, last_success_rate,
	last_speed_kbps, updated_at`

func scanMonitorState(row interface{ Scan(...any) error }) (*MonitorState, error) {
	var (
		st        MonitorState
		updatedAt int64
	)
	err := row.Scan(&st.ID, &st.AlertState, &st.Consecutive, &st.LastRoundState,
		&st.LastSuccessRate, &st.LastSpeedKbps, &updatedAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	st.UpdatedAt = fromUnixSec(updatedAt)
	return &st, nil
}

// GetMonitorState 不存在时返回初始态(UP/0)且 found=false。
func (s *Store) GetMonitorState(ctx context.Context, monitorID ID) (*MonitorState, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+monitorStateCols+` FROM monitor_states WHERE id=?`, monitorID.Hex())
	st, err := scanMonitorState(row)
	if errors.Is(err, ErrNotFound) {
		return &MonitorState{ID: monitorID.Hex(), AlertState: MonitorUP}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return st, true, nil
}

// PutMonitorState 整体写回状态(幂等 upsert)。
func (s *Store) PutMonitorState(ctx context.Context, st *MonitorState) error {
	st.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitor_states (`+monitorStateCols+`)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET alert_state=excluded.alert_state,
			consecutive=excluded.consecutive, last_round_state=excluded.last_round_state,
			last_success_rate=excluded.last_success_rate,
			last_speed_kbps=excluded.last_speed_kbps, updated_at=excluded.updated_at`,
		st.ID, st.AlertState, st.Consecutive, st.LastRoundState, st.LastSuccessRate,
		st.LastSpeedKbps, unixSec(st.UpdatedAt))
	return normalizeErr(err)
}

// DeleteMonitorState 监控删除时清理状态行。
func (s *Store) DeleteMonitorState(ctx context.Context, monitorID ID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM monitor_states WHERE id=?`, monitorID.Hex())
	return normalizeErr(err)
}

// ResetMonitorsConsecutive 批量清零连续破线计数(批量暂停用:维护窗口不计入连续破线,
// 与单行 SetMonitorEnabled 的处理一致)。没有状态行的监控本来就没有计数,0 命中属正常。
func (s *Store) ResetMonitorsConsecutive(ctx context.Context, ids []ID) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, unixSec(time.Now()))
	for _, id := range ids {
		args = append(args, id.Hex())
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE monitor_states SET consecutive=0, updated_at=? WHERE id IN (`+
			inPlaceholders(len(ids))+`)`, args...)
	return normalizeErr(err)
}

// GetMonitorStatesByIDs 批量读展示状态;缺失按初始 UP。读失败返回已取到的部分。
func (s *Store) GetMonitorStatesByIDs(ctx context.Context, ids []string) map[string]*MonitorState {
	out := map[string]*MonitorState{}
	if len(ids) == 0 {
		return out
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+monitorStateCols+` FROM monitor_states WHERE id IN (`+inPlaceholders(len(args))+`)`,
		args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanMonitorState(rows)
		if err == nil {
			out[st.ID] = st
		}
	}
	return out
}

// CountDownMonitors 统计此刻处于告警状态(alert_state=DOWN)的**启用中**监控数:
// 通知模板的 {{errorCount}} 与 webhook 载荷的 errorCount 用它说清"全局还挂着几个"
// (见 notifytmpl.Defaults 的标题前缀)。
//
// 暂停中的监控不算:暂停只清连续破线计数、**保留**告警状态(见 api.setMonitorEnabled),
// 它们不再产生轮次也不会再告警,计进去会让这个数虚高 —— 在监控列表上它们显示为「暂停」,
// 与这里的口径一致。
//
// 没有状态行的监控(从未定稿过)自然不在结果里。
func (s *Store) CountDownMonitors(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_states st
		JOIN monitors m ON m.id = st.id WHERE st.alert_state=? AND m.enabled=1`,
		MonitorDOWN).Scan(&n)
	return n, normalizeErr(err)
}
