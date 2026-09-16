package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/hub"
	"github.com/uptimemesh/dashboard/internal/scheduler"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/probe"
	"github.com/uptimemesh/shared/protocol"
)

// testMonitorWaitExtra 是等待节点回测试结果时,在探测超时之外多给的余量:
// 节点侧的执行上限就是 check.timeout_seconds,再多留一点网络与调度时间。
const testMonitorWaitExtra = 5 * time.Second

// testMonitor 用弹窗里**还没保存**的探测配置,让一个在线节点当场跑一次,并把请求与
// 返回明细回给页面(POST /monitors/test,监控弹窗的「测试」按钮)。
//
// 为什么不落库、不建轮次:这是"配置调试"动作,不是一次真实探测。若结果进了结果表,
// 可用率、状态条与告警判定都会被一次人工点击污染 —— 轮次只由调度器按周期创建(ADR-0003)。
// Dashboard 自己也不执行探测(ADR-0002):它只负责挑节点、下发配置、把结果带回来,
// 与真实轮次走的是同一条链路(同一个 CheckPayload、同一个 shared/probe 判定口径)。
func (a *API) testMonitor(r *ghttp.Request) {
	var req monitorReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法: " + err.Error()})
		return
	}
	ctx := r.Context()
	m, err := testCheckOf(&req)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	check, err := scheduler.CheckPayload(m)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	agent, err := a.pickTestAgent(ctx, &req, strings.TrimSpace(r.Get("testAgentId").String()))
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	wait := time.Duration(m.Timeout)*time.Second + testMonitorWaitExtra
	res, err := a.Hub.RunProbeTest(ctx, agent.ID, protocol.ProbeTestPayload{
		TestID:      store.NewID().Hex(),
		MonitorType: m.Type,
		Check:       check,
	}, wait)
	if err != nil {
		// 请求被客户端取消(关弹窗/关页面):没有收件人,也不必再回包。
		if ctx.Err() != nil {
			return
		}
		// 节点在线却没回:能力协商本应挡住老版本,这里是兜底(节点刚降级、链路卡住)。
		// 仍然回 code=0 的结果,页面直接把这句话显示在测试面板里。
		if errors.Is(err, hub.ErrTestTimeout) {
			r.Response.WriteJsonExit(g.Map{"code": 0, "data": testResultView(agent, m, nil,
				fmt.Sprintf("节点 %s 在 %s 内没有返回测试结果:请确认节点已升级到新版并在线", agent.Name, wait))})
			return
		}
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "测试未完成: " + err.Error()})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": testResultView(agent, m, res, "")})
}

// testCheckOf 用表单值组装探测配置(只组装探测相关字段)。
//
// 复用保存路径的**类型级**校验(URL、期望状态码、JSON 断言、主机/端口),但不要求名称、
// 周期与指派:测试的对象是探测本身,常见用法是先填地址点测试、确认通了再补名称。
func testCheckOf(req *monitorReq) (*store.Monitor, error) {
	m := &store.Monitor{
		Type:       strings.ToLower(strings.TrimSpace(req.Type)),
		Timeout:    req.Timeout,
		InvertMode: req.InvertMode,
	}
	if m.Timeout < 1 {
		m.Timeout = 10 // 与表单/后端默认值一致,免得测试等不到超时先被 Dashboard 放弃
	}
	if err := probe.ValidateTestType(m.Type); err != nil {
		return nil, err
	}
	// IP 协议族与保存路径同款校验:测试要测的必须是真正会下发的那份配置。
	ipVersion, err := ipVersionOf(req.IPVersion)
	if err != nil {
		return nil, err
	}
	m.IPVersion = ipVersion
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
	}
	return m, nil
}

