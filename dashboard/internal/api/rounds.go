package api

import (
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/store"
)

// registerRoundRoutes 轮次查询(票 05:详情页时间线;统计接口在票 09)
// 与状态变动记录(详情页另一半:只在 UP↔DOWN 翻转时才有行)。
func (a *API) registerRoundRoutes(group *ghttp.RouterGroup) {
	group.GET("/monitors/{id}/rounds", a.listMonitorRounds)
	group.GET("/monitors/{id}/state-changes", a.listMonitorStateChanges)
	// 全局变动流水(总览页的「最近状态变动记录」):跨所有监控,与上面按监控查的那条
	// 是两个接口 —— 载荷口径不同(见 listStateChanges 的说明)。
	group.GET("/state-changes", a.listStateChanges)
	group.GET("/rounds/{id}/results", a.listRoundResults)
}

// parseHexID 从路径参数解析记录主键。
func parseHexID(r *ghttp.Request, name string) (store.ID, bool) {
	id, err := store.IDFromHex(r.Get(name).String())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "ID 非法"})
		return "", false
	}
	return id, true
}

// fmtTime 把时间渲染成接口/推送统一的展示文本(精确到秒)。
//
// 一律先转**服务端本地时区**再格式化:库里存的是 UTC Unix 秒(store.unixSec),
// 读回来也是 UTC 的 time.Time,直接 Format 出来就是 UTC 墙钟 —— 部署在 +8 时区
// 就比用户手表差 8 小时(这正是容器时区那次的连带问题)。
//
// 展示口径只有这一处:轮次时间线、状态变动记录、节点首次/最近上线、监控创建时间、
// 以及 WebSocket 推送里的时间,全部走 fmtTime,不能有的地方 UTC 有的地方本地
// (推送曾单独按 UTC 渲染,和详情页对齐过一次,见 browser_push.go)。
// 趋势图的桶标签另走 bucketLabel(),它同样先转本地。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func resultViews(results []*store.CheckResult) []g.Map {
	out := make([]g.Map, 0, len(results))
	for _, res := range results {
		out = append(out, g.Map{
			"agentId": res.AgentID.Hex(), "ok": res.OK, "late": res.Late,
			"latencyMs": res.LatencyMs, "httpStatus": res.HTTPStatus,
			"error": res.Error, "createdAt": fmtTime(res.CreatedAt),
			"speedKbps": round2(res.SpeedKbps),
		})
	}
	return out
}

// roundView 把一轮(聚合 + 各节点结果)组装成详情页时间线的一行。
// 「最近轮次时间线」与「最近状态变动记录」共用它:后者只是同一行多带几个变动字段,
// 两块面板的展示口径因此不会漂移(见 .scratch/state-change-history/spec.md)。
func roundView(rd *store.Round, results []*store.CheckResult) g.Map {
	return g.Map{
		"id": rd.ID.Hex(), "state": rd.State,
		"scheduledAt": fmtTime(rd.ScheduledAt), "closedAt": fmtTime(rd.ClosedAt),
		"success": rd.Success, "valid": rd.Valid,
		"totalAgents": rd.TotalAgents, "successRate": rd.SuccessRate,
		// 下载速度监控:本轮平均速度(KB/s,前端按监控的 speedUnit 换算展示)。
		"avgSpeedKbps": round2(rd.AvgSpeedKbps()),
		"missingAlive": rd.MissingAlive, "missingDead": rd.MissingDead,
		// 任务没交到节点手上的那些(与离线同"不计分母",但成因在派发侧):
		// 详情页据此显示「未派发任务」而不是「离线缺样」。
		"missingUndispatched": rd.MissingUndispatched,
		"results":             resultViews(results),
	}
}

