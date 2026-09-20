package api

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
	"github.com/uptimemesh/shared/checkconfig"
)

// registerStatsRoutes 统计接口(票 09):总览 + 详情趋势。
func (a *API) registerStatsRoutes(group *ghttp.RouterGroup) {
	group.GET("/overview", a.overview)
	group.GET("/monitors/{id}/stats", a.monitorStats)
	group.GET("/monitors/{id}/agent-latency", a.monitorAgentLatency)
}

// statsWindow 解析详情页旧窗口参数,默认 24h。
func statsWindow(r *ghttp.Request) time.Duration {
	switch r.Get("window").String() {
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// 详情趋势的颗粒度白名单与「跨度决定默认/下限」规则:
//   - 跨度 ≤ 24 小时:默认 5 分钟,最低 1 分钟;
//   - 24 小时 < 跨度 ≤ 3 天:默认 30 分钟,最低 30 分钟;
//   - 跨度 > 3 天:默认 1 小时,最低 1 小时。
//
// 默认值与下限分开:1 分钟仍可手动选择(轮次密集时看细粒度),但默认取 5 分钟,
// 24 小时窗口正好 288 个点,趋势图更易读。
var statsBucketOptions = []time.Duration{
	time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour, 24 * time.Hour,
}

// statsMinBucket 该时间跨度允许的最小颗粒度。
func statsMinBucket(span time.Duration) time.Duration {
	switch {
	case span <= 24*time.Hour:
		return time.Minute
	case span <= 3*24*time.Hour:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

// statsDefaultBucket 未显式指定颗粒度时的默认值,与详情页的默认选择一致。
func statsDefaultBucket(span time.Duration) time.Duration {
	if span <= 24*time.Hour {
		return 5 * time.Minute
	}
	return statsMinBucket(span)
}

// bucketToken 颗粒度转可读 token(1m/5m/30m/1h/1d),用于错误提示。
func bucketToken(bucket time.Duration) string {
	switch bucket {
	case time.Minute:
		return "1m"
	case 5 * time.Minute:
		return "5m"
	case 30 * time.Minute:
		return "30m"
	case time.Hour:
		return "1h"
	case 24 * time.Hour:
		return "1d"
	default:
		return bucket.String()
	}
}

// parseBucket token 解析:接受 1m/5m/30m/1h/1d 与纯秒数。
func parseBucket(raw string) (time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1m":
		return time.Minute, true
	case "5m":
		return 5 * time.Minute, true
	case "30m":
		return 30 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "1d":
		return 24 * time.Hour, true
	}
	sec, err := time.ParseDuration(raw + "s")
	if err != nil || sec <= 0 {
		return 0, false
	}
	for _, opt := range statsBucketOptions {
		if opt == sec {
			return sec, true
		}
	}
	return 0, false
}

// statsQuery 解析详情趋势的时间区间与颗粒度:from/to 为 UTC Unix 秒,缺省时
// 回落到旧参数 window=24h|7d|30d(换算为 [now-window, now],便于兼容与脚本调用)。
// 颗粒度缺省取该跨度的默认值(≤24h 为 5 分钟);显式给出时校验白名单与跨度下限。
// 返回 ok=false 时已写好错误响应。
func statsQuery(r *ghttp.Request) (from, to time.Time, bucket time.Duration, ok bool) {
	fromSec, toSec := r.Get("from").Int64(), r.Get("to").Int64()
	if fromSec == 0 || toSec == 0 {
		to = time.Now()
		from = to.Add(-statsWindow(r))
	} else {
		from, to = time.Unix(fromSec, 0), time.Unix(toSec, 0)
	}
	if !to.After(from) {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "时间范围不合法:结束时间须晚于开始时间"})
		return from, to, 0, false
	}

	minBucket := statsMinBucket(to.Sub(from))
	raw := r.Get("bucket").String()
	if strings.TrimSpace(raw) == "" {
		return from, to, statsDefaultBucket(to.Sub(from)), true
	}
	got, valid := parseBucket(raw)
	if !valid {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "颗粒度不合法,可选 1m/5m/30m/1h/1d"})
		return from, to, 0, false
	}
	if got < minBucket {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "该时间跨度不允许小于 " +
			bucketToken(minBucket) + " 的颗粒度"})
		return from, to, 0, false
	}
	return from, to, got, true
}

