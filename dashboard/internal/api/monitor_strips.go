package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
)

// 监控列表页「最近状态」一列(状态条)的数据通路。
//
// 为什么它不跟 GET /monitors 一起走:那一列是每个监控 N 格历史色块(格数取后台设置
// 「最近状态格数」,默认 50、上限 200),线上 200+ 监控时就是上万格、约 1MB 的响应体,
// 而且**整张表要等它到齐才能渲染** —— 每次进页面都要等好几秒(v-loading 一直转)。
//
// 现在拆成两条道:
//   - GET /monitors 只给配置 + 展示状态(小而快),页面立刻可用;
//   - 色块由浏览器在 WS 上主动要一次快照(ReqMonitorStrips → 本文件的分块推送),
//     之后的新轮次仍由 round_finalized 增量追加(见 browser_push.go)。
//
// WS 不可用(被代理挡掉/断线)时前端改用 GET /monitors/strips 兜底,载荷与本文件
// 的推送同形 —— 两条路都走 monitorStrips 组装,色块颜色与格数口径不会漂移。

const (
	// stripChunkSize 一块推多少监控的状态条。太小会把连接的发送队列灌满帧数,
	// 太大则首块要等更久才发出;25 个(≈1250 格)落在两头之间,首块几十毫秒就能到。
	stripChunkSize = 25
	// stripSendTimeout 单帧写入超时:等不到写队列空闲就放弃整批(连接多半已经坏了),
	// 前端重连后会重新要一次。
	stripSendTimeout = 10 * time.Second
	// stripSnapshotTimeout 一次快照的总预算(查库 + 分块推送)。
	stripSnapshotTimeout = 30 * time.Second
	// stripMinGap 同一条连接上两次快照请求的最小间隔。WS 端点免鉴权(只读推送),而
	// 一次快照是上万行级别的查询 + 上百 KB 的响应 —— 不节流就是一台免费的放大器。
	// 正常页面每条连接只要一次(手工刷新会再要一次),1s 足够挡住刷请求又不误伤操作。
	stripMinGap = time.Second
)

// HandleBrowserRequest 处理浏览器请求帧(api.Register 里注入 webhub)。
// 目前只有「要一次状态条快照」一种请求。
func (a *API) HandleBrowserRequest(c *webhub.Conn, msg webhub.ClientMessage) {
	if a.Web == nil || c == nil {
		return
	}
	switch msg.Type {
	case webhub.ReqMonitorStrips:
		if !c.Throttle(webhub.ReqMonitorStrips, stripMinGap) {
			return // 太频繁:静默忽略(连接仍在,页面拿到的还是刚推过的那份)
		}
		a.pushMonitorStrips(c, msg.Rounds)
	}
}

// pushMonitorStrips 应答一次状态条请求:按块查库、逐块推给**这一条**连接。
func (a *API) pushMonitorStrips(c *webhub.Conn, rounds int) {
	// WS 请求没有 HTTP 请求上下文,自己起一个带超时的:整批(查询 + 推送)的总预算。
	ctx, cancel := context.WithTimeout(context.Background(), stripSnapshotTimeout)
	defer cancel()

	limit := a.stripRoundsFor(ctx, rounds)
	list, err := a.Store.ListMonitors(ctx)
	if err != nil {
		return // 查不到就不应答:前端有 HTTP 兜底,重连后也会再要一次
	}
	states := a.Store.GetMonitorStatesByIDs(ctx, monitorHexIDs(list))
	total := len(list)
	seq := 0
	for start := 0; start < total; start += stripChunkSize {
		end := start + stripChunkSize
		if end > total {
			end = total
		}
		data := webhub.MonitorStripsData{
			Rounds: limit, Total: total, Seq: seq,
			Strips: a.monitorStrips(ctx, list[start:end], states, limit),
		}
		if err := c.SendWait(webhub.Event{Type: webhub.EvMonitorStrips, Data: data}, stripSendTimeout); err != nil {
			return
		}
		seq++
	}
	if seq == 0 {
		// 一个监控都没有:也回一帧空快照,让前端知道"这次请求有应答",不必空等。
		_ = c.SendWait(webhub.Event{Type: webhub.EvMonitorStrips,
			Data: webhub.MonitorStripsData{Rounds: limit, Total: 0, Seq: 0}}, stripSendTimeout)
	}
}

// listMonitorStrips 状态条快照的 HTTP 兜底入口(GET /monitors/strips?rounds=N)。
// 载荷与 WS 的分块推送同形,只是不切块:WS 不可用时前端用它,其余场景都走推送。
func (a *API) listMonitorStrips(r *ghttp.Request) {
	ctx := r.Context()
	limit := a.stripRoundsFor(ctx, r.Get("rounds").Int())
	list, err := a.Store.ListMonitors(ctx)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	states := a.Store.GetMonitorStatesByIDs(ctx, monitorHexIDs(list))
	strips := a.monitorStrips(ctx, list, states, limit)

	// 自己编码,不走 GoFrame 的 gjson:载荷是上万格小对象,那个编码器(反射 + 键排序)
	// 在这里是实打实的开销;类型化结构交给 encoding/json 快得多(与 WS 同一条路)。
	body, err := json.Marshal(stripsResponse{
		Data: webhub.MonitorStripsData{Rounds: limit, Total: len(strips), Strips: strips},
	})
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "编码失败"})
		return
	}
	r.Response.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.Response.Write(body)
	r.Exit()
}

// stripsResponse 是 GET /monitors/strips 的响应外壳(与其它接口的 {code,data} 同形)。
type stripsResponse struct {
	Code int                      `json:"code"`
	Data webhub.MonitorStripsData `json:"data"`
}

// monitorStrips 组装一批监控的状态条快照。
// limit 由调用方给(stripRoundsFor:?rounds= 或后台设置),越界在 store 层回落默认 ——
// 与监控列表页渲染的格数只有这一个口径(见 .scratch/monitors-strip-length)。
func (a *API) monitorStrips(ctx context.Context, list []*store.Monitor,
	states map[string]*store.MonitorState, limit int) []webhub.MonitorStrip {
	if len(list) == 0 {
		return nil
	}
	recent, _ := a.Store.ListRecentRoundsByMonitors(ctx, monitorIDs(list), limit)
	out := make([]webhub.MonitorStrip, 0, len(list))
	for _, m := range list {
		out = append(out, webhub.MonitorStrip{
			MonitorID: m.ID.Hex(),
			Cells:     roundStatusStrip(recent[m.ID.Hex()], m, states[m.ID.Hex()]),
		})
	}
	return out
}

// monitorIDs 取一批监控的主键(轮次批量查询用)。
func monitorIDs(list []*store.Monitor) []store.ID {
	out := make([]store.ID, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// monitorHexIDs 取一批监控主键的 hex 文本(状态批量查询用)。
func monitorHexIDs(list []*store.Monitor) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID.Hex())
	}
	return out
}
