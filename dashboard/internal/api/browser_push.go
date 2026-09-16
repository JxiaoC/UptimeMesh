package api

import (
	"context"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/alert"
	"github.com/uptimemesh/dashboard/internal/scheduler"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
	"github.com/uptimemesh/shared/checkconfig"
)

// 缺样节点在展示里的"原因":活着却没回结果 vs 探活无响应 vs 任务没派出去。
// 文案与轮次定稿的探活决策表一一对应(见 scheduler.finalize),初始拉取与实时推送
// 共用同一份字面量。
const (
	errMissingAlive = "超时(节点在线)"
	errMissingDead  = "节点离线"
	// errMissingUndispatched 是"建轮与重连补发时它都不在线,任务从未交出去":
	// 与离线同"不计分母",但责任在派发侧 —— 重启后首轮那批假 DOWN 就是这个状态
	// (见 .scratch/restart-first-round/spec.md)。
	errMissingUndispatched = "未派发任务"
)

// agentTiles 拼出卡片上的节点明细:按监控配置的指派顺序,只列配置里有的节点。
//
// 为什么顺序以配置为准:轮次里的节点是**建轮时**解析出来的(指派模式为"所有节点 / 排除
// 节点"时会与配置里的两个列表都不一样),而卡片要回答的是"我配的这些节点现在怎么样",
// 所以两边都按配置的列表取齐(见 .scratch/overview-websocket/spec.md)。
//
// byAgent 是回传样本(实时推送取内存里的聚合、初始拉取取结果表),三张名单是缺样;
// 推送与 GET /overview 共用本函数,顺序与文案因此不会漂移。
func agentTiles(assigned []string, byAgent map[string]webhub.AgentTile,
	missingAlive, missingDead, missingUndispatched []string) []webhub.AgentTile {
	alive := make(map[string]bool, len(missingAlive))
	for _, id := range missingAlive {
		alive[id] = true
	}
	dead := make(map[string]bool, len(missingDead))
	for _, id := range missingDead {
		dead[id] = true
	}
	undispatched := make(map[string]bool, len(missingUndispatched))
	for _, id := range missingUndispatched {
		undispatched[id] = true
	}
	out := make([]webhub.AgentTile, 0, len(assigned))
	for _, id := range assigned {
		if tile, reported := byAgent[id]; reported {
			out = append(out, tile)
			continue
		}
		switch {
		case alive[id]:
			out = append(out, webhub.AgentTile{AgentID: id, Error: errMissingAlive})
		case dead[id]:
			out = append(out, webhub.AgentTile{AgentID: id, Error: errMissingDead})
		case undispatched[id]:
			out = append(out, webhub.AgentTile{AgentID: id, Error: errMissingUndispatched})
		}
	}
	return out
}

// sampleTiles 把聚合里的样本视图转成按 agentId 索引的节点明细(推送用)。
func sampleTiles(samples []scheduler.AgentSample) map[string]webhub.AgentTile {
	byAgent := make(map[string]webhub.AgentTile, len(samples))
	for _, s := range samples {
		byAgent[s.AgentID] = webhub.AgentTile{
			AgentID: s.AgentID, OK: s.OK, LatencyMs: round2(s.LatencyMs), Error: s.Error,
		}
	}
	return byAgent
}

// avgLatencyMs 本轮平均延时(没有可用样本时为 0),与 GET /overview 的 last 口径一致。
func avgLatencyMs(agg scheduler.Aggregation) float64 {
	if agg.LatencyCount <= 0 {
		return 0
	}
	return round2(agg.LatencySumMs / float64(agg.LatencyCount))
}

// 浏览器实时推送的载荷组装集中在这里:main.go 的调度回调与各监控接口共用,
// 保证"初始拉取"和"增量推送"两边的字段口径一致(见 docs/protocol.md)。
//
// 为什么要推这么全:列表页在 200+ 监控时,如果每个轮次定稿都去整表拉一次
// /monitors(每个监控还带 100 轮状态条),流量与渲染都会爆;推送里直接带上
// "这一轮的状态条单元 + 该监控最新状态",前端就能就地更新一行。

