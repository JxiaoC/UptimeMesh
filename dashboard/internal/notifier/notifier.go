// Package notifier 在监控告警状态翻转时向勾选的通知渠道 POST webhook。
// 载荷为通用 JSON(渠道无关,用户侧自行对接钉钉/企微/Slack/bark 等),
// 发送失败按 5s/15s/30s 重试 3 次(票 07)。
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/alert"
	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/checkconfig"
)

// Payload webhook 事件体。
// Title/Content 是按「设置 → 通知模板」渲染后的标题与正文(带占位符变量);
// 其余结构化字段保留原样,便于用户侧自行取用。
type Payload struct {
	Event       string  `json:"event"` // DOWN | UP | TEST
	Title       string  `json:"title"`
	Content     string  `json:"content"`
	MonitorID   string  `json:"monitorId"`
	MonitorName string  `json:"monitorName"`
	MonitorType string  `json:"monitorType"`
	URL         string  `json:"url"`
	SuccessRate float64 `json:"roundSuccessRate"`
	// SpeedKbps / SpeedUnit 仅下载速度监控有值:触发事件那一轮的平均速度与其配置单位
	// (SpeedKbps 是规范化后的 KB/s,单位供用户侧换算展示)。
	SpeedKbps float64 `json:"speedKbps,omitempty"`
	SpeedUnit string  `json:"speedUnit,omitempty"`
	// ErrorCount 是发送这一刻仍处于报警状态(alert_state=DOWN)的启用监控数,与模板变量
	// {{errorCount}} 同源(见 countDownAlerts):DOWN 事件里含本条监控,UP 事件里是恢复后
	// 剩下的那些。统计失败时为 0(模板变量此时渲染成空)。
	ErrorCount int `json:"errorCount"`
	// Nodes 是触发事件那一轮的节点明细(每个被指派节点一条):接收端不必再回查
	// Dashboard 就知道"是谁挂了、谁没回结果"。
	Nodes     []NodeStatus `json:"nodes,omitempty"`
	Timestamp string       `json:"timestamp"`
}

// 节点明细的状态取值(进模板变量与结构化载荷,前端据此上色)。
const (
	NodeUp      = "up"      // 按时回传且判定正常
	NodeDown    = "down"    // 按时回传但判定失败
	NodeMissing = "missing" // 节点在线却没回结果 ⇒ 该样本计失败
	NodeOffline = "offline" // 节点离线缺样 ⇒ 不计入分母
	// NodeUndispatched 任务从未交到该节点手上(建轮与重连补发时它都不在线)
	// ⇒ 不计入分母;与 offline 的区别是成因在派发侧,不是节点测不出目标。
	NodeUndispatched = "undispatched"
)

