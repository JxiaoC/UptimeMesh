package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/configfile"
	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/checkconfig"
)

// 配置文件导入(设置页「配置导入导出」标签页)。格式与版本策略见 internal/configfile,
// 完整设计见 .scratch/config-import-export/spec.md。
//
//	POST /settings/import/config/preview  只读预览:解析 + 逐条校验 + 判重,不写库
//	POST /settings/import/config          执行导入:按预览同一份口径落库
//
// 判重、覆盖与引用对齐都沿用 UptimeKuma 导入的既有口径:
//   - 判重键 = 类型 + 目标(http=URL / ping=host / tcp=host:port / push=名称);
//   - 默认只新建、跳过已存在的同目标监控;可选覆盖重写(文件是权威,见 applyOverwrite);
//   - 指派(节点 ID 属于各实例)一律由导入表单统一指定,文件里的 assignMode 只作记录;
//   - 通知渠道按**名称**对齐,缺失即新建。
type configImportReq struct {
	// Config 是配置文件原文(前端 JSON.parse 后原样回传)。用 RawMessage 是为了
	// 交给 configfile.Decode 做严格解码+版本闸门,而不是让本结构体替它解析。
	Config json.RawMessage `json:"config"`

	// 指派方式与新建监控一致(selected/all/exclude),默认 all。
	AssignMode       string   `json:"assignMode"`
	AssignedAgentIds []string `json:"assignedAgentIds"`
	ExcludedAgentIds []string `json:"excludedAgentIds"`

	// Overwrite 为真时,已存在的同目标监控会被文件里的配置整体重写(保留 ID 与历史)。
	Overwrite bool `json:"overwrite"`
	// ImportSettings 为假时不导入面板设置(保留期/状态格数/通知模板);缺省为真。
	ImportSettings *bool `json:"importSettings"`
}

// 单条监控的处置结果。
const (
	configStatusCreate    = "create"    // 新建
	configStatusUpdate    = "update"    // 已存在,将按文件覆盖重写
	configStatusDuplicate = "duplicate" // 已存在(或本次导入内重复),跳过
	configStatusInvalid   = "invalid"   // 校验不通过,跳过并给出原因
)

// 导入规模上限:防呆。上限存在的意义是别让一份手工拼出来的超大文件把同步导入
// (以及内存)拖垮;真实实例的量级是数百个监控。
const (
	maxConfigMonitors = 5000
	maxConfigChannels = 1000
)

// trimChannelNames 渠道名两侧空白一律去掉:对齐口径是 trim 后全等
// (与导出侧写入的名字一致)。
func trimChannelNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ---- 请求解析 ----

// readConfigImport 读取并解析请求体:配置文件走 configfile.Decode(严格解码 + 版本闸门),
// 失败时直接写响应并返回 false。
//
// 这里自己用 encoding/json 解请求体而不用 r.Parse:config 字段是 RawMessage,
// 需要原样交给 configfile.Decode 才能保住「未知字段直接报错」这条严格性。
func (a *API) readConfigImport(r *ghttp.Request) (*configImportReq, *configfile.File, bool) {
	var req configImportReq
	body := r.GetBody()
	if len(body) == 0 {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "请提供配置文件内容"})
		return nil, nil, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "请求体不是合法的 JSON: " + err.Error()})
		return nil, nil, false
	}
	if req.AssignMode = strings.ToLower(strings.TrimSpace(req.AssignMode)); req.AssignMode == "" {
		req.AssignMode = store.AssignModeAll
	}
	if req.AssignedAgentIds == nil {
		req.AssignedAgentIds = []string{}
	}
	if req.ExcludedAgentIds == nil {
		req.ExcludedAgentIds = []string{}
	}
	if len(req.Config) == 0 {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "请提供配置文件内容"})
		return nil, nil, false
	}
	file, err := configfile.Decode(req.Config)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": configDecodeMessage(err)})
		return nil, nil, false
	}
	return &req, file, true
}

