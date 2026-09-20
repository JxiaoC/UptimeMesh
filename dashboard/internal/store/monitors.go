package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// PushThreshold 是 push(外部上报)监控恒定的告警阈值。
//
// 为什么没有可调空间:push 的每一轮只有一个样本 —— 要么是外部系统的一次上报,
// 要么是静默看门狗补的失败轮 —— 本次成功率非 0% 即 100%。于是阈值在 (0,100]
// 区间里怎么填,判定结果都一模一样;而填成 0 会让 `成功率 < 阈值` 永远为假,
// 即该监控**永不告警**(一个静默的后门,见 .scratch/overview-sort-and-recent-changes)。
// 故表单不给这一项(见 web/src/views/MonitorFormDialog.vue),存储与判定一律固定为
// 本值,语义等价于"这一轮没有成功上报即低于阈值";失联快慢由
// 周期 × 连续轮数 决定(见 CONTEXT.md「外部上报(Push)」)。
const PushThreshold = 100

// Monitor(监控)是核心配置记录,定义见 CONTEXT.md。
// HTTP 探测参数平铺存储;PING 用 TargetHost;TCP 用 TargetHost + Port;
// download(下载速度监控)用 URL 等请求侧参数 + Threshold/SpeedUnit。
type Monitor struct {
	ID      ID     `json:"id"`
	Type    string `json:"type"` // http | ping | tcp | push | download
	Name    string `json:"name"`
	Group   string `json:"group,omitempty"` // 分组标签,空表示未分组
	Enabled bool   `json:"enabled"`
	Period  int    `json:"period"`  // 秒,10~3600
	Timeout int    `json:"timeout"` // 探测超时,秒
	// Threshold 是告警阈值,解释方式随类型而定:
	//   http/ping/tcp ⇒ 本次成功率下限(0~100);
	//   push          ⇒ 恒为 PushThreshold(每轮只有一个样本,填多少都一样);
	//   download      ⇒ 下载速度下限,单位见 SpeedUnit。
	Threshold   float64 `json:"threshold"`
	Consecutive int     `json:"consecutive"` // 连续破线轮数

	// HTTP 探测参数
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// ExpectStatusSpecs 是编辑态(单码或区间,如 "200"、"400~499");
	// ExpectStatusCodes 是其展开后的具体码,供探测判定使用。两者由 API 层保持一致。
	ExpectStatusSpecs []string `json:"expectStatusSpecs,omitempty"`
	ExpectStatusCodes []int    `json:"expectStatusCodes,omitempty"`
	ExpectContains    []string `json:"expectContains,omitempty"`
	ExpectNotContains []string `json:"expectNotContains,omitempty"`
	AllowInsecureTLS  bool     `json:"allowInsecureTLS,omitempty"`
	// InvertMode 反转模式:探测判定取反——探测失败算正常、探测成功算故障
	// (UptimeKuma Upside Down Mode 语义)。只作用于判定层,本次成功率与告警
	// 口径不变;语义与边界见 .scratch/invert-mode/spec.md。
	InvertMode bool `json:"invertMode,omitempty"`
	// IPVersion 是探测使用的 IP 协议族:'' / 'auto' 交给系统(默认),
	// 'ipv4' / 'ipv6' 强制该族;空值按 auto 处理以兼容存量数据。
	// 只作用于节点到目标那一段,不影响节点↔Dashboard 的长连接。
	IPVersion string `json:"ipVersion,omitempty"`
	// JSON 断言(与 UptimeKuma 的 JSON 查询同语义):用 JSONata 表达式从响应中取值,
	// 转字符串后与 JsonAssertExpected 比较。JsonPath 为空表示不做该断言;
	// 求值在 Agent 侧(shared/probe),语义见 .scratch/json-assert/spec.md。
	JsonPath         string `json:"jsonPath,omitempty"`
	JsonPathOperator string `json:"jsonPathOperator,omitempty"`
	JsonAssertExpect string `json:"jsonAssertExpected,omitempty"`

	// PING 探测参数(票 08)
	TargetHost string `json:"targetHost,omitempty"`
	// Port 是 TCP 端口监控的目标端口(tcp 类型必填,1~65535)。
	Port int `json:"port,omitempty"`
	// SpeedUnit 是 download(下载速度监控)的阈值单位:KB/s 或 MB/s;
	// 速度在比较、统计与存储里统一换算为 KB/s,只有录入与展示按该单位换算。
	SpeedUnit string `json:"speedUnit,omitempty"`
	// PushToken 是 push(外部上报)监控的上报令牌:外部系统调用
	// /api/push/{token} 报告状态,令牌即凭据;非 push 类型为空。
	// 只在创建时生成,后续编辑不改动(避免已部署的上报脚本失效)。
	PushToken string `json:"pushToken,omitempty"`
	// LastPushAt 是最近一次外部上报的时间(仅 push 类型维护,由上报端点更新);
	// 调度器据此判定"静默超时"并补失败轮次。零值表示从未上报。
	LastPushAt time.Time `json:"lastPushAt,omitempty"`

	// AssignMode 指派模式(三选一):
	//   AssignModeSelected("" 同样按 selected 处理,兼容存量数据)
	//     仅 AssignedAgentIds 中的节点参与。
	//   AssignModeAll
	//     全部已批准节点参与,建轮时动态解析,后续新增自动纳入。
	//   AssignModeExclude
	//     全部已批准节点中排除 ExcludedAgentIds,其余(含后续新增)参与。
	AssignMode       string   `json:"assignMode,omitempty"`
	AssignedAgentIds []string `json:"assignedAgentIds"`           // selected 模式使用,仅 approved 节点
	ExcludedAgentIds []string `json:"excludedAgentIds,omitempty"` // exclude 模式使用
	ChannelIds       []string `json:"channelIds"`                 // 票 07 起生效

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// 指派模式取值。
const (
	AssignModeSelected = "selected"
	AssignModeAll      = "all"
	AssignModeExclude  = "exclude"
)

const monitorCols = `id, type, name, group_name, enabled, period, timeout, threshold,
	consecutive, url, method, headers, body, expect_status_specs, expect_status_codes,
	expect_contains, expect_not_contains, allow_insecure_tls, invert_mode,
	ip_version, json_path, json_path_operator, json_assert_expect, target_host, port, speed_unit,
	push_token, last_push_at, assign_mode, assigned_agent_ids, excluded_agent_ids,
	channel_ids, created_at, updated_at`

func scanMonitor(row interface{ Scan(...any) error }) (*Monitor, error) {
	var (
		m                                            Monitor
		headers, specs, codes, contains, notContains sql.NullString
		assigned, excluded, channels                 sql.NullString
		enabled, insecure, invert                    bool
		lastPushAt                                   int64
		createdAt, updatedAt                         int64
	)
	err := row.Scan(&m.ID, &m.Type, &m.Name, &m.Group, &enabled, &m.Period, &m.Timeout,
		&m.Threshold, &m.Consecutive, &m.URL, &m.Method, &headers, &m.Body,
		&specs, &codes, &contains, &notContains, &insecure, &invert, &m.IPVersion,
		&m.JsonPath, &m.JsonPathOperator, &m.JsonAssertExpect, &m.TargetHost,
		&m.Port, &m.SpeedUnit, &m.PushToken, &lastPushAt,
		&m.AssignMode, &assigned, &excluded, &channels, &createdAt, &updatedAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	m.Enabled, m.AllowInsecureTLS, m.InvertMode = enabled, insecure, invert
	m.CreatedAt, m.UpdatedAt = fromUnixSec(createdAt), fromUnixSec(updatedAt)
	if lastPushAt > 0 {
		m.LastPushAt = fromUnixSec(lastPushAt)
	}
	for _, item := range []struct {
		raw  sql.NullString
		into any
	}{
		{headers, &m.Headers},
		{specs, &m.ExpectStatusSpecs},
		{codes, &m.ExpectStatusCodes},
		{contains, &m.ExpectContains},
		{notContains, &m.ExpectNotContains},
		{assigned, &m.AssignedAgentIds},
		{excluded, &m.ExcludedAgentIds},
		{channels, &m.ChannelIds},
	} {
		if err = parseJSON(item.raw, item.into); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// monitorArgs 把可变字段展开成 UPDATE 参数(顺序与 UpdateMonitor 的 SET 子句一致)。
// push_token / last_push_at 不在其中:前者只在创建时生成,后者只由上报端点维护,
// 普通编辑(表单整体提交)不得把它们清空。
func monitorArgs(m *Monitor) []any {
	return []any{
		m.Type, m.Name, m.Group, m.Enabled, m.Period, m.Timeout, m.Threshold, m.Consecutive,
		m.URL, m.Method, mustJSON(m.Headers), m.Body,
		mustJSON(m.ExpectStatusSpecs), mustJSON(m.ExpectStatusCodes),
		mustJSON(m.ExpectContains), mustJSON(m.ExpectNotContains), m.AllowInsecureTLS,
		m.InvertMode, m.IPVersion, m.JsonPath, m.JsonPathOperator, m.JsonAssertExpect,
		m.TargetHost, m.Port, m.SpeedUnit, m.AssignMode,
		mustJSON(m.AssignedAgentIds), mustJSON(m.ExcludedAgentIds), mustJSON(m.ChannelIds),
		unixSec(m.UpdatedAt),
	}
}

func (s *Store) InsertMonitor(ctx context.Context, m *Monitor) error {
	now := time.Now()
	if m.ID == "" {
		m.ID = NewID()
	}
	m.CreatedAt, m.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitors (`+monitorCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Type, m.Name, m.Group, m.Enabled, m.Period, m.Timeout, m.Threshold,
		m.Consecutive, m.URL, m.Method, mustJSON(m.Headers), m.Body,
		mustJSON(m.ExpectStatusSpecs), mustJSON(m.ExpectStatusCodes),
		mustJSON(m.ExpectContains), mustJSON(m.ExpectNotContains), m.AllowInsecureTLS,
		m.InvertMode, m.IPVersion, m.JsonPath, m.JsonPathOperator, m.JsonAssertExpect,
		m.TargetHost, m.Port, m.SpeedUnit, m.PushToken, unixSec(m.LastPushAt), m.AssignMode,
		mustJSON(m.AssignedAgentIds), mustJSON(m.ExcludedAgentIds), mustJSON(m.ChannelIds),
		unixSec(m.CreatedAt), unixSec(m.UpdatedAt))
	return normalizeErr(err)
}

// FindMonitorByID 按主键取监控;不存在返回 ErrNotFound。
func (s *Store) FindMonitorByID(ctx context.Context, id ID) (*Monitor, error) {
	row := s.dbRead.QueryRowContext(ctx, `SELECT `+monitorCols+` FROM monitors WHERE id=?`, id)
	return scanMonitor(row)
}

func (s *Store) ListMonitors(ctx context.Context) ([]*Monitor, error) {
	return s.queryMonitors(ctx, `SELECT `+monitorCols+` FROM monitors ORDER BY created_at DESC`)
}

// ListEnabledMonitors 供调度器使用。
func (s *Store) ListEnabledMonitors(ctx context.Context) ([]*Monitor, error) {
	return s.queryMonitors(ctx,
		`SELECT `+monitorCols+` FROM monitors WHERE enabled=1 ORDER BY created_at DESC`)
}

// FindMonitorsByIDs 批量取监控(批量操作后回推每一行的最新状态用)。顺序不保证,
// 不存在的 ID 直接不出现在结果里;空列表返回空切片而不是报错。
func (s *Store) FindMonitorsByIDs(ctx context.Context, ids []ID) ([]*Monitor, error) {
	if len(ids) == 0 {
		return []*Monitor{}, nil
	}
	return s.queryMonitors(ctx,
		`SELECT `+monitorCols+` FROM monitors WHERE id IN (`+inPlaceholders(len(ids))+`)`,
		idArgs(ids)...)
}

func (s *Store) queryMonitors(ctx context.Context, query string, args ...any) ([]*Monitor, error) {
	rows, err := s.dbRead.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, normalizeErr(rows.Err())
}

// UpdateMonitor 整体替换可变字段(ID/createdAt/push_token/last_push_at 不动)。
func (s *Store) UpdateMonitor(ctx context.Context, m *Monitor) error {
	m.UpdatedAt = time.Now()
	args := append(monitorArgs(m), m.ID)
	res, err := s.db.ExecContext(ctx, `UPDATE monitors SET type=?, name=?, group_name=?,
		enabled=?, period=?, timeout=?, threshold=?, consecutive=?, url=?, method=?,
		headers=?, body=?, expect_status_specs=?, expect_status_codes=?, expect_contains=?,
		expect_not_contains=?, allow_insecure_tls=?, invert_mode=?, ip_version=?,
		json_path=?, json_path_operator=?, json_assert_expect=?, target_host=?, port=?,
		speed_unit=?, assign_mode=?, assigned_agent_ids=?, excluded_agent_ids=?, channel_ids=?,
		updated_at=? WHERE id=?`,
		args...)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FindMonitorByPushToken 按上报令牌取 push 监控;未找到返回 ErrNotFound。
func (s *Store) FindMonitorByPushToken(ctx context.Context, token string) (*Monitor, error) {
	row := s.dbRead.QueryRowContext(ctx,
		`SELECT `+monitorCols+` FROM monitors WHERE push_token=? AND push_token<>''`, token)
	return scanMonitor(row)
}

// RecordPushReport 记录一次外部上报的时间(push 监控的静默看门狗以此为基准)。
// 只更新 last_push_at,不动 updated_at(那是配置变更时间)。
func (s *Store) RecordPushReport(ctx context.Context, id ID, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET last_push_at=? WHERE id=?`, unixSec(at), id)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMonitorEnabled 暂停/恢复监控。
func (s *Store) SetMonitorEnabled(ctx context.Context, id ID, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET enabled=?, updated_at=? WHERE id=?`,
		enabled, unixSec(time.Now()), id)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMonitor 删除监控记录本身(级联清理由 DeleteMonitorCascade 负责)。
func (s *Store) DeleteMonitor(ctx context.Context, id ID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM monitors WHERE id=?`, id)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// idArgs 把主键列表展开成 SQL 参数(监控相关的表主键都是 24 位 hex 文本)。
func idArgs(ids []ID) []any {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// SetMonitorsEnabled 批量暂停/恢复(一条 UPDATE ... WHERE id IN);返回实际命中的监控数。
// 与 SetMonitorEnabled 同语义,只是把"逐个调用"的循环留在数据库里:200+ 监控时前端
// 逐行调用是 200 个请求、200 个单行事务,而这里是一条语句。
func (s *Store) SetMonitorsEnabled(ctx context.Context, ids []ID, enabled bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]any{enabled, unixSec(time.Now())}, idArgs(ids)...)
	res, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET enabled=?, updated_at=? WHERE id IN (`+inPlaceholders(len(ids))+`)`,
		args...)
	if err != nil {
		return 0, normalizeErr(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteMonitorCascade 删除监控并在同一事务内清理其轮次、结果、小时聚合、告警状态
// 与状态变动记录,避免统计口径残留(票 05 后有数据)。返回 nil 表示监控不存在。
func (s *Store) DeleteMonitorCascade(ctx context.Context, id ID) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM monitors WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM rounds WHERE monitor_id=?`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM results WHERE monitor_id=?`, id); err != nil {
			return err
		}
		// 小时聚合按监控一并清掉:它唯一的数据源就是本监控的轮次,监控没了就是
		// 永远读不到的孤儿行(固定保留 31 天虽能兜底回收,但没必要留这 31 天)。
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM hourly_stats WHERE monitor_id=?`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM monitor_state_changes WHERE monitor_id=?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM monitor_states WHERE id=?`, id)
		return err
	})
}

// DeleteMonitorsCascade 批量级联删除(单事务):监控本身连同轮次、结果、小时聚合、
// 告警状态与状态变动记录一起清掉,返回实际删除的监控数。与 DeleteMonitorCascade
// 的差别是"不存在的 ID 不算失败"——批量删除对"选中的监控刚被别处删掉"应当是幂等的,
// affected 如实回报即可。
func (s *Store) DeleteMonitorsCascade(ctx context.Context, ids []ID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := idArgs(ids)
	placeholders := inPlaceholders(len(ids))
	var deleted int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM monitors WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return err
		}
		deleted, _ = res.RowsAffected()
		for _, table := range []string{"rounds", "results", "hourly_stats"} {
			if _, err = tx.ExecContext(ctx,
				`DELETE FROM `+table+` WHERE monitor_id IN (`+placeholders+`)`, args...); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM monitor_state_changes WHERE monitor_id IN (`+placeholders+`)`,
			args...); err != nil {
			return err
		}
		// monitor_states 的主键就是监控的 24 位 hex(见 monitor_states.go)。
		_, err = tx.ExecContext(ctx,
			`DELETE FROM monitor_states WHERE id IN (`+placeholders+`)`, args...)
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// AttachChannelToMonitors 把某渠道加到所有监控的通知渠道里(已包含的跳过),返回**实际
// 改动**的监控(完整行,调用方要拿去推送/回显)与监控总数。与 DetachChannelFromMonitors
// 对称:删渠道时逐个摘除,这里是"设为所有监控的渠道"。
//
// 为什么是"追加"而不是"改成只有这一个":替换会静默清掉每个监控原有的其它渠道,一次点击
// 丢掉一批告警路由是灾难性的。要改某个监控的勾选,去监控弹窗里改。
//
// 暂停的监控同样算"所有监控"(它们恢复后照样要告警),不额外过滤。
func (s *Store) AttachChannelToMonitors(ctx context.Context, channelID ID) (
	changed []*Monitor, total int, err error) {
	rows, err := s.dbRead.QueryContext(ctx, `SELECT id, channel_ids FROM monitors`)
	if err != nil {
		return nil, 0, normalizeErr(err)
	}
	type pending struct {
		id       ID
		channels []string
	}
	var todo []pending
	for rows.Next() {
		var (
			id  ID
			raw sql.NullString
		)
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, 0, normalizeErr(err)
		}
		total++
		var ids []string
		if err = parseJSON(raw, &ids); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if hasString(ids, channelID.Hex()) {
			continue // 已经有这个渠道:不动它,免得把 updated_at 也刷一遍
		}
		// 显式拷贝再追加:parseJSON 出来的切片容量可能有余量,直接 append 会让两个
		// pending 共享底层数组。
		todo = append(todo, pending{id: id, channels: append(append([]string{}, ids...), channelID.Hex())})
	}
	rows.Close()
	if err = normalizeErr(rows.Err()); err != nil {
		return nil, total, err
	}
	if len(todo) == 0 {
		return nil, total, nil
	}
	now := time.Now()
	ids := make([]ID, 0, len(todo))
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		for _, item := range todo {
			raw, merr := json.Marshal(item.channels)
			if merr != nil {
				return merr
			}
			if _, eerr := tx.ExecContext(ctx,
				`UPDATE monitors SET channel_ids=?, updated_at=? WHERE id=?`,
				string(raw), unixSec(now), item.id); eerr != nil {
				return eerr
			}
			ids = append(ids, item.id)
		}
		return nil
	})
	if err != nil {
		return nil, total, normalizeErr(err)
	}
	changed, err = s.FindMonitorsByIDs(ctx, ids)
	if err != nil {
		return nil, total, err
	}
	return changed, total, nil
}

// hasString 是切片里是否含某值的小工具(渠道/节点 ID 列表都用字符串数组存)。
func hasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// DetachChannelFromMonitors 从所有监控的勾选里摘除某渠道,避免悬空引用。
func (s *Store) DetachChannelFromMonitors(ctx context.Context, channelID ID) error {
	rows, err := s.dbRead.QueryContext(ctx, `SELECT id, channel_ids FROM monitors`)
	if err != nil {
		return normalizeErr(err)
	}
	type pending struct {
		id       ID
		channels []string
	}
	var todo []pending
	for rows.Next() {
		var (
			id  ID
			raw sql.NullString
		)
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return normalizeErr(err)
		}
		var ids []string
		if err = parseJSON(raw, &ids); err != nil {
			rows.Close()
			return err
		}
		kept := make([]string, 0, len(ids))
		changed := false
		for _, v := range ids {
			if v == channelID.Hex() {
				changed = true
				continue
			}
			kept = append(kept, v)
		}
		if changed {
			todo = append(todo, pending{id: id, channels: kept})
		}
	}
	rows.Close()
	if err = normalizeErr(rows.Err()); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, item := range todo {
			var raw []byte
			if raw, err = json.Marshal(item.channels); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx,
				`UPDATE monitors SET channel_ids=?, updated_at=? WHERE id=?`,
				string(raw), unixSec(time.Now()), item.id); err != nil {
				return err
			}
		}
		return nil
	})
}
