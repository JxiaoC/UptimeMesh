package webhub

// 浏览器推送事件契约(文档见 docs/protocol.md「浏览器实时推送」)。

const (
	EvHello          = "hello"           // 连接建立首帧
	EvRoundFinalized = "round_finalized" // 轮次定稿(每轮一发)
	EvMonitorFlipped = "monitor_flipped" // 监控告警状态翻转(DOWN/UP)
	EvAgentChanged   = "agent_changed"   // 节点上下线/审批状态变化
	// EvMonitorChanged / EvMonitorDeleted 是监控配置本身的增删改:列表页据此就地
	// 更新/移除一行,不必整表拉取。
	EvMonitorChanged = "monitor_changed"
	EvMonitorDeleted = "monitor_deleted"
	// EvMonitorsChanged / EvMonitorsDeleted 是**批量**操作(列表页多选后暂停/恢复/删除)
	// 的推送:一批只发一帧。
	//
	// 为什么不复用上面两个逐行事件:浏览器连接的发送队列只有 32 帧,一次批量操作
	// (线上有 200+ 监控)会瞬间灌进去上百帧,慢客户端直接被丢帧。合成一帧既不会丢,
	// 也省掉几百次 JSON 序列化。
	EvMonitorsChanged = "monitors_changed"
	EvMonitorsDeleted = "monitors_deleted"
	// EvMonitorStrips 是「最近状态」状态条的快照:浏览器**主动请求**(见 ClientMessage
	// 与下面的 MonitorStripsData),服务端点对点分块回帧 —— 不是广播:只有监控列表页
	// 需要它,别的页面不该跟着收上万格色块。之后的新轮次仍由 EvRoundFinalized
	// 增量追加(见 api.pushMonitorStrips)。
	EvMonitorStrips = "monitor_strips"
)

// ReqMonitorStrips 是浏览器请求状态条快照的帧类型(与回帧同名,见 Hub.handle)。
const ReqMonitorStrips = EvMonitorStrips

// StripCell 状态条的一格,与 monitor_changed 推送里 recentRounds 的元素同构。
// 用结构体而不是 g.Map:状态条动辄上万格,GoFrame 编码器(反射 + 键排序)在
// 这个规模下本身就是开销大头,WS 这条路的 encoding/json 对类型化载荷快得多。
type StripCell struct {
	Status      string  `json:"status"` // up | breach | down | unknown
	ScheduledAt string  `json:"scheduledAt"`
	SuccessRate float64 `json:"successRate"`
	// SpeedKbps 仅下载速度监控有值(本轮平均速度,KB/s)。
	SpeedKbps float64 `json:"speedKbps,omitempty"`
}

// MonitorStrip 一个监控的状态条(时间升序,最老在前)。
type MonitorStrip struct {
	MonitorID string      `json:"monitorId"`
	Cells     []StripCell `json:"cells"`
}

// MonitorStripsData 一块状态条快照。一次请求会被拆成多帧(每帧若干监控):
// 首块几十毫秒就能到,页面上的色块是一片片长出来的,而不是等整批查完才渲染。
// 帧按 Seq 顺序发出;Total 是本次请求涉及的监控总数(0 = 一个监控都没有)。
type MonitorStripsData struct {
	Rounds int            `json:"rounds"` // 本次取数的格数上限(前端据此裁剪已有色块)
	Total  int            `json:"total"`
	Seq    int            `json:"seq"`
	Strips []MonitorStrip `json:"strips"`
}

// AgentTile 一轮里某个指派节点的展示(与 GET /overview 的 agents 元素**同构**)。
// 总览卡片的「节点明细」用它:哪个节点正常、哪个失败、失败原因是什么。
// 推送与初始拉取共用 api.agentTiles 组装,两处口径(顺序、缺失样本文案)不会漂移。
type AgentTile struct {
	AgentID   string  `json:"agentId"`
	OK        bool    `json:"ok"`
	LatencyMs float64 `json:"latencyMs"`
	Error     string  `json:"error"`
}

// RoundFinalizedData 轮次定稿摘要。
//
// 除轮次本身,还带上该监控**最新的展示状态与状态条单元**:列表页收到后就能就地更新
// 一行(状态色 + 状态条追加一格),不必再为每个轮次定稿整表拉一次 /monitors
// ——200+ 监控时那是页面流量的主要来源。总览页同理要靠 LatencyMs 与 Agents 就地更新
// 一张卡片(它的两张数值格与节点明细都随轮次变化),否则每个轮次定稿又得整页重拉。
type RoundFinalizedData struct {
	MonitorID   string  `json:"monitorId"`
	RoundID     string  `json:"roundId"`
	State       string  `json:"state"`       // CLOSED | UNKNOWN
	SuccessRate float64 `json:"successRate"` // 本次成功率 0~100
	// SpeedKbps 是下载速度监控的本轮平均速度(KB/s);其余类型为 0。
	// 列表页据此把状态条悬停提示显示成速度(单位按监控配置换算)。
	SpeedKbps float64 `json:"speedKbps,omitempty"`

	// 状态条单元(与该监控已有单元同口径):up=达标、breach=破线(未判 DOWN)、
	// down=已判 DOWN 期间的破线、unknown=无有效样本。
	ScheduledAt string `json:"scheduledAt"` // "2006-01-02 15:04:05"(服务端本地时区)
	RoundStatus string `json:"roundStatus"` // up | breach | down | unknown

	// 监控最新状态(displayState 即列表上的色块口径)。
	DisplayState string `json:"displayState"` // UP | DOWN | UNKNOWN | PAUSED
	AlertState   string `json:"alertState"`   // UP | DOWN
	Consecutive  int    `json:"consecutiveBreaches"`

	// LatencyMs 是本轮按时回传样本的平均延时(没有可用样本时为 0):
	// 总览卡片「最新延时」那一格用。
	LatencyMs float64 `json:"latencyMs"`
	// Agents 是本轮各指派节点的展示(顺序 = 监控配置的指派列表):
	// 总览卡片「节点明细」那排小标签用。push(外部上报)监控没有节点,恒为空数组。
	Agents []AgentTile `json:"agents"`
}

