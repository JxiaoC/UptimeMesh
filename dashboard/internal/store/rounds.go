package store

import (
	"context"
	"database/sql"
	"time"
)

// 轮次定稿状态(票 06/07 使用 Closed 字段区分)。
const (
	RoundStateOpen    = "OPEN"
	RoundStateClosed  = "CLOSED"
	RoundStateUnknown = "UNKNOWN"
)

// Round(探测轮次)一次逻辑探测的聚合记录。
type Round struct {
	ID        ID `json:"id"`
	MonitorID ID `json:"monitorId"`
	// 创建时快照的指派节点(ID hex),定稿探活以此为对象。
	AssignedAgentIds []string  `json:"assignedAgentIds"`
	ScheduledAt      time.Time `json:"scheduledAt"`
	Deadline         time.Time `json:"deadline"`
	ClosedAt         time.Time `json:"closedAt,omitempty"`

	State       string  `json:"state"` // OPEN | CLOSED | UNKNOWN
	Success     int     `json:"success"`
	Valid       int     `json:"valid"`
	TotalAgents int     `json:"totalAgents"`
	SuccessRate float64 `json:"successRate"`

	// 本次平均延迟的中间量(票 09)。
	LatencySumMs float64 `json:"latencySumMs,omitempty"`
	LatencyCount int     `json:"latencyCount,omitempty"`

	// 本次平均下载速度的中间量(下载速度监控;其余类型恒为 0)。
	// SpeedCount 是给出速度的样本数(下载失败的样本速度为 0 但同样计数),
	// 平均速度 = SpeedSumKbps / SpeedCount。
	SpeedSumKbps float64 `json:"speedSumKbps,omitempty"`
	SpeedCount   int     `json:"speedCount,omitempty"`

	// 缺样决策表的三组节点(排障用)。前两组是"任务交到了节点手上但它没回结果"
	// (活着没报 ⇒ 计失败)与"节点离线"(不计分母);MissingUndispatched 是任务**从未
	// 交出去**的节点(建轮与重连补发时它都不在线),同"不计分母"但成因在派发侧。
	MissingAlive []string `json:"missingAlive,omitempty"`
	MissingDead  []string `json:"missingDead,omitempty"`
	// MissingUndispatched 见 scheduler.Aggregation.MissingUndispatched:
	// 与离线同处置(不计分母),单列一列是为了让页面/通知说清"这一格为什么没有样本"。
	MissingUndispatched []string `json:"missingUndispatched,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// AvgSpeedKbps 本轮平均下载速度(KB/s):没有给出速度的样本返回 0
// (只有下载速度监控会有非零值,其余类型恒为 0)。
func (r *Round) AvgSpeedKbps() float64 {
	if r.SpeedCount <= 0 {
		return 0
	}
	return r.SpeedSumKbps / float64(r.SpeedCount)
}

// CheckResult(检测结果)某节点对某轮次的单次执行记录。
type CheckResult struct {
	ID        ID `json:"id"`
	RoundID   ID `json:"roundId"`
	MonitorID ID `json:"monitorId"`
	AgentID   ID `json:"agentId"`

	OK         bool    `json:"ok"`
	LatencyMs  float64 `json:"latencyMs"`
	HTTPStatus int     `json:"httpStatus,omitempty"`
	Error      string  `json:"error,omitempty"`
	// SpeedKbps 是下载速度监控里该节点本轮测得的速度(KB/s);其余类型恒为 0。
	SpeedKbps float64 `json:"speedKbps,omitempty"`
	// Late=true 的晚到结果不进入轮次聚合(票 06 收口语义)。
	Late bool `json:"late"`
	// ScheduledAt 是该结果所属轮次的计划时间,小时分桶与窗口过滤都以它为准
	// (票 09 迁移修正:原实现用 createdAt 过滤、scheduledAt 分桶,口径不一致)。
	ScheduledAt time.Time `json:"scheduledAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

// RoundClose 轮次定稿要写回的聚合字段。
// 取代原先「用 bson.M 直接拼字段名」的载荷,避免存储层字段名泄漏到调度器。
type RoundClose struct {
	State        string
	Success      int
	Valid        int
	SuccessRate  float64
	LatencySumMs float64
	LatencyCount int
	SpeedSumKbps float64
	SpeedCount   int
	MissingAlive []string
	MissingDead  []string
	// MissingUndispatched 是任务从未交到节点手上的指派节点(计入 missing_dead 之外的
	// 单独一列;两者在聚合口径上都是"不计分母")。
	MissingUndispatched []string
}

const roundCols = `id, monitor_id, assigned_agent_ids, scheduled_at, deadline, closed_at,
	state, success, valid, total_agents, success_rate, latency_sum_ms, latency_count,
	speed_sum_kbps, speed_count, missing_alive, missing_dead, missing_undispatched, created_at`

func scanRound(row interface{ Scan(...any) error }) (*Round, error) {
	var (
		r                                   Round
		assigned, alive, dead, undispatched sql.NullString
		scheduled, deadline, closed         int64
		createdAt                           int64
	)
	err := row.Scan(&r.ID, &r.MonitorID, &assigned, &scheduled, &deadline, &closed,
		&r.State, &r.Success, &r.Valid, &r.TotalAgents, &r.SuccessRate,
		&r.LatencySumMs, &r.LatencyCount, &r.SpeedSumKbps, &r.SpeedCount,
		&alive, &dead, &undispatched, &createdAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	r.ScheduledAt, r.Deadline, r.ClosedAt = fromUnixSec(scheduled), fromUnixSec(deadline), fromUnixSec(closed)
	r.CreatedAt = fromUnixSec(createdAt)
	for _, item := range []struct {
		raw  sql.NullString
		into any
	}{
		{assigned, &r.AssignedAgentIds},
		{alive, &r.MissingAlive},
		{dead, &r.MissingDead},
		{undispatched, &r.MissingUndispatched},
	} {
		if err = parseJSON(item.raw, item.into); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

func (s *Store) InsertRound(ctx context.Context, r *Round) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	r.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO rounds (`+roundCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.MonitorID, mustJSON(r.AssignedAgentIds), unixSec(r.ScheduledAt),
		unixSec(r.Deadline), unixSec(r.ClosedAt), r.State, r.Success, r.Valid,
		r.TotalAgents, r.SuccessRate, r.LatencySumMs, r.LatencyCount,
		r.SpeedSumKbps, r.SpeedCount,
		mustJSON(r.MissingAlive), mustJSON(r.MissingDead), mustJSON(r.MissingUndispatched),
		unixSec(r.CreatedAt))
	return normalizeErr(err)
}

// CloseRound 定稿写入聚合字段(幂等:仅 OPEN 时生效,返回是否更新)。
func (s *Store) CloseRound(ctx context.Context, id ID, upd RoundClose) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE rounds SET state=?, success=?, valid=?,
		success_rate=?, latency_sum_ms=?, latency_count=?, speed_sum_kbps=?, speed_count=?,
		missing_alive=?, missing_dead=?, missing_undispatched=?,
		closed_at=? WHERE id=? AND state=?`,
		upd.State, upd.Success, upd.Valid, upd.SuccessRate, upd.LatencySumMs,
		upd.LatencyCount, upd.SpeedSumKbps, upd.SpeedCount,
		mustJSON(upd.MissingAlive), mustJSON(upd.MissingDead), mustJSON(upd.MissingUndispatched),
		unixSec(time.Now()), id, RoundStateOpen)
	if err != nil {
		return false, normalizeErr(err)
	}
	n, err := res.RowsAffected()
	return n == 1, normalizeErr(err)
}

// orphanRound 启动兜底要收口的残留 OPEN 轮次,连同复算所需的静态信息。
type orphanRound struct {
	ID          ID
	MonitorID   ID
	Assigned    []string  // 建轮时的指派节点快照(缺样判定以它为准)
	ScheduledAt time.Time // 小时聚合按它落桶
	Download    bool      // 速度口径:要不要累计本次速度中间量
}

// CloseOrphanOpenRounds 启动时调用:收口上次进程残留的 OPEN 轮次
// (单实例 Dashboard 重启中断调度的兜底,ADR-0003 已知后果)。
//
// 不能一律按"无有效样本"关掉。轮次聚合活在 Dashboard 内存里,样本却是**先落库**的
// (见 scheduler.handleResult:先 AddResult 聚合、再 InsertResult 入库),进程重启丢掉的
// 只是内存聚合,不是样本。若直接置 UNKNOWN+valid=0,就会出现"轮次说无有效样本、
// 节点明细里却躺着一条 200"的自相矛盾 —— 线上 2026-09-14 04:16:43 的 gitlab 轮:
// 样本 04:16:44 按时入库,04:17:48 进程重启后被本函数按零样本收口(该轮
// missing_alive/missing_dead 两列都是 NULL,正是"从未被 finalize 写过"的指纹)。
// 代价还不止显示:这些样本会从可用率里消失(可用率只认 rounds 聚合),
// 却在分节点延时曲线里照旧出现(曲线直读 results)。
//
// 因此这里按**已入库的按时样本**复算,口径与 scheduler.aggregate 一致:
//
//	valid   = 指派节点中按时回传的样本数(results.late=0,且 agent_id 在指派列表内)
//	success = 其中判定为成功的条数
//	缺样的指派节点一律**不计分母**并记入 missing_dead。
//
// 两处刻意与定稿(scheduler.finalize)不同,原因都是"启动瞬间无从判断":
//   - 不再折算反转:EffOK 折算在 handleResult 里就已落到 results.ok 上(存的是有效判定),
//     这里再折算一次会把反转监控翻回去;
//   - 不探活缺样节点:此刻节点必然还没重连,探活只会得到"离线"这个假信号。
//     "在线却没报 ⇒ 计失败"只属于进程活着时的定稿;这里宁可少算失败,也不凭空造失败。
//
// valid>0 ⇒ CLOSED(带成功率与延时/速度中间量),valid==0 才 ⇒ UNKNOWN。
// 不变量:启动后不留 OPEN 残留 —— 复算或写入失败的轮次由末尾 forceCloseOpenRounds
// 按老办法兜底:前端会把 OPEN 轮次当 0% 的故障轮渲染,留下 OPEN 比少算几轮更糟。
func (s *Store) CloseOrphanOpenRounds(ctx context.Context) (int64, error) {
	orphans, err := s.listOrphanOpenRounds(ctx)
	if err != nil {
		// 连待收口清单都读不出来:退回"一律 UNKNOWN",至少保证没有 OPEN 残留。
		n, sweepErr := s.forceCloseOpenRounds(ctx)
		if sweepErr != nil {
			return n, sweepErr
		}
		return n, err
	}
	var (
		closed   int64
		firstErr error
	)
	for _, rd := range orphans {
		ok, err := s.recoverOrphanRound(ctx, rd)
		if ok {
			closed++
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	n, sweepErr := s.forceCloseOpenRounds(ctx)
	closed += n
	if sweepErr != nil {
		return closed, sweepErr
	}
	return closed, firstErr
}

// listOrphanOpenRounds 读出全部残留 OPEN 轮次及其复算所需信息。
func (s *Store) listOrphanOpenRounds(ctx context.Context) ([]orphanRound, error) {
	rows, err := s.dbRead.QueryContext(ctx, `SELECT r.id, r.monitor_id, r.assigned_agent_ids,
			r.scheduled_at, COALESCE(m.type, '')
		FROM rounds r LEFT JOIN monitors m ON m.id = r.monitor_id
		WHERE r.state=? ORDER BY r.scheduled_at ASC`, RoundStateOpen)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []orphanRound{}
	for rows.Next() {
		var (
			rd         orphanRound
			assigned   sql.NullString
			scheduled  int64
			monitorTyp string
		)
		if err = rows.Scan(&rd.ID, &rd.MonitorID, &assigned, &scheduled, &monitorTyp); err != nil {
			return nil, normalizeErr(err)
		}
		if err = parseJSON(assigned, &rd.Assigned); err != nil {
			return nil, err
		}
		rd.ScheduledAt = fromUnixSec(scheduled)
		rd.Download = monitorTyp == monitorTypeDownload
		out = append(out, rd)
	}
	return out, normalizeErr(rows.Err())
}

// monitorTypeDownload 是下载速度监控的类型取值(与 checkconfig.TypeDownload 同为
// "download";store 不 import checkconfig —— 那是节点侧的配置包,存储层不该依赖它)。
const monitorTypeDownload = "download"

// recoverOrphanRound 按已入库的按时样本复算一个残留轮次并写回;返回是否真的收口了。
//
// 只认 late=0 且 agent_id 在指派列表内的样本(与 scheduler.aggregate 一致):
// 晚到样本本来就不进聚合,非指派节点的结果也不该进本轮分母。
func (s *Store) recoverOrphanRound(ctx context.Context, rd orphanRound) (bool, error) {
	assigned := make(map[string]bool, len(rd.Assigned))
	for _, id := range rd.Assigned {
		assigned[id] = true
	}
	rows, err := s.dbRead.QueryContext(ctx, `SELECT agent_id, ok, latency_ms, speed_kbps
		FROM results WHERE round_id=? AND late=0`, rd.ID)
	if err != nil {
		return false, normalizeErr(err)
	}
	upd := RoundClose{State: RoundStateUnknown}
	// 成功判定直接取 results.ok(存的就是有效判定,已折算过反转),见函数注释。
	reported := make(map[string]bool, len(rd.Assigned))
	for rows.Next() {
		var (
			agentID   ID
			ok        bool
			latencyMs float64
			speedKbps float64
		)
		if err = rows.Scan(&agentID, &ok, &latencyMs, &speedKbps); err != nil {
			rows.Close()
			return false, normalizeErr(err)
		}
		if !assigned[agentID.Hex()] {
			continue
		}
		reported[agentID.Hex()] = true
		upd.Valid++
		upd.LatencySumMs += latencyMs
		upd.LatencyCount++
		if ok {
			upd.Success++
		}
		if rd.Download {
			// 速度按原始观测累计,下载失败的样本速度为 0 但照样计数。
			if ok {
				upd.SpeedSumKbps += speedKbps
			}
			upd.SpeedCount++
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, normalizeErr(err)
	}
	// 缺样节点:一律不计分母,记入 missing_dead(启动瞬间无从判断"离线"还是"在线没报")。
	for _, id := range rd.Assigned {
		if !reported[id] {
			upd.MissingDead = append(upd.MissingDead, id)
		}
	}
	if upd.Valid > 0 {
		upd.State = RoundStateClosed
		upd.SuccessRate = float64(upd.Success) / float64(upd.Valid) * 100
	}
	updated, err := s.CloseRound(ctx, rd.ID, upd)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil // 已被别处定稿(幂等)
	}
	// 小时聚合要一起补:可用率只认它,少了这一步样本仍然进不了 24h/7d/30d 窗口。
	// 口径与 scheduler.finalize 一致:按本轮 valid/success 累加。
	if upd.Valid > 0 {
		if err = s.AddHourlyStat(ctx, rd.MonitorID, rd.ScheduledAt, 1,
			upd.Valid, upd.Success, upd.LatencySumMs, upd.LatencyCount); err != nil {
			return true, err
		}
	}
	return true, nil
}

// forceCloseOpenRounds 兜底:不读样本,把残留 OPEN 轮次一律按 UNKNOWN 关闭
// (老办法;复算路径失败或读不到清单时用它保证"启动后不留 OPEN")。
func (s *Store) forceCloseOpenRounds(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE rounds SET state=?, valid=0, success=0,
		success_rate=0, closed_at=? WHERE state=?`,
		RoundStateUnknown, unixSec(time.Now()), RoundStateOpen)
	if err != nil {
		return 0, normalizeErr(err)
	}
	return res.RowsAffected()
}

// FindRoundByID 按主键取轮次;不存在返回 ErrNotFound。
func (s *Store) FindRoundByID(ctx context.Context, id ID) (*Round, error) {
	row := s.dbRead.QueryRowContext(ctx, `SELECT `+roundCols+` FROM rounds WHERE id=?`, id)
	return scanRound(row)
}

// ListRoundsByMonitor 详情页轮次时间线(新→旧)。
// 同一秒内的多轮(例如外部上报连续到达)用 rowid 兜底排序,保证顺序就是写入顺序。
func (s *Store) ListRoundsByMonitor(ctx context.Context, monitorID ID, limit int) ([]*Round, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.dbRead.QueryContext(ctx, `SELECT `+roundCols+` FROM rounds
		WHERE monitor_id=? ORDER BY scheduled_at DESC, rowid DESC LIMIT ?`, monitorID, limit)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*Round{}
	for rows.Next() {
		r, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, normalizeErr(rows.Err())
}

// FindLatestClosed 最近一个定稿轮次(含 UNKNOWN;总览卡片"最近轮"用)。
func (s *Store) FindLatestClosed(ctx context.Context, monitorID ID) (*Round, bool, error) {
	row := s.dbRead.QueryRowContext(ctx, `SELECT `+roundCols+` FROM rounds
		WHERE monitor_id=? AND state<>? ORDER BY scheduled_at DESC LIMIT 1`,
		monitorID, RoundStateOpen)
	r, err := scanRound(row)
	if err == ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return r, true, nil
}

func (s *Store) InsertResult(ctx context.Context, cr *CheckResult) error {
	if cr.ID == "" {
		cr.ID = NewID()
	}
	if cr.CreatedAt.IsZero() {
		cr.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO results
		(id, round_id, monitor_id, agent_id, ok, latency_ms, http_status, error, speed_kbps, late,
		 scheduled_at, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		cr.ID, cr.RoundID, cr.MonitorID, cr.AgentID, cr.OK, cr.LatencyMs,
		cr.HTTPStatus, cr.Error, cr.SpeedKbps, cr.Late, unixSec(cr.ScheduledAt), unixSec(cr.CreatedAt))
	return normalizeErr(err)
}

// RoundLite 列表页状态条用的轮次精简视图。
type RoundLite struct {
	State       string    `json:"state"`
	SuccessRate float64   `json:"successRate"`
	ScheduledAt time.Time `json:"scheduledAt"`
	// SpeedSumKbps/SpeedCount 是下载速度监控的本次速度中间量(其余类型为 0),
	// 供状态条悬停显示本轮平均速度。
	SpeedSumKbps float64 `json:"speedSumKbps,omitempty"`
	SpeedCount   int     `json:"speedCount,omitempty"`
}

// AvgSpeedKbps 本轮平均下载速度(KB/s);没有速度样本时为 0。
func (r *RoundLite) AvgSpeedKbps() float64 {
	if r.SpeedCount <= 0 {
		return 0
	}
	return r.SpeedSumKbps / float64(r.SpeedCount)
}

// ListRecentRoundsByMonitors 批量取每个监控最近 limit 个已定稿轮次(OPEN 除外),
// 时间升序返回(最老在前)。列表页状态条专用:一次查询避免每监控一次查询的 N+1。
// limit 由调用方按后台设置「最近状态格数」给定(见 Store.StatusStripRounds);
// 越界或未给时回落默认格数,与列表接口缺省口径一致。
func (s *Store) ListRecentRoundsByMonitors(ctx context.Context, monitorIDs []ID,
	limit int) (map[string][]*RoundLite, error) {
	out := map[string][]*RoundLite{}
	if len(monitorIDs) == 0 {
		return out, nil
	}
	if limit <= 0 || limit > MaxStatusStripRounds {
		limit = DefaultStatusStripRounds
	}
	// 占位符顺序与 SQL 一致:state、各 monitor_id、limit。
	args := make([]any, 0, len(monitorIDs)+2)
	args = append(args, RoundStateOpen)
	for _, id := range monitorIDs {
		args = append(args, id.Hex())
	}
	args = append(args, limit)
	rows, err := s.dbRead.QueryContext(ctx, `SELECT monitor_id, state, success_rate, scheduled_at,
			speed_sum_kbps, speed_count FROM (
			SELECT monitor_id, state, success_rate, scheduled_at, speed_sum_kbps, speed_count,
				ROW_NUMBER() OVER (PARTITION BY monitor_id ORDER BY scheduled_at DESC) AS rn
			FROM rounds WHERE state<>? AND monitor_id IN (`+inPlaceholders(len(monitorIDs))+`)
		) WHERE rn <= ? ORDER BY monitor_id, scheduled_at ASC`,
		args...)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			monitorID ID
			item      RoundLite
			scheduled int64
		)
		if err = rows.Scan(&monitorID, &item.State, &item.SuccessRate, &scheduled,
			&item.SpeedSumKbps, &item.SpeedCount); err != nil {
			return nil, normalizeErr(err)
		}
		item.ScheduledAt = fromUnixSec(scheduled)
		key := monitorID.Hex()
		out[key] = append(out[key], &item)
	}
	return out, normalizeErr(rows.Err())
}

// FindResultsByRound 一轮内各节点的回传结果。
func (s *Store) FindResultsByRound(ctx context.Context, roundID ID) ([]*CheckResult, error) {
	rows, err := s.dbRead.QueryContext(ctx, `SELECT id, round_id, monitor_id, agent_id, ok,
		latency_ms, http_status, error, speed_kbps, late, scheduled_at, created_at
		FROM results WHERE round_id=? ORDER BY agent_id`, roundID)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*CheckResult{}
	for rows.Next() {
		var (
			cr                   CheckResult
			httpStatus           int
			errText              string
			late                 bool
			scheduled, createdAt int64
		)
		if err = rows.Scan(&cr.ID, &cr.RoundID, &cr.MonitorID, &cr.AgentID, &cr.OK,
			&cr.LatencyMs, &httpStatus, &errText, &cr.SpeedKbps, &late,
			&scheduled, &createdAt); err != nil {
			return nil, normalizeErr(err)
		}
		cr.HTTPStatus, cr.Error, cr.Late = httpStatus, errText, late
		cr.ScheduledAt, cr.CreatedAt = fromUnixSec(scheduled), fromUnixSec(createdAt)
		out = append(out, &cr)
	}
	return out, normalizeErr(rows.Err())
}

// CountRoundsWithState 某监控各定稿状态轮次数(总览/详情用)。
func (s *Store) CountRoundsWithState(ctx context.Context, monitorID ID) (map[string]int, error) {
	rows, err := s.dbRead.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM rounds WHERE monitor_id=? GROUP BY state`, monitorID)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err = rows.Scan(&state, &n); err != nil {
			return nil, normalizeErr(err)
		}
		out[state] = n
	}
	return out, normalizeErr(rows.Err())
}