// listMonitorRounds 监控详情的轮次时间线:每轮聚合 + 各节点结果一次带出。
func (a *API) listMonitorRounds(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	rounds, err := a.Store.ListRoundsByMonitor(r.Context(), id, r.Get("limit").Int())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(rounds))
	for _, rd := range rounds {
		results, _ := a.Store.FindResultsByRound(r.Context(), rd.ID)
		out = append(out, roundView(rd, results))
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// listMonitorStateChanges 监控详情的状态变动记录(新→旧,仅 UP↔DOWN 翻转)。
// 每行的展示内容取自触发翻转的那一轮(状态/成功率/节点明细),时间列用变动时间
// (`changedAt`,该轮定稿、状态机真正翻转的时刻)而不是该轮的计划时间 —— 后者是
// 轮次时间线那一列的口径,两者相差"探测超时 + 宽限期 + 探活 + 一个 tick"。
// 载荷因此同时带出 scheduledAt(轮次口径)与 changedAt(变动口径),展示哪一个由前端定。
// 触发轮次查不到(理论上不该发生:级联删除会把两者一起清掉)时跳过该条 ——
// 与其显示半条,不如不显示。
func (a *API) listMonitorStateChanges(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	changes, err := a.Store.ListStateChangesByMonitor(r.Context(), id, r.Get("limit").Int())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(changes))
	for _, sc := range changes {
		rd, err := a.Store.FindRoundByID(r.Context(), sc.RoundID)
		if err != nil {
			continue
		}
		results, _ := a.Store.FindResultsByRound(r.Context(), rd.ID)
		view := roundView(rd, results)
		view["id"] = sc.ID.Hex() // 行主键是变动记录,轮次 ID 单独用 roundId 带出
		view["roundId"] = sc.RoundID.Hex()
		view["fromState"] = sc.FromState
		view["toState"] = sc.ToState
		view["changedAt"] = fmtTime(sc.ChangedAt)
		// 报警持续时长(秒):只有恢复(DOWN→UP)那条有值,前端据此在恢复卡片旁
		// 多显示一张时长卡。0 = 没有可配对的报错记录 ⇒ 前端不显示,不把 0 当"瞬间恢复"。
		view["durationSec"] = sc.DurationSec
		out = append(out, view)
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// listStateChanges 全局最近的状态变动记录(新→旧),总览页的「最近状态变动记录」用。
//
// 与 listMonitorStateChanges 的载荷刻意不同:那张表是**一个监控的排障视图**
// (轮次行 + 节点明细),这张是**跨监控的变更流水** —— 一行只回答"谁、什么时候、
// 从什么变成什么、当时成功率多少"。20 行各回一趟轮次结果(每行再查节点明细)
// 只会放大响应体,而节点明细在这里没有阅读场景。因此直接读变动表自身的字段。
//
// 监控名一次批量关联,不是 N+1;监控已删(级联删除本应把记录一起清掉)的行跳过 ——
// 与详情页「宁可少一条,也不显示半条」同款。
func (a *API) listStateChanges(r *ghttp.Request) {
	changes, err := a.Store.ListStateChanges(r.Context(), r.Get("limit").Int())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	ids := make([]store.ID, 0, len(changes))
	for _, sc := range changes {
		ids = append(ids, sc.MonitorID)
	}
	monitors, _ := a.Store.FindMonitorsByIDs(r.Context(), ids)
	byID := make(map[string]*store.Monitor, len(monitors))
	for _, m := range monitors {
		byID[m.ID.Hex()] = m
	}
	out := make([]g.Map, 0, len(changes))
	for _, sc := range changes {
		m, found := byID[sc.MonitorID.Hex()]
		if !found {
			continue
		}
		// 下载速度监控这一行展示速度而不是成功率:单位随监控配置,前端据此换算。
		out = append(out, g.Map{
			"id": sc.ID.Hex(), "monitorId": sc.MonitorID.Hex(), "monitorName": m.Name,
			"monitorType": m.Type, "speedUnit": m.SpeedUnit,
			"roundId": sc.RoundID.Hex(), "fromState": sc.FromState, "toState": sc.ToState,
			"successRate": round2(sc.SuccessRate), "speedKbps": round2(sc.SpeedKbps),
			// 报警持续时长(秒):恢复那条才非 0,前端在「恢复」标签旁多显示一张时长卡。
			"durationSec": sc.DurationSec,
			"changedAt":   fmtTime(sc.ChangedAt),
		})
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// listRoundResults 单轮原始结果(排障用)。
func (a *API) listRoundResults(r *ghttp.Request) {
	rid, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	results, err := a.Store.FindResultsByRound(r.Context(), rid)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": resultViews(results)})
}