// MonitorFlippedData 告警状态翻转(载荷同样带够列表页就地更新所需的状态)。
//
// 除状态本身,还带上"这次翻转留下的那条状态变动记录":总览页的「最近状态变动记录」
// 是一张跨监控的流水,收到后直接把一行插到表头即可,不必为一次翻转再拉 /state-changes。
// 记录与翻转是 1:1 的同一件事(调度器先落记录再回调),所以并入本事件而不是另发一帧。
type MonitorFlippedData struct {
	MonitorID    string  `json:"monitorId"`
	Name         string  `json:"name"`
	AlertState   string  `json:"alertState"` // UP | DOWN
	SuccessRate  float64 `json:"roundSuccessRate"`
	DisplayState string  `json:"displayState"`
	Consecutive  int     `json:"consecutiveBreaches"`

	// 以下字段是那条变动记录本身(与 GET /state-changes 的一行同形)。
	// ToState 不另设字段:它就是 AlertState(翻转后的告警态)。
	RoundID   string `json:"roundId"`   // 触发翻转的那一轮
	FromState string `json:"fromState"` // 翻转前的告警态
	// ChangedAt 是变动记录的时间,与 GET /state-changes 的那一行逐字一致:
	// 两边都经 api.fmtTime 转服务端本地时区渲染(记录存的是 UTC 秒)。
	ChangedAt string `json:"changedAt"` // "2006-01-02 15:04:05"
	// DurationSec 是本次报警的持续秒数,仅恢复(UP)时有意义:等于上一条报错记录
	// 到这次恢复的间隔(调度器落记录时算好)。总览流水的「恢复」行旁据此多显示
	// 一张时长卡;0 = 没有可配对的报错记录,前端不显示(与接口同口径)。
	DurationSec int64 `json:"durationSec"`
	// 判定值按监控类型展示:Type 告诉前端这一行该显示速度还是成功率,
	// SpeedUnit/SpeedKbps 供下载速度监控换算展示(其余类型为 ""/0)。
	Type      string  `json:"type"`
	SpeedUnit string  `json:"speedUnit"`
	SpeedKbps float64 `json:"speedKbps"`
}

// AgentChangedData 节点在线/审批状态变化(载荷只给身份,详情前端自拉)。
type AgentChangedData struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Online  bool   `json:"online"`
}

// MonitorsChangedData 批量暂停/恢复:每项与 GET /monitors 的一行同构,但**不含
// recentRounds** —— 启停不改变历史状态条,前端就地更新时保留原有色块(见
// .scratch/monitor-bulk-actions/spec.md)。
type MonitorsChangedData struct {
	Monitors []map[string]any `json:"monitors"`
}

// MonitorsDeletedData 批量删除:一次给出被删掉的监控 ID 列表。
type MonitorsDeletedData struct {
	MonitorIDs []string `json:"monitorIds"`
}

// 状态条单元的四种口径。
const (
	RoundStatusUp      = "up"      // 达标轮
	RoundStatusBreach  = "breach"  // 破线轮(成功率低于阈值,但还没判成 DOWN)
	RoundStatusDown    = "down"    // 已判 DOWN 期间的破线轮(告警态已是 DOWN)
	RoundStatusUnknown = "unknown" // 无有效样本轮
)

// RoundCellStatus 把一轮的定稿结果折算成状态条色块口径。
// 列表页的初始数据(api.roundStatusStrip)与实时推送共用本函数,保证两处一致。
//
// value 与 threshold 是**同单位的判定值**:成功率监控是本次成功率 vs 百分比阈值,
// 下载速度监控是本轮平均速度 vs 速度阈值(KB/s)—— 两者都"越低越差",故比较方式相同。
// alertState 是**这一轮定稿之后**的监控告警态(UP/DOWN),破线轮据此分两种颜色:
// 连续破线还没攒够轮数 → breach(黄,预警);已经判成 DOWN → down(红,故障)。
// 无有效样本的轮次一律 unknown(它冻结状态机,不代表目标状态)。
func RoundCellStatus(state string, value, threshold float64, alertState string) string {
	switch {
	case state == "UNKNOWN":
		return RoundStatusUnknown
	case value >= threshold:
		return RoundStatusUp
	case alertState == "DOWN":
		return RoundStatusDown
	default:
		return RoundStatusBreach
	}
}