// configDecodeMessage 把解析/版本错误翻译成可操作的中文提示。
// 版本过高这条是重点:必须让用户知道「不是文件坏了,是仪表盘该升级了」。
func configDecodeMessage(err error) string {
	var tooNew *configfile.TooNewError
	switch {
	case errors.As(err, &tooNew):
		return fmt.Sprintf("配置文件版本过高:文件为 v%d,当前仪表盘仅支持 v%d;"+
			"请升级仪表盘后再导入(高版本文件可能含本版本不认识的配置,直接导入会丢配置)",
			tooNew.File, tooNew.Supported)
	case errors.Is(err, configfile.ErrNotConfigFile):
		return "这不是 UptimeMesh 配置文件(缺少 kind 标识或标识不符);" +
			"请确认选择的是通过「导出配置文件」下载的 JSON"
	case errors.Is(err, configfile.ErrBadVersion):
		return "配置文件缺少有效的 configVersion:文件可能被手工改动过,请重新导出"
	default:
		return "配置文件解析失败: " + err.Error()
	}
}

// ---- 计划(预览与执行共用) ----

// plannedChannel 一条渠道配置的处置。
type plannedChannel struct {
	entry configfile.Channel
	// existing 非空表示本地已有同名渠道,导入时复用它(不新建)。
	existing *store.Channel
	// dupOf 非空表示本次文件里前面已经出现过同名渠道:只建/复用第一个,
	// 后面的都指向它(否则会给同一批监控建出一堆同名渠道)。
	dupOf *plannedChannel
	// message 非空表示这条无效,跳过原因。
	message string
}

// plannedMonitor 一条监控配置的处置。
type plannedMonitor struct {
	entry configfile.Monitor
	// monitor 是校验通过后的可入库文档;无效条目为 nil。
	monitor *store.Monitor
	// existing 非空表示库内已有同目标监控。
	existing *store.Monitor
	status   string
	message  string
	// channelNames 是校验后保留下来的渠道名(引用了无效渠道的名字已被剔除)。
	channelNames []string
	// tokenConflict 为真时,文件里的 push 上报令牌与库内其它监控冲突:新建时改用新
	// 生成的令牌,覆盖更新时保留原令牌(见 applyMonitors)。
	tokenConflict bool
	// target/typ 供预览展示(无效条目也算得出来)。
	target string
	typ    string
}

// configPlan 是一次导入的完整计划:预览直接渲染它,执行按它落库。
// 两边共用同一份计划,「预览说会建 3 个」与「真的建 3 个」才不会对不上。
type configPlan struct {
	file           *configfile.File
	req            configImportReq
	channels       []*plannedChannel
	monitors       []*plannedMonitor
	settings       *plannedSettings
	warnings       []string
	existingTokens map[string]bool
}

// plannedSettings 面板设置的处置(逐项独立:非法项只跳过,不影响监控导入)。
type plannedSettings struct {
	days          *int
	rounds        *int
	templates     *map[string]notifytmpl.Template
	currentDays   int
	currentRounds int
	// templateActions 按事件说明模板会怎么变(预览展示用)。
	templateActions []templateAction
	applied         []string
}

type templateAction struct {
	event  string
	action string // overwrite(写入文件里的模板)/ resetDefault(清回内置默认)/ ""
}