// bucketLabel 桶起点转展示文本:天桶只到日期,小时桶到小时,更细的到分钟。
func bucketLabel(at time.Time, bucket time.Duration) string {
	local := at.Local()
	switch {
	case bucket >= 24*time.Hour:
		return local.Format("2006-01-02")
	case bucket >= time.Hour:
		return local.Format("2006-01-02 15:00")
	default:
		return local.Format("2006-01-02 15:04")
	}
}

// overviewAvailabilityWindows 总览卡片展示的可用率窗口(24h/7d/30d)。
var overviewAvailabilityWindows = []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

// availabilityValue 取指定窗口的可用率展示值:窗口内无样本返回 nil,前端显示「—」。
func availabilityValue(list []store.AvailabilityWindow, window time.Duration) any {
	for _, w := range list {
		if w.Window == window && w.OK {
			return round2(w.Rate)
		}
	}
	return nil
}

// overview 每监控:展示状态、最近轮、24h/7d/30d 可用率、各节点最新结果。
//
// 暂停的监控不在总览里出卡片:它不产生新轮次,卡片上的状态与可用率只会是停摆前的
// 旧值,查看/恢复它的地方是「监控」页。但顶部汇总仍要提示「已暂停 N 个」,所以暂停
// 监控照样返回一行身份信息——只有 id/名称/类型/分组/周期与 PAUSED 状态,不带任何
// 统计,因而也不必为它查最近轮、小时聚合与节点明细;前端按 enabled 过滤并计数。
func (a *API) overview(r *ghttp.Request) {
	monitors, err := a.Store.ListMonitors(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	// 只取启用中监控的告警状态行:暂停的那些连状态都用不上(DisplayState 直接给 PAUSED)。
	ids := make([]string, 0, len(monitors))
	for _, m := range monitors {
		if m.Enabled {
			ids = append(ids, m.ID.Hex())
		}
	}
	states := a.Store.GetMonitorStatesByIDs(r.Context(), ids)

	out := make([]g.Map, 0, len(monitors))
	for _, m := range monitors {
		v := g.Map{
			"id": m.ID.Hex(), "name": m.Name, "type": m.Type, "group": m.Group,
			"enabled": m.Enabled, "url": urlOf(m), "period": m.Period,
			// threshold 是卡片排序的判据:最近轮判定值低于它 = 「低于阈值」(黄色预警),
			// 与状态条同一口径。前端不重新发明阈值语义,只做 lastValue < threshold 比较。
			"threshold":       m.Threshold,
			"displayState":    DisplayState(m, states[m.ID.Hex()]),
			"latencyMs":       0.0,
			"availability24h": nil,
			"availability7d":  nil,
			"availability30d": nil,
			"agents":          []webhub.AgentTile{},
		}
		// 下载速度监控:阈值单位、比较单位的阈值(KB/s)与最新速度随行带出,
		// 前端才能把卡片第 4 格显示成「最新速度」并按速度阈值分档。
		if m.Type == checkconfig.TypeDownload {
			v["speedUnit"] = m.SpeedUnit
			v["thresholdValue"] = round2(checkconfig.FromKbps(thresholdOf(m), m.SpeedUnit))
		}
		if !m.Enabled {
			out = append(out, v)
			continue
		}
		last, hasLast, _ := a.Store.FindLatestClosed(r.Context(), m.ID)
		// 三个窗口共用一次小时聚合查询。
		avails, _ := a.Store.Availabilities(r.Context(), m.ID, overviewAvailabilityWindows...)
		v["availability24h"] = availabilityValue(avails, 24*time.Hour)
		v["availability7d"] = availabilityValue(avails, 7*24*time.Hour)
		v["availability30d"] = availabilityValue(avails, 30*24*time.Hour)
		v["agents"] = a.agentSummary(r, m, last)
		if hasLast && last != nil {
			v["lastSuccessRate"] = round2(last.SuccessRate)
			v["lastRoundState"] = last.State
			v["lastRoundAt"] = fmtTime(last.ScheduledAt)
			if m.Type == checkconfig.TypeDownload {
				// 速度按监控配置的单位展示(存储与比较一律 KB/s)。
				v["lastSpeedKbps"] = round2(last.AvgSpeedKbps())
				v["lastSpeed"] = round2(checkconfig.FromKbps(last.AvgSpeedKbps(), m.SpeedUnit))
			}
			if last.LatencyCount > 0 {
				v["latencyMs"] = round2(last.LatencySumMs / float64(last.LatencyCount))
			}
		}
		out = append(out, v)
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// agentSummary 最近定稿轮里各指派节点的展示:ok/latency/错误,或离线缺样。
// 顺序与缺样文案由 agentTiles 统一(实时推送共用同一个函数),这里只负责从结果表
// (含晚到结果)取回传样本 —— 两处口径因此不会漂移。
func (a *API) agentSummary(r *ghttp.Request, m *store.Monitor, last *store.Round) []webhub.AgentTile {
	if last == nil {
		return []webhub.AgentTile{}
	}
	results, _ := a.Store.FindResultsByRound(r.Context(), last.ID)
	byAgent := make(map[string]webhub.AgentTile, len(results))
	for _, res := range results {
		byAgent[res.AgentID.Hex()] = webhub.AgentTile{
			AgentID: res.AgentID.Hex(), OK: res.OK,
			LatencyMs: round2(res.LatencyMs), Error: res.Error,
		}
	}
	return agentTiles(m.AssignedAgentIds, byAgent,
		last.MissingAlive, last.MissingDead, last.MissingUndispatched)
}

// monitorStats 详情趋势图:任意时间段 + 颗粒度(1m/5m/30m/1h/1d)的桶序列。
// 参数:from/to(UTC Unix 秒)+ bucket(1m|5m|30m|1h|1d,或秒数);缺省时兼容旧
// window=24h|7d|30d。数据源为 rounds 聚合,口径是 sum(success)/sum(valid)。
func (a *API) monitorStats(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	from, to, bucket, ok := statsQuery(r)
	if !ok {
		return
	}
	stats, err := a.Store.GetStatsBuckets(r.Context(), id, from, to, bucket)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(stats))
	for _, s := range stats {
		var availability any
		if s.Valid > 0 {
			availability = round2(float64(s.Success) / float64(s.Valid) * 100)
		}
		var avgLatency any
		if s.LatencyCount > 0 {
			avgLatency = round2(s.LatencySumMs / float64(s.LatencyCount))
		}
		// 下载速度监控桶内平均速度(KB/s);无速度样本为 null(前端断线)。
		var avgSpeed any
		if s.SpeedCount > 0 {
			avgSpeed = round2(s.AvgSpeedKbps())
		}
		out = append(out, g.Map{
			"bucketAt":     s.BucketAt.Unix(),
			"bucket":       bucketLabel(s.BucketAt, bucket),
			"availability": availability,
			"avgLatencyMs": avgLatency,
			"avgSpeedKbps": avgSpeed,
			"speedCount":   s.SpeedCount,
			"valid":        s.Valid, "success": s.Success, "rounds": s.Rounds,
		})
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// monitorAgentLatency 详情页分节点趋势:与 /stats 同时间段、同颗粒度、同桶边界,
// 前端按 bucketAt 对齐到同一横轴。
//
// 一次聚合同时带出两种口径(见 store.GetAgentLatencyBuckets):延时(ms)与下载速度
// (KB/s),前端按监控类型取用 —— 下载速度监控的节点曲线画速度,其余画延时。
// 接口不按类型裁剪:同一行结果本来就是同一次观测的两个侧面,给全由调用方选,
// 比让后端猜"这个监控该看哪个"更少耦合。
func (a *API) monitorAgentLatency(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	from, to, bucket, ok := statsQuery(r)
	if !ok {
		return
	}
	pts, err := a.Store.GetAgentLatencyBuckets(r.Context(), id, from, to, bucket)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(pts))
	for _, p := range pts {
		out = append(out, g.Map{
			"agentId":      p.AgentID.Hex(),
			"bucketAt":     p.BucketAt.Unix(),
			"bucket":       bucketLabel(p.BucketAt, bucket),
			"avgLatencyMs": round2(p.AvgLatencyMs),
			"avgSpeedKbps": round2(p.AvgSpeedKbps),
			"count":        p.Count,
		})
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// urlOf 返回监控的展示目标:HTTP 与下载速度监控是 URL,PING 是主机名,
// TCP 是"主机:端口",push(外部上报)没有目标,展示为空(前端显示为「外部上报」)。
func urlOf(m *store.Monitor) string {
	switch m.Type {
	case "http", "download":
		return m.URL
	case "tcp":
		return net.JoinHostPort(m.TargetHost, strconv.Itoa(m.Port))
	case "push":
		return ""
	}
	return m.TargetHost
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