// NodeStatus 一个被指派节点在触发事件那一轮的结论。
type NodeStatus struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	// Status 见 NodeUp / NodeDown / NodeMissing / NodeOffline / NodeUndispatched。
	Status     string  `json:"status"`
	OK         bool    `json:"ok"`
	LatencyMs  float64 `json:"latencyMs,omitempty"`
	SpeedKbps  float64 `json:"speedKbps,omitempty"`
	HTTPStatus int     `json:"httpStatus,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// MissingNodes 是触发事件那一轮的缺样情况(由调度器给出):活着却没回结果的节点、
// 离线缺样的节点,以及任务从未交到手上的节点。这三类没有结果行,只查结果表拿不到
// 完整画面,所以必须单独带过来。
type MissingNodes struct {
	Alive []string // 节点在线却没回结果 ⇒ 计失败
	Dead  []string // 节点离线缺样 ⇒ 不计分母
	// Undispatched 任务没能交给它(建轮与重连补发时它都不在线)⇒ 不计分母。
	Undispatched []string
}

// Notifier 异步投递(webhook 不应阻塞调度循环)。
type Notifier struct {
	st *store.Store
	cl *http.Client
	wg sync.WaitGroup

	// RetryDelays 测试可缩短;默认 5s/15s/30s。
	RetryDelays []time.Duration
}

func New(st *store.Store) *Notifier {
	return &Notifier{
		st:          st,
		cl:          &http.Client{Timeout: 10 * time.Second},
		RetryDelays: []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second},
	}
}

// Wait 等待所有在途投递完成(优雅退出/测试用)。
func (n *Notifier) Wait() { n.wg.Wait() }

// EmitMonitorEvent 异步向监控勾选的全部渠道投递事件。
// missing 是触发事件那一轮的缺样名单(由调度器在定稿时给出):其余节点的结果由
// nodeStatuses 从结果表回读,两者合成完整的「节点状态」明细。
func (n *Notifier) EmitMonitorEvent(ctx context.Context, m *store.Monitor, ev alert.Event, missing MissingNodes) {
	ts := time.Now().Format(time.RFC3339)
	speedKbps, speedText := speedVars(m, ev)
	nodes := n.nodeStatuses(ctx, ev.RoundID, missing)
	// 报警数在状态落库之后取(调度器先 PutMonitorState 再回调通知):DOWN 事件里已经
	// 含本条监控,"还有几个"才是值班的人看到的真实局面。
	errorCount, errorCountText := n.countDownAlerts(ctx)
	vars := notifytmpl.Vars{
		Event: ev.Type, MonitorID: m.ID.Hex(), MonitorName: m.Name,
		MonitorType: strings.ToUpper(m.Type), URL: urlOf(m),
		SuccessRate: strconv.FormatFloat(ev.SuccessRate, 'f', -1, 64),
		Speed:       speedText,
		Agents:      renderNodes(nodes, m.Type, m.SpeedUnit),
		ErrorCount:  errorCountText,
		Timestamp:   ts,
	}
	rendered := notifytmpl.Render(n.template(ctx, ev.Type, m.Type), vars)
	p := Payload{
		Event: ev.Type, Title: rendered.Title, Content: rendered.Content,
		MonitorID: m.ID.Hex(), MonitorName: m.Name,
		MonitorType: m.Type, URL: urlOf(m),
		SuccessRate: ev.SuccessRate, SpeedKbps: speedKbps, SpeedUnit: m.SpeedUnit,
		ErrorCount: errorCount, Nodes: nodes, Timestamp: ts,
	}
	selected := n.st.FindChannelsByIDs(ctx, m.ChannelIds)
	active := enabledChannels(selected)
	// 一条都没发出去时留一行日志:"告警怎么没发出来"最常见的两种原因(勾选的渠道全被
	// 禁用、或全被删除)只有在这里看得出来,否则只能靠翻库。
	if len(active) == 0 && len(m.ChannelIds) > 0 {
		g.Log().Warningf(ctx, "监控 %s 勾选了 %d 个通知渠道,但没有一个可用(已禁用或已删除),本次 %s 通知未投递",
			m.Name, len(m.ChannelIds), ev.Type)
	}
	n.dispatch(ctx, active, p)
}

// enabledChannels 滤掉已禁用的渠道:勾选关系保留(启用后立即恢复投递),
// 但禁用期间一条都不发 —— 这正是渠道页那个开关的语义。
func enabledChannels(channels []*store.Channel) []*store.Channel {
	out := make([]*store.Channel, 0, len(channels))
	for _, ch := range channels {
		if ch.Enabled {
			out = append(out, ch)
		}
	}
	return out
}

// countDownAlerts 统计此刻仍处于报警状态的监控数,并给出模板变量 {{errorCount}} 的文本形式。
// 口径(只算启用中的 DOWN 监控)在 store.CountDownMonitors 里,注释见那里。
//
// 统计失败不该拖累通知本身:只记一条日志,让变量退化成空串(标题读作「【报警】xxx」,
// 一眼能看出这个数没取到),载荷里的 errorCount 则为 0 —— 不拿"0 个报警"冒充统计成功。
func (n *Notifier) countDownAlerts(ctx context.Context) (count int, text string) {
	total, err := n.st.CountDownMonitors(ctx)
	if err != nil {
		g.Log().Warningf(ctx, "统计报警中的监控数失败,通知里的 errorCount 将缺省: %v", err)
		return 0, ""
	}
	return total, strconv.Itoa(total)
}

// nodeStatuses 组装触发事件那一轮的节点明细:结果表里的按时结果 + 缺样名单。
// 查库失败只记日志并少一段明细 —— 通知本身比明细重要,不能因此不发。
func (n *Notifier) nodeStatuses(ctx context.Context, roundID string, missing MissingNodes) []NodeStatus {
	names := n.agentNames(ctx)
	out := make([]NodeStatus, 0, len(missing.Alive)+len(missing.Dead))
	if rid, err := store.IDFromHex(roundID); err == nil {
		results, rerr := n.st.FindResultsByRound(ctx, rid)
		if rerr != nil {
			g.Log().Warningf(ctx, "读取轮 %s 的节点结果失败,通知里将缺少节点明细: %v", roundID, rerr)
		}
		for _, res := range results {
			status, ok := NodeDown, res.OK
			if ok {
				status = NodeUp
			}
			out = append(out, NodeStatus{
				AgentID: res.AgentID.Hex(), Name: nameOf(names, res.AgentID.Hex()),
				Status: status, OK: ok, LatencyMs: res.LatencyMs, SpeedKbps: res.SpeedKbps,
				HTTPStatus: res.HTTPStatus, Error: res.Error,
			})
		}
	}
	for _, id := range missing.Alive {
		out = append(out, NodeStatus{AgentID: id, Name: nameOf(names, id), Status: NodeMissing, OK: false})
	}
	for _, id := range missing.Dead {
		out = append(out, NodeStatus{AgentID: id, Name: nameOf(names, id), Status: NodeOffline, OK: false})
	}
	for _, id := range missing.Undispatched {
		out = append(out, NodeStatus{AgentID: id, Name: nameOf(names, id), Status: NodeUndispatched, OK: false})
	}
	// 按节点名排序:告警要能一眼扫出"哪个节点挂了",顺序随轮次内 ID 排会每次都不一样。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// agentNames 读取节点名映射(含伪删除节点:历史轮次仍要显示名称)。
// 节点量级是几十个,一次全量读比逐个查省事;失败时回退成 ID 片段。
func (n *Notifier) agentNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	agents, err := n.st.FindAllAgentsIncludingDeleted(ctx)
	if err != nil {
		g.Log().Warningf(ctx, "读取节点列表失败,通知里的节点名将回退为 ID: %v", err)
		return out
	}
	for _, ag := range agents {
		out[ag.ID.Hex()] = ag.Name
	}
	return out
}

func nameOf(names map[string]string, id string) string {
	if name := names[id]; name != "" {
		return name
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// renderNodes 把节点明细渲染成通知正文里的多行文本(每行一个节点),供 {{agents}} 使用。
// 文案与 Agent 侧一致,一律中文:后端返回的文案不随界面语言变(见 AGENTS.md「国际化」)。
func renderNodes(nodes []NodeStatus, monitorType, speedUnit string) string {
	if len(nodes) == 0 {
		return "(无节点明细)"
	}
	var b strings.Builder
	for i, nd := range nodes {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- " + nd.Name + ": ")
		switch nd.Status {
		case NodeUp:
			b.WriteString("正常" + okSuffix(nd, monitorType, speedUnit))
		case NodeDown:
			b.WriteString("失败" + reasonSuffix(nd))
		case NodeMissing:
			b.WriteString("未回传结果(节点在线)")
		case NodeOffline:
			b.WriteString("离线(未计入本轮)")
		case NodeUndispatched:
			b.WriteString("未派发任务(未计入本轮)")
		default:
			b.WriteString(nd.Status)
		}
	}
	return b.String()
}

// okSuffix 正常样本的补充信息:下载速度监控报速度,其余报耗时。
func okSuffix(nd NodeStatus, monitorType, speedUnit string) string {
	if monitorType == checkconfig.TypeDownload && nd.SpeedKbps > 0 {
		unit := speedUnit
		if unit == "" {
			unit = checkconfig.SpeedUnitKBps
		}
		return fmt.Sprintf("(%.2f %s)", checkconfig.FromKbps(nd.SpeedKbps, unit), unit)
	}
	if nd.LatencyMs <= 0 {
		return ""
	}
	return fmt.Sprintf("(%.0f ms)", nd.LatencyMs)
}

// reasonSuffix 失败样本的原因:优先用节点回报的错误文案,其次退到状态码。
func reasonSuffix(nd NodeStatus) string {
	if strings.TrimSpace(nd.Error) != "" {
		return "(" + nd.Error + ")"
	}
	if nd.HTTPStatus > 0 {
		return fmt.Sprintf("(HTTP %d)", nd.HTTPStatus)
	}
	return ""
}

// speedVars 计算下载速度监控的通知变量:SpeedKbps 为规范化速度(KB/s,其余类型 0),
// speedText 是按监控配置单位格式化的展示文本(其余类型为空串,模板里渲染成空)。
func speedVars(m *store.Monitor, ev alert.Event) (kbps float64, text string) {
	if m.Type != checkconfig.TypeDownload {
		return 0, ""
	}
	unit := m.SpeedUnit
	if unit == "" {
		unit = checkconfig.SpeedUnitKBps
	}
	return ev.Value, fmt.Sprintf("%.2f %s", checkconfig.FromKbps(ev.Value, unit), unit)
}

// EmitTest 渠道"测试发送"按钮:同步返回投递结果描述。
// 刻意**不**看 Enabled:禁用渠道的按钮照样能发 —— 想先验通 URL 与模板再启用是常见顺序,
// 而这是用户亲手点的一次性请求,不是自动告警。
func (n *Notifier) EmitTest(ctx context.Context, ch *store.Channel) error {
	ts := time.Now().Format(time.RFC3339)
	// 测试消息也填真实报警数:自定义模板里的 {{errorCount}} 能当场看出取值口径
	// (默认 TEST 模板不带这个变量,标题仍是「【测试】」)。
	errorCount, errorCountText := n.countDownAlerts(ctx)
	rendered := notifytmpl.Render(n.template(ctx, notifytmpl.EventTest, ""), notifytmpl.Vars{
		Event: notifytmpl.EventTest, MonitorName: "(测试消息)", Timestamp: ts,
		ErrorCount: errorCountText,
	})
	p := Payload{
		Event: notifytmpl.EventTest, Title: rendered.Title, Content: rendered.Content,
		MonitorName: "(测试消息)", Timestamp: ts, ErrorCount: errorCount,
	}
	return n.postWithRetry(ctx, ch, p)
}

// template 读取并解析指定事件的消息模板;读库失败时回落默认模板,
// 保证配置异常不会阻断通知发送。monitorType 决定**默认**文案的口径
// (下载速度监控讲速度,其余讲成功率);用户自定义的模板永远优先。
func (n *Notifier) template(ctx context.Context, event, monitorType string) notifytmpl.Template {
	resolved := notifytmpl.DefaultsFor(monitorType)
	if st, err := n.st.GetSettings(ctx); err == nil {
		resolved = notifytmpl.ResolveFor(st.NotifyTemplates, monitorType)
	} else {
		g.Log().Warningf(ctx, "读取通知模板失败,使用默认模板: %v", err)
	}
	if t, ok := resolved[event]; ok {
		return t
	}
	return resolved[notifytmpl.EventDown]
}

// dispatch 每渠道一个 goroutine,互不阻塞。
func (n *Notifier) dispatch(ctx context.Context, channels []*store.Channel, p Payload) {
	for _, ch := range channels {
		n.wg.Add(1)
		go func(ch *store.Channel) {
			defer n.wg.Done()
			if err := n.postWithRetry(ctx, ch, p); err != nil {
				g.Log().Errorf(ctx, "webhook 渠道 %s(%s) 最终失败: %v", ch.Name, ch.URL, err)
			}
		}(ch)
	}
}

func (n *Notifier) postWithRetry(ctx context.Context, ch *store.Channel, p Payload) error {
	body, err := channelBody(ch, p)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		lastErr = n.postOnce(ctx, ch.URL, body)
		if lastErr == nil {
			return nil
		}
		if attempt >= len(n.RetryDelays) {
			return fmt.Errorf("尝试 %d 次均失败,最后一次: %w", attempt+1, lastErr)
		}
		g.Log().Debugf(ctx, "webhook %s 第 %d 次失败(%v),%s 后重试",
			ch.URL, attempt+1, lastErr, n.RetryDelays[attempt])
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.RetryDelays[attempt]):
		}
	}
}

// channelBody 生成请求体:配了自定义模板则用模板渲染,否则发默认通用 JSON。
func channelBody(ch *store.Channel, p Payload) ([]byte, error) {
	if strings.TrimSpace(ch.BodyTemplate) == "" {
		return json.Marshal(p)
	}
	v := notifytmpl.Vars{
		Event: p.Event, MonitorID: p.MonitorID, MonitorName: p.MonitorName,
		MonitorType: strings.ToUpper(p.MonitorType), URL: p.URL,
		SuccessRate: strconv.FormatFloat(p.SuccessRate, 'f', -1, 64),
		Speed:       speedTextOf(p),
		Agents:      renderNodes(p.Nodes, p.MonitorType, p.SpeedUnit),
		// 载荷里 errorCount 恒为数字(统计失败时是 0),照数字渲染:放在裸值位置
		// (`{{errorCount}}` 当数字用)也不会像空串那样留下非法 JSON。
		ErrorCount: strconv.Itoa(p.ErrorCount),
		Timestamp:  p.Timestamp,
	}
	return []byte(notifytmpl.RenderBody(ch.BodyTemplate, v, p.Title, p.Content)), nil
}

// speedTextOf 由载荷还原 {{speed}} 的展示文本(下载速度监控才有;其余为空)。
func speedTextOf(p Payload) string {
	if p.SpeedUnit == "" || p.SpeedKbps == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f %s", checkconfig.FromKbps(p.SpeedKbps, p.SpeedUnit), p.SpeedUnit)
}

func (n *Notifier) postOnce(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "UptimeMesh/0.1 webhook")
	resp, err := n.cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func urlOf(m *store.Monitor) string {
	if m.Type == "http" || m.Type == "download" {
		return m.URL
	}
	return m.TargetHost
}