// broadcast 是空实现安全的包装:测试与部分装配路径里可能没有 webhub。
func (a *API) broadcast(ev webhub.Event) {
	if a.Web == nil {
		return
	}
	a.Web.Broadcast(ev)
}

// BroadcastRoundFinalized 推送一轮定稿:列表页据此更新该监控的状态色,并把这一轮
// 追加到它的状态条上;总览页据此更新一张卡片(状态、最新成功率/速度、最新延时、
// 节点明细);详情页也可据此刷新。
//
// 整份 Aggregation 直接带进来(而不是挑几个字段传):判定值(MetricValue)、本次成功率与
// 平均下载速度都要进载荷,而阈值口径随监控类型而变(成功率 vs 速度),在这里重算一遍
// 迟早会和调度器漂移。总览卡片要的延时与节点明细同样直接从聚合取(它在定稿时已经算过),
// 广播路径因此不产生任何额外查询。
func (a *API) BroadcastRoundFinalized(_ context.Context, m *store.Monitor,
	agg scheduler.Aggregation, st *store.MonitorState) {
	// 未经状态机的监控(理论上不会发生)按初始态回填;色块的红/黄也由它决定。
	alertState := store.MonitorUP
	if st != nil {
		alertState = st.AlertState
	}
	data := webhub.RoundFinalizedData{
		MonitorID:   m.ID.Hex(),
		RoundID:     agg.RoundID,
		State:       string(agg.State),
		SuccessRate: agg.SuccessRate,
		SpeedKbps:   round2(agg.AvgSpeedKbps()),
		ScheduledAt: fmtTime(agg.ScheduledAt),
		// 这一轮定稿之后的告警态决定色块:破线且还没判 DOWN → 黄,已判 DOWN → 红。
		RoundStatus:  webhub.RoundCellStatus(string(agg.State), agg.MetricValue, thresholdOf(m), alertState),
		DisplayState: DisplayState(m, st),
		AlertState:   alertState,
		// 总览卡片的「最新延时」与「节点明细」:后者的顺序与文案由 agentTiles 统一,
		// 与 GET /overview 的 agents 逐字段一致(见该函数的说明)。
		LatencyMs: avgLatencyMs(agg),
		Agents: agentTiles(m.AssignedAgentIds, sampleTiles(agg.Samples),
			agg.MissingAlive, agg.MissingDead, agg.MissingUndispatched),
	}
	if st != nil {
		data.Consecutive = st.Consecutive
	}
	a.broadcast(webhub.Event{Type: webhub.EvRoundFinalized, Data: data})
}

// BroadcastMonitorFlipped 推送告警状态翻转:载荷带够列表页就地更新所需的状态,
// 以及这次翻转留下的那条状态变动记录(总览页的变动流水直接插一行,不必再拉接口)。
//
// ev 由调度器在落完变动记录后回填身份与时间(RoundID/FromState/ChangedAt);
// 判定值按监控类型展示,故速度字段随监控配置一起带出。
func (a *API) BroadcastMonitorFlipped(m *store.Monitor, ev *alert.Event,
	agg scheduler.Aggregation, st *store.MonitorState) {
	if ev == nil {
		return
	}
	data := webhub.MonitorFlippedData{
		MonitorID: m.ID.Hex(), Name: m.Name,
		AlertState: ev.Type, SuccessRate: ev.SuccessRate,
		DisplayState: DisplayState(m, st),
		RoundID:      ev.RoundID,
		FromState:    ev.FromState,
		// 变动时间必须与 /state-changes 那一行**逐字一致**:两处都走 fmtTime,
		// 由它统一转本地时区渲染(库里存的仍是 UTC 秒)。不要再在这里 .UTC() ——
		// 那样推送与接口会差一个时区。
		ChangedAt: fmtTime(ev.ChangedAt),
		// 报警持续时长:恢复那一条才有值(调度器落记录时回查上一条报错记录算出),
		// 与 GET /state-changes 的同名字段逐项一致 —— 总览流水的「恢复」行旁据此
		// 多显示一张时长卡,推送与快照两条来源不能有歧义。
		DurationSec: ev.DurationSec,
		Type:        m.Type,
		SpeedUnit:   m.SpeedUnit,
		SpeedKbps:   round2(agg.AvgSpeedKbps()),
	}
	if st != nil {
		data.Consecutive = st.Consecutive
	}
	a.broadcast(webhub.Event{Type: webhub.EvMonitorFlipped, Data: data})
}