// planConfigImport 生成导入计划。只读:不写库、不改内存里的任何配置。
func (a *API) planConfigImport(ctx context.Context, file *configfile.File, req configImportReq) (*configPlan, error) {
	if len(file.Monitors) > maxConfigMonitors {
		return nil, fmt.Errorf("配置文件条目过多:监控 %d 个,上限 %d 个",
			len(file.Monitors), maxConfigMonitors)
	}
	if len(file.Channels) > maxConfigChannels {
		return nil, fmt.Errorf("配置文件条目过多:渠道 %d 个,上限 %d 个",
			len(file.Channels), maxConfigChannels)
	}
	plan := &configPlan{file: file, req: req, existingTokens: map[string]bool{}}

	// 1) 渠道:先校验,再与本地按名字对齐。
	localChannels, err := a.Store.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*store.Channel{}
	for _, c := range sortedChannelsByName(localChannels) {
		if _, dup := byName[c.Name]; dup {
			plan.warn("本地存在多个同名渠道「%s」,导入时只对齐到最早创建的那一个", c.Name)
			continue
		}
		byName[c.Name] = c
	}
	planned := map[string]*plannedChannel{}
	for _, entry := range file.Channels {
		cr := channelReq{Name: entry.Name, URL: entry.URL, BodyTemplate: entry.BodyTemplate}
		pc := &plannedChannel{entry: entry}
		if err := cr.validate(); err != nil {
			pc.message = err.Error()
		} else {
			pc.entry = configfile.Channel{Name: cr.Name, URL: cr.URL, BodyTemplate: cr.BodyTemplate,
				Enabled: entry.Enabled}
			pc.existing = byName[cr.Name]
			// 文件内重名:后一条指向第一条(第一条无效时由它自己来建)。
			if first, ok := planned[cr.Name]; ok && first.message == "" {
				pc.dupOf = first
			} else {
				planned[cr.Name] = pc
			}
		}
		plan.channels = append(plan.channels, pc)
	}

	// 2) 监控:实体身份(判重键)与可入库文档分开算 —— 无效条目也要能被判重与展示。
	existing, err := a.existingMonitorTargets(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range existing {
		if m.PushToken != "" {
			plan.existingTokens[m.PushToken] = true
		}
	}
	validChannels := map[string]bool{}
	for _, pc := range plan.channels {
		if pc.message == "" {
			validChannels[pc.entry.Name] = true
		}
	}
	seen := map[string]bool{}
	for _, entry := range file.Monitors {
		pm := &plannedMonitor{entry: entry}
		pm.channelNames = trimChannelNames(entry.ChannelNames)
		// 引用了无效渠道的监控照常导入,只是丢掉这条渠道引用(并在下面提示)。
		kept := make([]string, 0, len(pm.channelNames))
		for _, name := range pm.channelNames {
			if validChannels[name] || byName[name] != nil {
				kept = append(kept, name)
				continue
			}
			plan.warn("监控「%s」引用的通知渠道「%s」无效,已忽略该渠道", entry.Name, name)
		}
		pm.channelNames = kept

		// 身份(类型+目标)先算出来:无效条目也要参与判重与预览展示。
		identity := identityMonitor(entry)
		pm.typ, pm.target = identity.Type, urlOf(identity)
		key := dedupeKeyOf(identity)

		mr := configMonitorReq(entry, req, plan, &pm.tokenConflict)
		monitor, err := mr.validate(ctx, a.Store)
		if err != nil {
			pm.status, pm.message = configStatusInvalid, err.Error()
			plan.monitors = append(plan.monitors, pm)
			continue
		}
		monitor.Enabled = boolValue(entry.Enabled, true)
		pm.monitor = monitor

		switch {
		case existing[key] != nil:
			pm.existing = existing[key]
			if req.Overwrite {
				pm.status = configStatusUpdate
			} else {
				pm.status = configStatusDuplicate
				pm.message = "已存在相同目标的监控,已跳过(可勾选「覆盖已存在的同目标监控配置」改为重写)"
			}
		case seen[key]:
			pm.status = configStatusDuplicate
			pm.message = "本次导入中存在相同目标的监控,只导入第一条"
		default:
			pm.status = configStatusCreate
			seen[key] = true
		}
		// 令牌冲突只在**新建**时才是问题:跳过/覆盖都不改库内令牌,提示出来只是噪音。
		if pm.tokenConflict && pm.status == configStatusCreate {
			plan.warn("监控「%s」的上报令牌已被本实例的其它监控占用,将改用新生成的令牌"+
				"(需要把上报方改指向新的上报地址)", entry.Name)
		}
		plan.monitors = append(plan.monitors, pm)
	}

	// 3) 面板设置:逐项校验,非法项跳过并警告(不阻断监控导入)。
	if err := a.planConfigSettings(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// planConfigSettings 校验文件里的面板设置,并把「当前值 → 新值」的差异记录下来。
func (a *API) planConfigSettings(ctx context.Context, plan *configPlan) error {
	cfg := plan.file.Settings
	if cfg == nil {
		return nil
	}
	st, err := a.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	days := st.ResultRetentionDays
	if days <= 0 {
		days = store.DefaultResultRetentionDays
	}
	ps := &plannedSettings{currentDays: days, currentRounds: store.NormalizeStatusStripRounds(st.StatusStripRounds)}

	if cfg.ResultRetentionDays != nil {
		if v := *cfg.ResultRetentionDays; v < 1 || v > 365 {
			plan.warn("配置里的原始结果保留天数 %d 不在 1~365 之间,已跳过该项", v)
		} else {
			ps.days = &v
		}
	}
	if cfg.StatusStripRounds != nil {
		if v := *cfg.StatusStripRounds; v < store.MinStatusStripRounds || v > store.MaxStatusStripRounds {
			plan.warn("配置里的最近状态格数 %d 不在 %d~%d 之间,已跳过该项",
				v, store.MinStatusStripRounds, store.MaxStatusStripRounds)
		} else {
			ps.rounds = &v
		}
	}
	if cfg.NotifyTemplates != nil {
		// 语义:文件里出现这一节 = 通知模板**整体替换**(没有的事件回落内置默认)。
		out := map[string]notifytmpl.Template{}
		for ev, tpl := range *cfg.NotifyTemplates {
			if !notifytmpl.IsEvent(ev) {
				plan.warn("配置里的通知模板事件 %q 无法识别,已跳过该事件", ev)
				continue
			}
			tpl.Title = strings.TrimSpace(tpl.Title)
			tpl.Content = strings.TrimSpace(tpl.Content)
			if len(tpl.Title) > templateMaxLen || len(tpl.Content) > templateMaxLen {
				plan.warn("通知模板 %s 超过 %d 字符,已跳过该事件(该事件回落默认模板)", ev, templateMaxLen)
				continue
			}
			if tpl.Title == "" && tpl.Content == "" {
				continue // 与设置页同语义:两者皆空 = 用默认,不落库
			}
			out[ev] = tpl
		}
		ps.templates = &out
		// 逐事件给出"会发生什么":本地自定义而文件里没有的事件会被清回默认,必须明说。
		for _, ev := range notifytmpl.Events {
			_, fileHas := out[ev]
			_, localHas := st.NotifyTemplates[ev]
			switch {
			case fileHas:
				ps.templateActions = append(ps.templateActions, templateAction{event: ev, action: "overwrite"})
			case localHas:
				ps.templateActions = append(ps.templateActions, templateAction{event: ev, action: "resetDefault"})
			}
		}
	}
	plan.settings = ps
	return nil
}

func (p *configPlan) warn(format string, args ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

// identityMonitor 由文件条目造一个只含"身份字段"的监控文档,用于判重与展示。
// 类型/目标/端口/名称与导入后的实物一致,故 invalid 条目也能正确参与判重。
func identityMonitor(entry configfile.Monitor) *store.Monitor {
	return &store.Monitor{
		// 类型必须与 monitorReq.validate 的归一化口径一致(小写去空白),否则
		// 文件里写 "HTTP" 时判重键会与库内的 "http" 对不上,变成重复创建。
		Type: strings.ToLower(strings.TrimSpace(entry.Type)), Name: strings.TrimSpace(entry.Name),
		URL: entry.URL, TargetHost: entry.TargetHost, Port: entry.Port,
	}
}

// configMonitorReq 把文件条目 + 导入表单拼成一份 monitorReq,复用新建监控的校验口径
// (周期 10~3600、超时 ≤ 周期、URL 必须 http(s)、期望状态码可解析、JSONata 可编译、
// TCP 端口区间、下载速度单位与阈值……)。
//
// 缺字段回落默认值:周期 60s、超时 10s、阈值 100、连续 3 轮、启用 —— 与新建监控表单
// 及 UptimeKuma 导入一致。这是「低版本配置文件也能导入」的具体体现:老文件里没有的
// 字段按默认值补齐,而不是判无效。
func configMonitorReq(entry configfile.Monitor, req configImportReq,
	plan *configPlan, tokenConflict *bool) monitorReq {
	mr := monitorReq{
		Type: strings.ToLower(strings.TrimSpace(entry.Type)), Name: strings.TrimSpace(entry.Name),
		Group:       entry.Group,
		Period:      intValue(entry.Period, kumaDefaultPeriod),
		Timeout:     intValue(entry.Timeout, kumaDefaultTimeout),
		Threshold:   floatValue(entry.Threshold, kumaDefaultThreshold),
		Consecutive: intValue(entry.Consecutive, kumaDefaultConsecutive),
		URL:         entry.URL, Method: entry.Method, Headers: entry.Headers, Body: entry.Body,
		ExpectStatusSpecs: entry.ExpectStatusSpecs,
		Contains:          entry.ExpectContains, NotContains: entry.ExpectNotContains,
		AllowInsecure: boolValue(entry.AllowInsecureTLS, false),
		InvertMode:    boolValue(entry.InvertMode, false),
		IPVersion:     entry.IPVersion,
		JSONPath:      entry.JSONPath, JSONPathOperator: entry.JSONPathOperator,
		ExpectedValue: entry.JSONAssertExpected,
		TargetHost:    entry.TargetHost, Port: entry.Port, SpeedUnit: entry.SpeedUnit,
		// 指派:节点 ID 属于各实例,一律用导入表单的值;文件里的 assignMode 只作记录。
		AssignMode: req.AssignMode, AgentIds: req.AssignedAgentIds, ExcludeIds: req.ExcludedAgentIds,
	}
	// 通知渠道的 ID 在这里还定不下来(将要新建的渠道此刻没有 ID),由
	// applyChannels / applyMonitors 建好后按名字回填 —— 计划阶段只负责剔除无效引用。
	// push 上报令牌:能沿用就沿用(已部署的上报脚本不用改指向),冲突则另生成一个。
	// 只登记冲突事实,是否值得提示由调用方按这条监控的最终处置决定(见 planConfigImport)。
	if strings.ToLower(strings.TrimSpace(entry.Type)) == checkconfig.TypePush && entry.PushToken != "" {
		if plan.existingTokens[entry.PushToken] {
			*tokenConflict = true
		} else {
			plan.existingTokens[entry.PushToken] = true
			mr.PushToken = entry.PushToken
		}
	}
	enabled := boolValue(entry.Enabled, true)
	mr.Enabled = &enabled
	return mr
}

// sortedChannelsByName 按 (名称, 创建时间升序, ID) 排序:同名渠道取哪一个必须是确定的,
// 否则预览与执行在存在同名渠道时可能给出不同结果。
func sortedChannelsByName(list []*store.Channel) []*store.Channel {
	out := append([]*store.Channel{}, list...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.Hex() < out[j].ID.Hex()
	})
	return out
}

// ---- 预览 ----

// configImportPreview 只读预览:返回文件信息、逐条处置与设置差异,不写任何库。
func (a *API) configImportPreview(r *ghttp.Request) {
	req, file, ok := a.readConfigImport(r)
	if !ok {
		return
	}
	plan, err := a.planConfigImport(r.Context(), file, *req)
	if err != nil {
		writeConfigPlanError(r, err)
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": plan.previewView()})
}

func (p *configPlan) previewView() g.Map {
	var create, update, duplicate, invalid int
	items := make([]g.Map, 0, len(p.monitors))
	for _, pm := range p.monitors {
		switch pm.status {
		case configStatusCreate:
			create++
		case configStatusUpdate:
			update++
		case configStatusDuplicate:
			duplicate++
		default:
			invalid++
		}
		items = append(items, g.Map{
			"name": pm.entry.Name, "type": pm.typ, "target": pm.target,
			"group": pm.entry.Group, "status": pm.status, "message": pm.message,
		})
	}
	var channelsCreate, channelsReuse, channelsInvalid int
	channelItems := make([]g.Map, 0, len(p.channels))
	for _, pc := range p.channels {
		status := configStatusCreate
		switch {
		case pc.message != "":
			status = configStatusInvalid
			channelsInvalid++
		case pc.existing != nil || pc.dupOf != nil:
			// 复用本地同名渠道,或复用本次文件里前一条同名渠道。
			status = configStatusDuplicate
			channelsReuse++
		default:
			channelsCreate++
		}
		channelItems = append(channelItems, g.Map{
			"name": pc.entry.Name, "url": pc.entry.URL, "status": status, "message": pc.message,
		})
	}
	return g.Map{
		"file": g.Map{
			"kind": p.file.Kind, "configVersion": p.file.ConfigVersion,
			"exportedAt": p.file.ExportedAt, "supportedVersion": configfile.CurrentVersion,
		},
		"assignMode": p.req.AssignMode,
		"summary": g.Map{
			"monitors": len(p.monitors), "channels": len(p.channels),
			"create": create, "update": update, "duplicate": duplicate, "invalid": invalid,
			"channelsCreate": channelsCreate, "channelsReuse": channelsReuse,
			"channelsInvalid": channelsInvalid,
		},
		"settings": p.settingsView(),
		"monitors": items,
		"channels": channelItems,
		"warnings": stringList(p.warnings),
	}
}

// settingsView 设置差异:当前值 → 文件里的值,以及各事件模板会怎么变。
func (p *configPlan) settingsView() g.Map {
	if p.settings == nil || p.file.Settings == nil {
		return g.Map{"present": false, "changes": []g.Map{}, "templates": []g.Map{}}
	}
	ps := p.settings
	changes := []g.Map{}
	if ps.days != nil {
		changes = append(changes, g.Map{"key": "resultRetentionDays",
			"current": ps.currentDays, "next": *ps.days})
	}
	if ps.rounds != nil {
		changes = append(changes, g.Map{"key": "statusStripRounds",
			"current": ps.currentRounds, "next": *ps.rounds})
	}
	if ps.templates != nil {
		changes = append(changes, g.Map{"key": "notifyTemplates", "current": -1, "next": len(*ps.templates)})
	}
	templates := make([]g.Map, 0, len(ps.templateActions))
	for _, ta := range ps.templateActions {
		templates = append(templates, g.Map{"event": ta.event, "action": ta.action})
	}
	return g.Map{"present": true, "changes": changes, "templates": templates}
}

// ---- 执行 ----

// configImport 执行导入:按同一份计划落库。
//
// 顺序:先建/复用渠道(监控要引用它们的 ID),再逐条建/覆盖监控,最后应用面板设置。
// 逐条 best-effort、非事务(与 UptimeKuma 导入一致):一条失败不影响其它条目,
// 结果里如实回报 created/updated/skipped/invalid/failed。
func (a *API) configImport(r *ghttp.Request) {
	req, file, ok := a.readConfigImport(r)
	if !ok {
		return
	}
	ctx := r.Context()
	plan, err := a.planConfigImport(ctx, file, *req)
	if err != nil {
		writeConfigPlanError(r, err)
		return
	}
	importSettings := req.ImportSettings == nil || *req.ImportSettings

	channelIDs, channelsCreated, channelsReused, channelFailed, channelItems := a.applyChannels(ctx, plan)

	touched, counts, monitorItems := a.applyMonitors(ctx, plan, channelIDs)

	settingsApplied, err := a.applyConfigSettings(ctx, plan, importSettings)
	if err != nil {
		writeConfigPlanError(r, err)
		return
	}

	// 一次批量操作最多可能新建/覆盖数百个监控:合成一帧推给浏览器
	// (EvMonitorsChanged 载荷是不含状态条的轻量行,前端就地 upsert),不逐行发。
	a.BroadcastMonitorsChanged(ctx, touched)

	g.Log().Infof(ctx,
		"管理员 %s 导入配置文件(v%d):新建 %d、覆盖 %d、跳过 %d、无效 %d、失败 %d;"+
			"渠道新建 %d、复用 %d、失败 %d;面板设置 %v",
		r.GetCtxVar(ctxUserKey).String(), file.ConfigVersion,
		counts.created, counts.updated, counts.skipped, counts.invalid, counts.failed,
		channelsCreated, channelsReused, channelFailed, settingsApplied)

	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"file":    g.Map{"configVersion": file.ConfigVersion, "exportedAt": file.ExportedAt},
		"created": counts.created, "updated": counts.updated, "skipped": counts.skipped,
		"invalid": counts.invalid, "failed": counts.failed,
		"channelsCreated": channelsCreated, "channelsReused": channelsReused,
		"channelsFailed":  channelFailed,
		"settingsApplied": settingsApplied,
		"warnings":        stringList(plan.warnings),
		"monitors":        monitorItems,
		"channels":        channelItems,
	}})
}

// applyChannels 建/复用渠道,返回「名字 → 渠道 ID」(新建的渠道此刻才拿到 ID)
// 与各计数。渠道写失败不会让整份文件作废:引用它的监控少勾一个渠道即可。
func (a *API) applyChannels(ctx context.Context, plan *configPlan) (
	map[string]string, int, int, int, []g.Map) {
	nameToID := map[string]string{}
	items := make([]g.Map, 0, len(plan.channels))
	var created, reused, failed int
	for _, pc := range plan.channels {
		if pc.message != "" {
			failed++
			items = append(items, g.Map{"name": pc.entry.Name, "ok": false,
				"message": "渠道配置无效,已跳过: " + pc.message})
			continue
		}
		if pc.existing != nil {
			reused++
			nameToID[pc.entry.Name] = pc.existing.ID.Hex()
			items = append(items, g.Map{"name": pc.entry.Name, "ok": true, "id": pc.existing.ID.Hex(),
				"message": "已存在同名渠道,复用(不新建、不改动其配置)"})
			continue
		}
		if pc.dupOf != nil {
			// 文件里重名的后一条:指向前一条(前一条若写失败,这条也无渠道可用)。
			id, ok := nameToID[pc.dupOf.entry.Name]
			if !ok {
				failed++
				items = append(items, g.Map{"name": pc.entry.Name, "ok": false,
					"message": "与前一条同名渠道重复,且前一条未能入库"})
				continue
			}
			reused++
			nameToID[pc.entry.Name] = id
			items = append(items, g.Map{"name": pc.entry.Name, "ok": true, "id": id,
				"message": "与本次导入中前面的同名渠道合并"})
			continue
		}
		c := &store.Channel{Name: pc.entry.Name, URL: pc.entry.URL, BodyTemplate: pc.entry.BodyTemplate,
			Enabled: boolValue(pc.entry.Enabled, true)}
		if err := a.Store.InsertChannel(ctx, c); err != nil {
			// 渠道写失败不该让整份文件作废:引用它的监控会少一个渠道,并在下面提示。
			failed++
			plan.warn("通知渠道「%s」写入失败,引用它的监控不会勾选该渠道", pc.entry.Name)
			items = append(items, g.Map{"name": pc.entry.Name, "ok": false, "message": "写入失败"})
			continue
		}
		created++
		nameToID[pc.entry.Name] = c.ID.Hex()
		items = append(items, g.Map{"name": pc.entry.Name, "ok": true, "id": c.ID.Hex(),
			"message": "已创建"})
	}
	return nameToID, created, reused, failed, items
}

// configImportCounts 执行阶段的计数。
type configImportCounts struct {
	created, updated, skipped, invalid, failed int
}

// applyMonitors 逐条建/覆盖监控,返回「被改动的监控」(推送与响应行用)与结果明细。
func (a *API) applyMonitors(ctx context.Context, plan *configPlan,
	nameToID map[string]string) ([]*store.Monitor, configImportCounts, []g.Map) {
	var counts configImportCounts
	touched := []*store.Monitor{}
	items := make([]g.Map, 0, len(plan.monitors))
	for _, pm := range plan.monitors {
		if pm.status == configStatusInvalid {
			counts.invalid++
			items = append(items, g.Map{"name": pm.entry.Name, "type": pm.typ, "target": pm.target,
				"ok": false, "message": "配置无效,已跳过: " + pm.message})
			continue
		}
		if pm.status == configStatusDuplicate {
			counts.skipped++
			items = append(items, g.Map{"name": pm.entry.Name, "type": pm.typ, "target": pm.target,
				"ok": false, "message": pm.message})
			continue
		}
		m := pm.monitor
		m.ChannelIds = idsOfNames(pm.channelNames, nameToID)
		switch pm.status {
		case configStatusUpdate:
			// 覆盖重写:文件是权威(阈值/周期/请求细节/启用状态/渠道都按文件),但保留
			// 实例内的身份与历史:ID、创建时间、push 上报令牌(库内的才是已部署脚本用的),
			// 以及指派(由导入表单指定,见 configMonitorReq)。
			m.ID, m.CreatedAt = pm.existing.ID, pm.existing.CreatedAt
			m.PushToken = pm.existing.PushToken
			if err := a.Store.UpdateMonitor(ctx, m); err != nil {
				counts.failed++
				items = append(items, g.Map{"name": m.Name, "type": m.Type, "target": urlOf(m),
					"ok": false, "message": "覆盖更新失败"})
				continue
			}
			if !m.Enabled {
				// 覆盖成暂停时清掉连续破线计数(与单行暂停同语义:维护窗口不计入)。
				_ = a.Store.ResetMonitorsConsecutive(ctx, []store.ID{m.ID})
			}
			counts.updated++
			touched = append(touched, m)
			items = append(items, g.Map{"name": m.Name, "id": m.ID.Hex(), "type": m.Type,
				"target": urlOf(m), "ok": true,
				"message": "已存在,已按配置文件的配置重写(ID 与历史轮次保留,指派按本次导入设置)"})
		default:
			if err := a.Store.InsertMonitor(ctx, m); err != nil {
				counts.failed++
				items = append(items, g.Map{"name": m.Name, "type": m.Type, "target": urlOf(m),
					"ok": false, "message": "写入失败"})
				continue
			}
			counts.created++
			touched = append(touched, m)
			message := "已创建"
			if !m.Enabled {
				message = "已创建(配置里为暂停,导入后同样暂停)"
			}
			if pm.tokenConflict {
				message += ";上报令牌与已有监控冲突,已改用新令牌"
			}
			items = append(items, g.Map{"name": m.Name, "id": m.ID.Hex(), "type": m.Type,
				"target": urlOf(m), "ok": true, "message": message})
		}
	}
	return touched, counts, items
}

// idsOfNames 按渠道名取 ID(名字在计划阶段已确认有效;取不到就跳过,不会留下空引用)。
func idsOfNames(names []string, nameToID map[string]string) []string {
	ids := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		id, ok := nameToID[name]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// applyConfigSettings 应用面板设置,返回实际生效的项名。
//
// 非法项在计划阶段已被剔除,这里只写合法值;任何一项写失败都不影响其它项与监控导入
// (设置是附属内容,不值得让整份文件作废)。
func (a *API) applyConfigSettings(ctx context.Context, plan *configPlan, importSettings bool) ([]string, error) {
	applied := []string{}
	if plan.settings == nil || !importSettings {
		return applied, nil
	}
	ps := plan.settings
	if ps.days != nil {
		if err := a.Store.SetResultRetentionDays(ctx, *ps.days); err != nil {
			plan.warn("原始结果保留天数写入失败: %v", err)
		} else {
			applied = append(applied, "resultRetentionDays")
		}
	}
	if ps.rounds != nil {
		if err := a.Store.SetStatusStripRounds(ctx, *ps.rounds); err != nil {
			plan.warn("最近状态格数写入失败: %v", err)
		} else {
			applied = append(applied, "statusStripRounds")
		}
	}
	if ps.templates != nil {
		// 整体替换:文件里没有的事件即回落内置默认(与设置页「清空 = 恢复默认」同源)。
		if err := a.Store.SetNotifyTemplates(ctx, *ps.templates); err != nil {
			plan.warn("通知模板写入失败: %v", err)
		} else {
			applied = append(applied, "notifyTemplates")
		}
	}
	return applied, nil
}

// writeConfigPlanError 把计划阶段的内部错误翻成响应。
func writeConfigPlanError(r *ghttp.Request, err error) {
	// 条目超限是用户可修正的输入问题,给 400;其余是内部失败。
	if strings.HasPrefix(err.Error(), "配置文件条目过多") {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	g.Log().Errorf(r.Context(), "导入配置文件失败: %v", err)
	r.Response.WriteJsonExit(g.Map{"code": 500, "message": "导入失败"})
}

// stringList 保证 JSON 里是 [] 而不是 null(前端直接 .length / v-for)。
func stringList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ---- 缺省值辅助 ----

func boolValue(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func intValue(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func floatValue(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}
