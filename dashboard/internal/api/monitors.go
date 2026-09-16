package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/alert"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/probe"
)

type monitorReq struct {
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Group       string            `json:"group"`
	Period      int               `json:"period"`
	Timeout     int               `json:"timeout"`
	Threshold   float64           `json:"threshold"`
	Consecutive int               `json:"consecutive"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	// ExpectStatusSpecs 是编辑态(单码或区间,如 "400~499");ExpectStatus 为兼容
	// 旧客户端保留的展开码列表,仅在未提供 Specs 时作为回退。
	ExpectStatusSpecs []string `json:"expectStatusSpecs"`
	ExpectStatus      []int    `json:"expectStatusCodes"`
	Contains          []string `json:"expectContains"`
	NotContains       []string `json:"expectNotContains"`
	AllowInsecure     bool     `json:"allowInsecureTLS"`
	// JSON 断言(与 UptimeKuma 的 JSON 查询同语义):JSONata 表达式取值 → 转字符串与期望值比较。
	JSONPath         string `json:"jsonPath"`
	JSONPathOperator string `json:"jsonPathOperator"`
	ExpectedValue    string `json:"jsonAssertExpected"`
	// InvertMode 反转探测判定(失败算正常、成功算故障);缺省不传即 false。
	InvertMode bool   `json:"invertMode"`
	TargetHost string `json:"targetHost"`
	// IPVersion 是探测使用的 IP 协议族:auto(默认,交给系统)/ ipv4 / ipv6。
	// 只作用于节点到目标的探测,不影响节点↔Dashboard 的连接。
	IPVersion string `json:"ipVersion"`
	// Port 是 TCP 端口监控的目标端口(1~65535)。
	Port int `json:"port"`
	// SpeedUnit 是下载速度监控的阈值单位:KB/s 或 MB/s(其余类型忽略)。
	SpeedUnit string `json:"speedUnit"`
	// PushToken 是 push 监控的上报令牌;新建时服务端生成,编辑时原样回传(不接受改写)。
	PushToken  string   `json:"pushToken"`
	AssignMode string   `json:"assignMode"`
	ExcludeIds []string `json:"excludedAgentIds"`
	AgentIds   []string `json:"assignedAgentIds"`
	ChannelIds []string `json:"channelIds"`
	Enabled    *bool    `json:"enabled"`
}

// ipVersionOf 归一化并校验表单里的 IP 协议族:空值按 auto(系统默认)处理,
// 兼容不传该字段的旧客户端与存量监控。
func ipVersionOf(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		v = checkconfig.IPVersionAuto
	}
	if !checkconfig.IPVersionValid(v) {
		return "", errors.New("IP 协议仅支持 auto(自动)、ipv4 或 ipv6")
	}
	return v, nil
}

// validate 中文可读错误;返回归一化后的 monitor 文档。
func (req *monitorReq) validate(ctx context.Context, st *store.Store) (*store.Monitor, error) {
	m := &store.Monitor{
		Type:   strings.ToLower(strings.TrimSpace(req.Type)),
		Name:   strings.TrimSpace(req.Name),
		Group:  strings.TrimSpace(req.Group),
		Period: req.Period, Timeout: req.Timeout,
		Threshold: req.Threshold, Consecutive: req.Consecutive,
		AssignMode: strings.TrimSpace(req.AssignMode),
		ChannelIds: req.ChannelIds,
		// 反转模式对 HTTP 与 PING 同义,统一在 validate 里落值。
		InvertMode: req.InvertMode,
	}
	if m.Type == "" {
		m.Type = checkconfig.TypeHTTP
	}
	if m.Name == "" {
		return nil, errors.New("监控名称必填")
	}
	if m.Type != checkconfig.TypeHTTP && m.Type != checkconfig.TypePing &&
		m.Type != checkconfig.TypeTCP && m.Type != checkconfig.TypePush &&
		m.Type != checkconfig.TypeDownload {
		return nil, errors.New("监控类型仅支持 http、ping、tcp、push 或 download")
	}
	// IP 协议族:所有会下发探测的类型都支持(auto/ipv4/ipv6)。
	ipVersion, err := ipVersionOf(req.IPVersion)
	if err != nil {
		return nil, err
	}
	m.IPVersion = ipVersion
	if m.Period < 10 || m.Period > 3600 {
		return nil, errors.New("周期须在 10~3600 秒之间")
	}
	if m.Timeout < 1 || m.Timeout > m.Period {
		return nil, fmt.Errorf("超时须在 1~%d 秒之间", m.Period)
	}
	// 阈值口径随类型而变:下载速度监控是速度下限(单位见 speedUnit),其余是成功率百分比。
	if m.Type == checkconfig.TypeDownload {
		if !(req.Threshold > 0) {
			return nil, errors.New("下载速度阈值须大于 0")
		}
		if req.Threshold > maxSpeedThreshold {
			return nil, fmt.Errorf("下载速度阈值不能超过 %g", float64(maxSpeedThreshold))
		}
	} else if m.Threshold < 0 || m.Threshold > 100 {
		return nil, errors.New("可用率阈值须在 0~100 之间")
	}
	if m.Consecutive < 1 {
		m.Consecutive = 3 // spec 默认值
	}
	// 指派模式:空值按 selected 处理,兼容存量文档。
	if m.AssignMode == "" {
		m.AssignMode = store.AssignModeSelected
	}
	// push(外部上报)监控没有节点参与探测:忽略表单里的指派设置,
	// 固定为"全部节点"但两个列表都留空(调度器本就不给它下发任务)。
	// IP 协议族同理无意义(探测发生在外部系统里),清空免得列表上显示一个不存在的配置。
	if m.Type == checkconfig.TypePush {
		m.AssignMode = store.AssignModeAll
		m.AssignedAgentIds = []string{}
		m.ExcludedAgentIds = []string{}
		m.IPVersion = ""
		// 阈值同理无意义:每轮只有一个样本,成功率非 0% 即 100%,填多少判定都一样;
		// 而填 0 会让该监控永不告警。表单已不展示这一项,这里把收到的值一律归一化,
		// 免得旧客户端/配置文件把那个数字带进来(见 store.PushThreshold)。
		m.Threshold = store.PushThreshold
	} else {
		switch m.AssignMode {
		case store.AssignModeSelected:
			m.AssignedAgentIds = req.AgentIds
			m.ExcludedAgentIds = []string{}
			if len(m.AssignedAgentIds) == 0 {
				return nil, errors.New("请至少指派一个节点")
			}
		case store.AssignModeAll:
			// 全部节点:建轮时动态解析,两个列表都清空。
			m.AssignedAgentIds = []string{}
			m.ExcludedAgentIds = []string{}
		case store.AssignModeExclude:
			m.AssignedAgentIds = []string{}
			m.ExcludedAgentIds = req.ExcludeIds
			if m.ExcludedAgentIds == nil {
				m.ExcludedAgentIds = []string{}
			}
			// 排除项是“不探测”黑名单:只校验 ID 格式,不校验节点状态——
			// 否则节点被吊销后旧监控会因残留的排除项而无法保存。
			for _, sid := range m.ExcludedAgentIds {
				if _, err := store.IDFromHex(sid); err != nil {
					return nil, errors.New("排除节点 ID 非法")
				}
			}
		default:
			return nil, errors.New("指派模式仅支持 selected/all/exclude")
		}
	}
	if m.ChannelIds == nil {
		m.ChannelIds = []string{}
	}

	switch m.Type {
	case checkconfig.TypeHTTP:
		if err := validateHTTPMonitor(m, req); err != nil {
			return nil, err
		}
	case checkconfig.TypeDownload:
		if err := validateDownloadMonitor(m, req); err != nil {
			return nil, err
		}
	case checkconfig.TypePing:
		m.TargetHost = strings.TrimSpace(req.TargetHost)
		if m.TargetHost == "" {
			return nil, errors.New("PING 监控须填写目标主机")
		}
	case checkconfig.TypeTCP:
		m.TargetHost = strings.TrimSpace(req.TargetHost)
		if m.TargetHost == "" {
			return nil, errors.New("TCP 端口监控须填写目标主机")
		}
		if req.Port < 1 || req.Port > 65535 {
			return nil, errors.New("TCP 端口须在 1~65535 之间")
		}
		m.Port = req.Port
	case checkconfig.TypePush:
		// 令牌只在首次创建时生成;编辑时沿用原值(存储层也不会改写该列),
		// 免得已部署的上报脚本因为一次编辑而失效。
		m.PushToken = strings.TrimSpace(req.PushToken)
		if m.PushToken == "" {
			token, err := NewPushToken()
			if err != nil {
				return nil, errors.New("生成上报令牌失败,请重试")
			}
			m.PushToken = token
		}
	}
	// 指派节点必须都是已批准节点。
	for _, sid := range m.AssignedAgentIds {
		oid, err := store.IDFromHex(sid)
		if err != nil {
			return nil, errors.New("指派节点 ID 非法")
		}
		a, err := st.FindAgentByID(ctx, oid)
		if err != nil || a.Status != store.AgentApproved {
			return nil, fmt.Errorf("指派节点 %s 不存在或未批准", sid)
		}
	}
	return m, nil
}

// maxSpeedThreshold 是下载速度阈值的上限(用配置单位表示):只是防呆上限
// (例如把字节数当速度填进来),不做量纲校验。
const maxSpeedThreshold = 1 << 30

// validateRequestTarget 校验 HTTP 与下载速度监控共用的**请求侧**配置:
// URL(必须是 http(s))、方法白名单、请求头、请求体、忽略证书校验。
// 两者在这部分是"同一份配置",故共用一段校验;差异只在判定项(见各自的调用方)。
func validateRequestTarget(m *store.Monitor, req *monitorReq) error {
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("URL 须为合法的 http(s) 地址")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return errors.New("不支持的请求方法: " + method)
	}
	m.URL = u.String()
	m.Method = method
	m.Headers = req.Headers
	m.Body = req.Body
	m.AllowInsecureTLS = req.AllowInsecure
	return nil
}

// validateDownloadMonitor 校验下载速度监控:请求侧与 HTTP 一致,另外要求速度阈值与单位合法。
// 反转模式对速度监控无意义(判定值是速度,不是探测成败),这里强制关闭。
func validateDownloadMonitor(m *store.Monitor, req *monitorReq) error {
	if err := validateRequestTarget(m, req); err != nil {
		return err
	}
	unit := strings.TrimSpace(req.SpeedUnit)
	if unit == "" {
		unit = checkconfig.SpeedUnitKBps
	}
	if !checkconfig.SpeedUnitValid(unit) {
		return errors.New("下载速度单位仅支持 KB/s 或 MB/s")
	}
	m.SpeedUnit = unit
	m.Threshold = req.Threshold
	m.InvertMode = false
	return nil
}

func validateHTTPMonitor(m *store.Monitor, req *monitorReq) error {
	if err := validateRequestTarget(m, req); err != nil {
		return err
	}
	// 期望状态码:编辑态 spec(单码/区间)为准;未提供时回退旧字段,再空则默认 200。
	specs := req.ExpectStatusSpecs
	if len(specs) == 0 && len(req.ExpectStatus) > 0 {
		specs = checkconfig.StatusSpecsFromCodes(req.ExpectStatus)
	}
	if len(specs) == 0 {
		specs = []string{"200"}
	}
	normSpecs := make([]string, 0, len(specs))
	seen := map[string]bool{}
	for _, raw := range specs {
		spec, err := checkconfig.ParseStatusSpec(raw)
		if err != nil {
			return err
		}
		if seen[spec] {
			continue
		}
		seen[spec] = true
		normSpecs = append(normSpecs, spec)
	}
	codes, err := checkconfig.ExpandStatusSpecs(normSpecs)
	if err != nil {
		return err
	}
	m.ExpectStatusSpecs = normSpecs
	m.ExpectStatusCodes = codes
	m.ExpectContains = req.Contains
	m.ExpectNotContains = req.NotContains
	// JSON 断言:表达式留空即关闭;非空时校验 JSONata 表达式可编译、运算符在白名单内,
	// 让用户在保存时就得到反馈,而不是等到探测时才失败。
	m.JsonPath = strings.TrimSpace(req.JSONPath)
	if m.JsonPath != "" {
		operator := strings.TrimSpace(req.JSONPathOperator)
		if msg := probe.ValidateJSONAssert(m.JsonPath, operator); msg != "" {
			return errors.New(msg)
		}
		if operator == "" {
			operator = checkconfig.OperatorEqual
		}
		m.JsonPathOperator = operator
		m.JsonAssertExpect = req.ExpectedValue
	} else {
		m.JsonPathOperator = ""
		m.JsonAssertExpect = ""
	}
	return nil
}

// expectStatusSpecs 返回监控的编辑态期望状态码;兼容仅有展开码的存量文档。
func expectStatusSpecs(m *store.Monitor) []string {
	if len(m.ExpectStatusSpecs) > 0 {
		return m.ExpectStatusSpecs
	}
	if len(m.ExpectStatusCodes) > 0 {
		return checkconfig.StatusSpecsFromCodes(m.ExpectStatusCodes)
	}
	return []string{"200"}
}

func (a *API) registerMonitorRoutes(group *ghttp.RouterGroup) {
	group.GET("/monitors", a.listMonitors)
	// 状态条快照的 HTTP 兜底(WS 不可用时,见 monitor_strips.go)。静态路径要先于
	// /monitors/{id} 注册:GoFrame 的模糊路由与静态路由冲突时以静态优先,这里
	// 顺带把它摆在前面,免得将来调路由优先级时"strips"被当成监控 ID。
	group.GET("/monitors/strips", a.listMonitorStrips)
	group.POST("/monitors", a.createMonitor)
	// 批量操作:一次作用于多个监控(列表页多选后的暂停/恢复/删除)。
	group.POST("/monitors/batch", a.batchMonitors)
	// 测试:把弹窗里未保存的探测配置交给一个在线节点跑一次,只回结果不落库(见 monitors_check.go)。
	group.POST("/monitors/test", a.testMonitor)
	group.GET("/monitors/{id}", a.getMonitor)
	group.PUT("/monitors/{id}", a.updateMonitor)
	group.DELETE("/monitors/{id}", a.deleteMonitor)
	group.POST("/monitors/{id}/pause", func(r *ghttp.Request) { a.setMonitorEnabled(r, false) })
	group.POST("/monitors/{id}/resume", func(r *ghttp.Request) { a.setMonitorEnabled(r, true) })
}

// listMonitors 监控列表:只给**配置 + 展示状态**,不含状态条(recentRounds)。
//
// 状态条为什么不在这里:每监控 N 格历史色块(默认 50、上限 200),200+ 监控时是
// 上万格、约 1MB 的响应体,而整张表要等它到齐才渲染 —— 进页面因此要等好几秒。
// 现在色块由浏览器在 WS 上单独要一次快照(见 monitor_strips.go),之后的增量由
// round_finalized 推送维护,WS 不可用时用 GET /monitors/strips 兜底。
// 于是本接口的响应体只随监控条数增长,进页面立刻可用。
func (a *API) listMonitors(r *ghttp.Request) {
	list, err := a.Store.ListMonitors(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID.Hex())
	}
	states := a.Store.GetMonitorStatesByIDs(r.Context(), ids)

	// 行的展示结构:view 是给前端的字段,rank 用于排序。
	type monitorRow struct {
		view g.Map
		rank int
	}
	rows := make([]monitorRow, 0, len(list))
	for _, m := range list {
		v := monitorView(m)
		state := DisplayState(m, states[m.ID.Hex()])
		v["displayState"] = state
		if st := states[m.ID.Hex()]; st != nil {
			v["alertState"] = st.AlertState
			v["consecutiveBreaches"] = st.Consecutive
		} else {
			v["alertState"] = store.MonitorUP
			v["consecutiveBreaches"] = 0
		}
		rows = append(rows, monitorRow{view: v, rank: monitorStateRank(state)})
	}
	// 故障优先:DOWN 在最前,其次是未知/等待上报、正常,暂停的排最后。
	// 同一档内保持 store 给出的顺序(创建时间新→旧)。
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].rank < rows[j].rank })

	out := make([]g.Map, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.view)
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// stripRoundsFor 决定这一次要取多少格状态条:
//   - ?rounds= 落在合法区间内 → 用它(接口调用方可以按需放大/缩小这一次的窗口);
//   - 否则取后台设置「最近状态格数」(store.StatusStripRounds,缺省 50,越界回落默认)。
//
// 列表接口与推给浏览器的整行都必须走它:两处一旦给出不同的格数,前端就会出现
// "编辑保存后色块凭空变多"(见 .scratch/monitors-strip-length)。
func (a *API) stripRoundsFor(ctx context.Context, override int) int {
	if override > 0 && override <= store.MaxStatusStripRounds {
		return override
	}
	return a.Store.StatusStripRounds(ctx)
}

// monitorStateRank 列表排序权重:越需要被看到的越靠前。
func monitorStateRank(state string) int {
	switch state {
	case store.MonitorDOWN:
		return 0
	case "UNKNOWN": // 含 push 监控的"等待上报"
		return 1
	case store.MonitorUP:
		return 2
	default: // PAUSED
		return 3
	}
}

// roundStatusStrip 把最近轮次映射为状态条色块序列(时间升序,最老在前)。
// 色块口径与实时推送共用 webhub.RoundCellStatus:up=达标轮;breach=破线轮(还没判 DOWN);
// down=已判 DOWN 期间的破线轮;unknown=无有效样本轮。
//
// 色块要回答"这一轮定稿时监控是什么状态",所以用告警状态机本身
// (alert.Apply,与调度器同一实现)从窗口最老一轮起重放:连续破线攒够 consecutive 轮
// 的那一轮起转红,中途达标即清零(黄色回落绿)。这样"黄=预警、红=故障"与真实判定
// 永远同源,而不是另写一套阈值比较。
//
// 判定值与阈值都按监控类型取口径(见 thresholdOf 与 roundValueOf):下载速度监控用
// 本轮平均速度,其余用本次成功率 —— 两者都"越低越差"。
//
// st 是该监控当前的状态记录,用来给重放播种(stripSeed):窗口左边缘落在破线中间时,
// 靠它才能把"早就在 DOWN 里"的轮次显示成红色而不是黄色预警。
func roundStatusStrip(rounds []*store.RoundLite, m *store.Monitor, st *store.MonitorState) []webhub.StripCell {
	threshold := thresholdOf(m)
	consecutive := m.Consecutive
	if consecutive < 1 {
		consecutive = 1 // 与监控配置校验(>=1)对齐,退化的 0 会让首个破线轮直接判 DOWN
	}
	out := make([]webhub.StripCell, 0, len(rounds))
	state := alert.State{AlertState: alert.Up, Consecutive: stripSeed(rounds, m, st)}
	if state.Consecutive >= consecutive {
		// 窗口之前就已经攒够连续破线轮数:这段破线从窗口第一格起就是红的。
		state.AlertState = alert.Down
	}
	for _, rd := range rounds {
		next, _ := alert.Apply(state, alert.RoundOutcome{
			Valid:       rd.State != store.RoundStateUnknown, // UNKNOWN 冻结状态机
			Value:       roundValueOf(m, rd),
			SuccessRate: rd.SuccessRate,
		}, threshold, consecutive)
		state = next
		cell := webhub.StripCell{
			Status:      webhub.RoundCellStatus(rd.State, roundValueOf(m, rd), threshold, state.AlertState),
			ScheduledAt: fmtTime(rd.ScheduledAt),
			SuccessRate: round2(rd.SuccessRate),
		}
		// 下载速度监控的状态条悬停要显示本轮速度(单位按监控配置换算,前端负责展示)。
		if m.Type == checkconfig.TypeDownload {
			cell.SpeedKbps = round2(rd.AvgSpeedKbps())
		}
		out = append(out, cell)
	}
	return out
}

// roundValueOf 返回一轮的状态条/告警判定值(与 thresholdOf 同单位)。
func roundValueOf(m *store.Monitor, rd *store.RoundLite) float64 {
	if m.Type == checkconfig.TypeDownload {
		return rd.AvgSpeedKbps()
	}
	return rd.SuccessRate
}

// stripSeed 反推"窗口最老一轮之前已经积累的连续破线轮数",让窗口左边缘落在破线中间时
// 也能正确显示红色,而不是把一段持续了很久的故障的前几格误显示成黄色预警。
//
// 依据:monitor_states.consecutive 是**当前**连续破线轮数 L(含窗口内的这些轮),窗口末尾
// 可见的破线轮数为 V(缺样不算断点,达标轮才是)。若这段破线整段落在窗口内则 L == V,
// 种子 0;若 L > V,说明这段破线在窗口之前就开始了,窗口开始时的计数就是 L-V。
// 只对当前处于 DOWN 的监控播种:没判 DOWN 就不可能有跨窗口的破线段(攒够轮数
// 就会判 DOWN),这时窗口内的重放本来就准确。
func stripSeed(rounds []*store.RoundLite, m *store.Monitor, st *store.MonitorState) int {
	if st == nil || st.AlertState != store.MonitorDOWN {
		return 0
	}
	threshold := thresholdOf(m)
	visible := 0
	for i := len(rounds) - 1; i >= 0; i-- {
		rd := rounds[i]
		if rd.State == store.RoundStateUnknown {
			continue // 缺样冻结计数,不打断这段破线
		}
		if roundValueOf(m, rd) >= threshold {
			break // 达标轮是这段破线的起点
		}
		visible++
	}
	if seed := st.Consecutive - visible; seed > 0 {
		return seed
	}
	return 0
}

// DisplayState UI 状态色口径:暂停优先;最近轮 UNKNOWN 则 UNKNOWN;否则告警态。
// push 监控在收到第一次上报之前没有任何状态记录,显式显示为 UNKNOWN(等待上报),
// 而不是默认的 UP —— 否则一个从未上报过的监控会一直显示"正常"。
func DisplayState(m *store.Monitor, st *store.MonitorState) string {
	if !m.Enabled {
		return "PAUSED"
	}
	if st == nil {
		if m.Type == checkconfig.TypePush {
			return "UNKNOWN"
		}
		return store.MonitorUP
	}
	if st.LastRoundState == "UNKNOWN" {
		return "UNKNOWN"
	}
	return st.AlertState
}

func monitorView(m *store.Monitor) g.Map {
	return g.Map{
		"id": m.ID.Hex(), "type": m.Type, "name": m.Name, "group": m.Group, "enabled": m.Enabled,
		"period": m.Period, "timeout": m.Timeout,
		"threshold": m.Threshold, "consecutive": m.Consecutive,
		"url": m.URL, "method": m.Method, "headers": m.Headers, "body": m.Body,
		"expectStatusSpecs": expectStatusSpecs(m),
		"expectStatusCodes": m.ExpectStatusCodes,
		"expectContains":    m.ExpectContains, "expectNotContains": m.ExpectNotContains,
		"allowInsecureTLS":   m.AllowInsecureTLS,
		"invertMode":         m.InvertMode,
		"ipVersion":          m.IPVersion,
		"jsonPath":           m.JsonPath,
		"jsonPathOperator":   m.JsonPathOperator,
		"jsonAssertExpected": m.JsonAssertExpect,
		"targetHost":         m.TargetHost,
		"port":               m.Port,
		// speedUnit 仅 download 类型有值:阈值(threshold)的解释单位。
		"speedUnit": m.SpeedUnit,
		// push 监控:令牌 + 相对上报地址(前端补 origin 展示与复制)。
		"pushToken": m.PushToken,
		"pushUrl":   pushURLOf(m),
		"lastPushAt": func() string {
			if m.LastPushAt.IsZero() {
				return ""
			}
			return fmtTime(m.LastPushAt)
		}(),
		"assignMode":       m.AssignMode,
		"excludedAgentIds": m.ExcludedAgentIds,
		"assignedAgentIds": m.AssignedAgentIds, "channelIds": m.ChannelIds,
		"createdAt": fmtTime(m.CreatedAt),
	}
}

// RoundKicker 让"编辑 / 恢复后立刻跑一轮"这条链路不直接依赖 scheduler 包:
// 轮次与下发只有调度协程一个写入者(ADR-0003),API 不能自己建轮,只能通知调度器
// 清掉该监控的建轮节流(下一个 tick 就建轮)。由调度器实现(cmd 里注入);
// 为 nil 时静默跳过(未接调度器的装配路径与部分测试)。
type RoundKicker interface {
	Kick(monitorID store.ID)
	KickAll(monitorIDs []store.ID)
}

// kickRound 通知调度器"这个监控该马上跑一轮"(未接调度器时什么都不做)。
func (a *API) kickRound(id store.ID) {
	if a.Kicker == nil {
		return
	}
	a.Kicker.Kick(id)
}

// kickRounds 批量版(批量恢复用)。
func (a *API) kickRounds(ids []store.ID) {
	if a.Kicker == nil || len(ids) == 0 {
		return
	}
	a.Kicker.KickAll(ids)
}

func (a *API) parseMonitorID(r *ghttp.Request) (store.ID, bool) {
	id, err := store.IDFromHex(r.Get("id").String())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "监控 ID 非法"})
		return "", false
	}
	return id, true
}

func (a *API) getMonitor(r *ghttp.Request) {
	id, ok := a.parseMonitorID(r)
	if !ok {
		return
	}
	m, err := a.Store.FindMonitorByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "监控不存在"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": monitorView(m)})
}

func (a *API) createMonitor(r *ghttp.Request) {
	var req monitorReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法: " + err.Error()})
		return
	}
	m, err := req.validate(r.Context(), a.Store)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	m.Enabled = req.Enabled == nil || *req.Enabled
	if err := a.Store.InsertMonitor(r.Context(), m); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "创建失败"})
		return
	}
	// 新建的监控直接推给浏览器:列表页插入一行,不必整表重拉。
	a.BroadcastMonitorChanged(r.Context(), m)
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": monitorView(m)})
}

func (a *API) updateMonitor(r *ghttp.Request) {
	id, ok := a.parseMonitorID(r)
	if !ok {
		return
	}
	existing, err := a.Store.FindMonitorByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "监控不存在"})
		return
	}
	var req monitorReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	if req.Type == "" {
		req.Type = existing.Type
	}
	m, err := req.validate(r.Context(), a.Store)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	m.ID = existing.ID
	m.CreatedAt = existing.CreatedAt
	m.Enabled = req.Enabled == nil || *req.Enabled
	err = a.Store.UpdateMonitor(r.Context(), m)
	if errors.Is(err, store.ErrNotFound) {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "监控不存在"})
		return
	}
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "更新失败"})
		return
	}
	a.BroadcastMonitorChanged(r.Context(), m)
	// 配置改了就该马上按新配置跑一轮,而不是干等满一个周期(周期最大 3600 秒)。
	// 监控暂停时不建轮(调度器只跑启用中的监控),恢复时会再踢一次。
	a.kickRound(m.ID)
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": monitorView(m)})
}

func (a *API) deleteMonitor(r *ghttp.Request) {
	id, ok := a.parseMonitorID(r)
	if !ok {
		return
	}
	// 级联:连同其轮次、结果与告警状态一起删除(单事务),避免统计口径残留(票 05 后有数据)。
	err := a.Store.DeleteMonitorCascade(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "监控不存在"})
		return
	}
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "删除失败"})
		return
	}
	a.BroadcastMonitorDeleted(id.Hex())
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已删除"})
}

func (a *API) setMonitorEnabled(r *ghttp.Request, enabled bool) {
	id, ok := a.parseMonitorID(r)
	if !ok {
		return
	}
	err := a.Store.SetMonitorEnabled(r.Context(), id, enabled)
	if errors.Is(err, store.ErrNotFound) {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "监控不存在"})
		return
	}
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "操作失败"})
		return
	}
	if !enabled {
		// 暂停(维护窗口):清破线计数、保留告警状态——恢复后若首轮达标仍会发出恢复通知。
		// 调度器每 tick 重读启用列表,暂停后不产生新轮次;在途轮次照常定稿入库。
		if st, found, e := a.Store.GetMonitorState(r.Context(), id); e == nil && found {
			st.Consecutive = 0
			_ = a.Store.PutMonitorState(r.Context(), st)
		}
	} else {
		// 恢复:卡片上的状态与可用率都还停在暂停前,立刻按当前配置跑一轮刷新它们。
		a.kickRound(id)
	}
	word := "已恢复"
	if !enabled {
		word = "已暂停"
	}
	// 启用状态变了:推一次该监控的最新行(含状态色),列表页就地更新。
	if m, e := a.Store.FindMonitorByID(r.Context(), id); e == nil {
		a.BroadcastMonitorChanged(r.Context(), m)
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": word})
}

// ---- 批量操作(列表页多选)----

// 批量操作的动作白名单。
const (
	batchActionPause  = "pause"
	batchActionResume = "resume"
	batchActionDelete = "delete"
)

type batchMonitorReq struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"`
}