// monitorRowView 组装"列表页的一行":monitorView + 列表展示所需的状态字段。
// GET /monitors 之外的三个出口(单行推送、批量推送、批量操作的响应体)共用它,
// 保证各处字段口径一致。withStrip 为 false 时不带 recentRounds —— 调用方要么会自己
// 保留前端已有的色块,要么本来就不需要(例如批量启停)。
func (a *API) monitorRowView(ctx context.Context, m *store.Monitor, withStrip bool) g.Map {
	st, _, err := a.Store.GetMonitorState(ctx, m.ID)
	if err != nil {
		st = nil
	}
	view := monitorView(m)
	view["displayState"] = DisplayState(m, st)
	if st != nil {
		view["alertState"] = st.AlertState
		view["consecutiveBreaches"] = st.Consecutive
	} else {
		view["alertState"] = store.MonitorUP
		view["consecutiveBreaches"] = 0
	}
	if withStrip {
		// 格数与列表接口同源(后台设置「最近状态格数」,见 api.stripRoundsFor):
		// 两处给不同的格数会让页面上的色块在保存后突然变多。
		if recent, err := a.Store.ListRecentRoundsByMonitors(ctx, []store.ID{m.ID},
			a.stripRoundsFor(ctx, 0)); err == nil {
			view["recentRounds"] = roundStatusStrip(recent[m.ID.Hex()], m, st)
		}
	}
	return view
}

// thresholdOf 返回监控阈值的**比较单位**值:普通监控就是百分比阈值;
// 下载速度监控换算成 KB/s —— 与本次平均速度同单位,状态条与状态机才能直接比大小;
// push 恒为 store.PushThreshold(存量库里若还留着 0,展示口径不能跟着退化成"永不破线")。
func thresholdOf(m *store.Monitor) float64 {
	switch m.Type {
	case checkconfig.TypeDownload:
		return checkconfig.ToKbps(m.Threshold, m.SpeedUnit)
	case checkconfig.TypePush:
		return store.PushThreshold
	}
	return m.Threshold
}

// BroadcastMonitorChanged 推送"某个监控的配置/启用状态变了":载荷与 GET /monitors
// 里的一行同构(含状态条),列表页直接替换/插入该行即可。
func (a *API) BroadcastMonitorChanged(ctx context.Context, m *store.Monitor) {
	a.broadcast(webhub.Event{Type: webhub.EvMonitorChanged, Data: a.monitorRowView(ctx, m, true)})
}

// BroadcastMonitorsChanged 推送一批监控的启用状态变化(列表页多选后暂停/恢复):
// 一批只发一帧 EvMonitorsChanged,载荷是若干**轻量行**(不含 recentRounds)。
// 同时把这份轻量行返回给调用方,批量操作的响应体直接用它,发起方不必等推送。
func (a *API) BroadcastMonitorsChanged(ctx context.Context, list []*store.Monitor) []map[string]any {
	rows := make([]map[string]any, 0, len(list))
	for _, m := range list {
		row := a.monitorRowView(ctx, m, false)
		rows = append(rows, row)
	}
	if len(rows) > 0 {
		a.broadcast(webhub.Event{
			Type: webhub.EvMonitorsChanged,
			Data: webhub.MonitorsChangedData{Monitors: rows},
		})
	}
	return rows
}

// BroadcastMonitorDeleted 推送"某个监控被删了"。
func (a *API) BroadcastMonitorDeleted(id string) {
	a.broadcast(webhub.Event{Type: webhub.EvMonitorDeleted, Data: map[string]string{"monitorId": id}})
}

// BroadcastMonitorsDeleted 推送一批监控被删除(列表页多选后删除):一批只发一帧。
func (a *API) BroadcastMonitorsDeleted(ids []string) {
	if len(ids) == 0 {
		return
	}
	a.broadcast(webhub.Event{
		Type: webhub.EvMonitorsDeleted,
		Data: webhub.MonitorsDeletedData{MonitorIDs: ids},
	})
}