// pickTestAgent 挑一个能执行测试的节点:已批准 + 在线 + 声明了 probe_test 能力,
// 并且落在监控的指派范围内(测试的就是这批节点将来真正会跑的东西)。
//
// prefer 非空表示用户指定了节点(接口支持,当前页面不传);指定的节点不可用时报错,
// 而不是悄悄换一个 —— "我以为在 A 上测的"是最容易误判的坑。
func (a *API) pickTestAgent(ctx context.Context, req *monitorReq, prefer string) (*store.Agent, error) {
	if a.Hub == nil {
		return nil, errors.New("节点通道未初始化,无法执行测试")
	}
	approved, err := a.Store.FindAgentsByStatus(ctx, store.AgentApproved)
	if err != nil {
		return nil, errors.New("读取节点列表失败")
	}
	if len(approved) == 0 {
		return nil, errors.New("还没有已批准的节点,无法测试:请先在「节点」页接入并批准一个节点")
	}
	mode := strings.TrimSpace(req.AssignMode)
	if mode == "" {
		mode = store.AssignModeSelected
	}
	if mode == store.AssignModeSelected && len(req.AgentIds) == 0 {
		return nil, errors.New("「指定节点」模式下请先勾选节点:测试需要至少一个在线节点执行")
	}
	inScope := func(hex string) bool {
		switch mode {
		case store.AssignModeSelected:
			return containsString(req.AgentIds, hex)
		case store.AssignModeExclude:
			return !containsString(req.ExcludeIds, hex)
		default: // all:全部已批准节点
			return true
		}
	}
	var scoped, online, capable int
	var first, preferred *store.Agent
	for _, ag := range approved {
		hex := ag.ID.Hex()
		if !inScope(hex) {
			continue
		}
		scoped++
		if !a.Hub.IsOnline(ag.ID) {
			continue
		}
		online++
		if !ag.Supports(protocol.CapProbeTest) {
			continue // 老版本不认识 probe_test 帧:发过去只会石沉大海
		}
		capable++
		if first == nil {
			first = ag
		}
		if hex == prefer {
			preferred = ag
		}
	}
	pick := first
	if prefer != "" {
		pick = preferred
	}
	if pick == nil {
		switch {
		case scoped == 0:
			return nil, errors.New("指派范围内没有已批准的节点,请调整指派设置")
		case online == 0:
			return nil, errors.New("指派范围内的节点都不在线:测试必须由在线节点执行")
		case capable == 0:
			return nil, errors.New("在线节点的版本过旧(缺少测试能力):请在「节点」页一键升级后再试")
		default:
			return nil, errors.New("指定的节点当前不可用于测试(需在线且已升级)")
		}
	}
	return pick, nil
}

// testResultView 把节点回传的测试结果摊成页面直接可用的结构。
//
// ok 是**有效判定**(已按反转模式折算,页面据此决定红/绿),probeOk 是节点的原始判定;
// 反转模式下两者相反,页面要能说清"这次是探测成功,但在反转模式里算故障"。
// res 为 nil 表示测试没能拿到结果(超时),此时只有 error 一句话。
func testResultView(agent *store.Agent, m *store.Monitor, res *protocol.ProbeTestResultPayload, failMsg string) g.Map {
	view := g.Map{
		"agentId": agent.ID.Hex(), "agentName": agent.Name,
		"type": m.Type, "invertMode": m.InvertMode,
		// speedUnit 供前端把 speedKbps 换算成用户配置的单位展示(download 类型才有意义)。
		"speedUnit": m.SpeedUnit,
		"request":   g.Map{}, "response": g.Map{},
	}
	if res == nil {
		view["ok"] = false
		view["probeOk"] = false
		view["error"] = failMsg
		return view
	}
	effective := res.OK
	if m.InvertMode {
		effective = !res.OK
	}
	view["ok"] = effective
	view["probeOk"] = res.OK
	view["latencyMs"] = round2(res.LatencyMs)
	view["httpStatus"] = res.HTTPStatus
	view["error"] = res.Error
	// 下载速度监控的测试结果要带上测得的平均速度与字节数(弹窗展示"有多快")。
	view["speedKbps"] = round2(res.SpeedKbps)
	view["bytes"] = res.Bytes
	if d := res.Detail; d != nil {
		view["request"] = g.Map{
			"method": d.Method, "url": d.URL, "headers": d.Headers,
			"body": d.Body, "target": d.Target,
		}
		view["response"] = g.Map{
			"status": d.Status, "statusText": d.StatusText, "headers": d.RespHeaders,
			"bodyExcerpt": d.BodyExcerpt, "bodyBytes": d.BodyBytes,
			"bodyTruncated": d.BodyTruncated, "bodyNote": d.BodyNote,
			"finalUrl": d.FinalURL, "redirects": d.Redirects,
		}
	}
	return view
}

// containsString 小工具:两个短列表的包含判断(指派/排除范围)。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