// batchMonitors 一次处理一批监控的暂停/恢复/删除(POST /monitors/batch)。
//
// 为什么不让前端循环调用单行接口:线上有 200+ 监控,那是 200 个请求、200 个单行事务,
// 而且"第 37 个失败了算成功还是失败"没有好答案。这里启停是一条 UPDATE、删除是一个事务,
// affected 如实回报(选中的监控可能刚被别处删掉)。
//
// 语义与单行接口对齐:暂停同样清零连续破线计数并保留告警状态;恢复不动告警状态。
// 推送侧合成一帧(EvMonitorsChanged / EvMonitorsDeleted),不逐行发 —— 见 webhub/events.go。
func (a *API) batchMonitors(r *ghttp.Request) {
	var req batchMonitorReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action != batchActionPause && action != batchActionResume && action != batchActionDelete {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "批量操作仅支持 pause、resume 或 delete"})
		return
	}
	// ID 先去重、再逐条校验:重复 ID 会让 affected 与实际不符;非法 ID 拒绝整个请求
	// —— 批量操作里静默跳过比报错更危险(用户以为删掉了,其实没删)。
	ids := make([]store.ID, 0, len(req.IDs))
	seen := make(map[string]bool, len(req.IDs))
	for _, raw := range req.IDs {
		trimmed := strings.TrimSpace(raw)
		id, err := store.IDFromHex(trimmed)
		if err != nil {
			r.Response.WriteJsonExit(g.Map{"code": 400, "message": "监控 ID 非法: " + trimmed})
			return
		}
		if seen[id.Hex()] {
			continue
		}
		seen[id.Hex()] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "请至少选择一个监控"})
		return
	}
	ctx := r.Context()

	if action == batchActionDelete {
		deleted, err := a.Store.DeleteMonitorsCascade(ctx, ids)
		if err != nil {
			r.Response.WriteJsonExit(g.Map{"code": 500, "message": "批量删除失败"})
			return
		}
		hexes := make([]string, 0, len(ids))
		for _, id := range ids {
			hexes = append(hexes, id.Hex())
		}
		a.BroadcastMonitorsDeleted(hexes)
		r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
			"action": action, "requested": len(ids), "affected": deleted, "monitors": []g.Map{},
		}})
		return
	}

	enabled := action == batchActionResume
	affected, err := a.Store.SetMonitorsEnabled(ctx, ids, enabled)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "操作失败"})
		return
	}
	if !enabled {
		// 与单行暂停同语义。计数清零失败不阻断启停本身(单行接口也是这么处理的):
		// 最坏情况是维护窗口里的旧计数参与了下一次判定。
		_ = a.Store.ResetMonitorsConsecutive(ctx, ids)
	} else {
		// 与单行恢复同语义:一批恢复完立刻各跑一轮(调度器下一个 tick 建轮)。
		a.kickRounds(ids)
	}
	// 回读最新行:响应体与推送载荷都要反映改完之后的状态。
	list, err := a.Store.FindMonitorsByIDs(ctx, ids)
	if err != nil {
		list = nil
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"action": action, "requested": len(ids), "affected": affected,
		"monitors": a.BroadcastMonitorsChanged(ctx, list),
	}})
}
