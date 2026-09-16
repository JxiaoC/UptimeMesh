// Scheduler 按监控周期创建轮次、经 hub 下发任务、到点收口定稿。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/alert"
	"github.com/uptimemesh/dashboard/internal/hub"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// DefaultGraceSeconds 轮次收集宽限期默认值(可在 settings.roundGraceSeconds 配置)。
const DefaultGraceSeconds = 5

// PongTimeout 定稿探活单次等待。
const PongTimeout = 3 * time.Second

// serialDispatchSlack 串行下发时单个节点的额外预算:节点侧的执行上限是
// check.timeout_seconds(下载测速到点必回失败结果),这里再留出下发/回传的网络时间。
// 既用于"等上一个节点"的耐心值,也参与轮次截止时间的顺延计算。
const serialDispatchSlack = 3 * time.Second

// MonitorSource 调度器读取监控配置的存储接口。
type MonitorSource interface {
	ListEnabledMonitors(ctx context.Context) ([]*store.Monitor, error)
	FindMonitorByID(ctx context.Context, id store.ID) (*store.Monitor, error)
}

// monitorSlot 是调度器为每个监控维护的建轮节流状态。
//
// due 是"下一次应当建轮的计划时刻"。建轮后它按周期**整拍**推进(due += period),
// 而不是记成"这一拍实际建轮的时刻" —— 后者会把每一拍的 tick 抖动/延迟都吃进基准,
// 于是每拍只会更晚:线上实测 60 秒周期约 57% 的间隔变成 61 秒,3 天累计漂移近 40 分钟
// (见 .scratch/schedule-drift/spec.md)。
//
// period 是写下 due 时的周期(秒)。正常改周期走 API 的 Kick(它直接清掉槽位),
// 这里只是兜底:配置被别处直接改写时也认得出周期变了,重新起步。
//
// push(外部上报)监控也共用这个槽位,但那里的 due 是"上一轮静默轮的时刻",只作节流用
// (见 push.go 的 cyclePush)。
type monitorSlot struct {
	due    time.Time
	period int
}

type Scheduler struct {
	st  *store.Store
	h   *hub.Hub
	src MonitorSource

	tick     time.Duration
	mu       sync.Mutex
	rounds   map[store.ID]*InFlightRound // roundID -> 在途轮次
	monitors map[store.ID]monitorSlot    // monitorID -> 下次到期时刻
	grace    time.Duration               // 收集宽限期,来自 settings,cycle 刷新

	// OnRoundFinalized 每次轮次定稿(含 UNKNOWN)且告警状态机更新后回调。
	// ev 非 nil 表示发生 UP/DOWN 翻转;st 是该轮之后的监控状态(展示色块与连续破线数
	// 的推送要用)。票 07 notifier 与票 10 浏览器推送共用。
	// 载荷里还要带总览卡片就地更新所需的字段(本轮平均延时、各节点明细),以及翻转时
	// 那条状态变动记录的身份(ev.RoundID/FromState/ChangedAt —— 调度器回填,
	// 浏览器据此把一行插进总览的变动流水,不必再拉接口)。
	OnRoundFinalized func(ctx context.Context, m *store.Monitor, agg Aggregation, ev *alert.Event, st *store.MonitorState)
}

// New 默认 1s tick;测试可缩短。
func New(st *store.Store, h *hub.Hub, src MonitorSource) *Scheduler {
	s := &Scheduler{
		st: st, h: h, src: src,
		tick:     time.Second,
		grace:    DefaultGraceSeconds * time.Second,
		rounds:   map[store.ID]*InFlightRound{},
		monitors: map[store.ID]monitorSlot{},
	}
	h.OnProbeResult = s.handleResult
	return s
}

// Run 阻塞运行调度循环,ctx 取消退出。启动先收口上次进程残留的 OPEN 轮次
// (按已入库的按时样本复算,见 store.CloseOrphanOpenRounds)。
func (s *Scheduler) Run(ctx context.Context) {
	if n, err := s.st.CloseOrphanOpenRounds(ctx); err != nil {
		g.Log().Errorf(ctx, "收口残留 OPEN 轮次失败: %v", err)
	} else if n > 0 {
		g.Log().Infof(ctx, "已收口 %d 个残留 OPEN 轮次", n)
	}
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cycle(ctx)
		}
	}
}

// cycle 一轮调度:创建到期的轮次 + 定稿过期的轮次。
func (s *Scheduler) cycle(ctx context.Context) {
	now := time.Now()
	// 刷新收集宽限期(票 06:settings.roundGraceSeconds 可配置)。
	if st, err := s.st.GetSettings(ctx); err == nil && st.RoundGraceSeconds > 0 {
		s.grace = time.Duration(st.RoundGraceSeconds) * time.Second
	}
	monitors, err := s.src.ListEnabledMonitors(ctx)
	if err != nil {
		g.Log().Errorf(ctx, "读取启用监控失败: %v", err)
		return
	}
	for _, m := range monitors {
		// push(外部上报)监控不走"下发探测"这条路:轮次由上报端点或静默看门狗产生。
		if m.Type == checkconfig.TypePush {
			s.cyclePush(ctx, m, now)
			continue
		}
		// 下载速度监控一轮里逐节点串行测速,一轮耗时随节点数变长:上一轮还没定稿时
		// 不再开新的一轮(否则两轮的节点会同时在测速,又把带宽分掉)。
		inflight := m.Type == checkconfig.TypeDownload && s.hasInFlightRound(m.ID)
		s.mu.Lock()
		slot, seen := s.monitors[m.ID]
		s.mu.Unlock()
		fire, planAt, next := planRound(slot, seen, now, time.Duration(m.Period)*time.Second, inflight)
		s.mu.Lock()
		s.monitors[m.ID] = next
		s.mu.Unlock()
		if fire {
			s.createRound(ctx, m, planAt, now)
		}
	}
	// 定稿到期轮次。
	s.mu.Lock()
	due := make([]store.ID, 0)
	for rid := range s.rounds {
		due = append(due, rid)
	}
	s.mu.Unlock()
	for _, rid := range due {
		s.mu.Lock()
		r := s.rounds[rid]
		s.mu.Unlock()
		if r != nil && !r.Closed() && now.After(r.Spec.Deadline) {
			s.finalize(ctx, r)
		}
	}
}

// CheckPayload 把监控配置序列化成下发给节点的探测配置(checkconfig.HTTP/Ping/TCP)。
// 建轮次与「监控弹窗的测试按钮」共用本函数:测试要测的必须是**真正会下发的那份配置**,
// 两处各写一遍迟早会漂移(线上会出现"测试通过、轮次却总是失败")。
func CheckPayload(m *store.Monitor) (json.RawMessage, error) {
	switch m.Type {
	case checkconfig.TypeHTTP:
		return json.Marshal(checkconfig.HTTP{
			Method: m.Method, URL: m.URL, Headers: m.Headers, Body: m.Body,
			TimeoutSeconds: m.Timeout, ExpectStatusCodes: m.ExpectStatusCodes,
			ExpectContains: m.ExpectContains, ExpectNotContains: m.ExpectNotContains,
			AllowInsecureTLS: m.AllowInsecureTLS,
			IPVersion:        m.IPVersion,
			JSONPath:         m.JsonPath, JSONPathOperator: m.JsonPathOperator,
			ExpectedValue: m.JsonAssertExpect,
		})
	case checkconfig.TypePing:
		return json.Marshal(checkconfig.Ping{
			Host: m.TargetHost, TimeoutSeconds: m.Timeout, IPVersion: m.IPVersion,
		})
	case checkconfig.TypeTCP:
		return json.Marshal(checkconfig.TCP{
			Host: m.TargetHost, Port: m.Port, TimeoutSeconds: m.Timeout,
			IPVersion: m.IPVersion,
		})
	case checkconfig.TypeDownload:
		// 下载速度监控:请求侧参数与 HTTP 相同,但不下发阈值(判定在 Dashboard 侧,
		// 依据是节点回传的平均速度与本轮阈值)。
		return json.Marshal(checkconfig.Download{
			Method: m.Method, URL: m.URL, Headers: m.Headers, Body: m.Body,
			TimeoutSeconds: m.Timeout, AllowInsecureTLS: m.AllowInsecureTLS,
			IPVersion: m.IPVersion,
		})
	case checkconfig.TypePush:
		// push 监控没有 Agent 探测,轮次由外部上报/静默看门狗产生(见 push.go)。
		return nil, errors.New("push 监控不需要下发探测任务")
	}
	return nil, errors.New("不支持的监控类型: " + m.Type)
}

// assignedAgentIDs 返回本轮应下发的节点 ID,由监控的指派模式决定:
//
//	selected(或空):用配置里的固定快照;
//	all:建轮时解析当前全部已批准节点,后续新增自动纳入;
//	exclude:同上再剔除 ExcludedAgentIds,过滤项始终排除。
func (s *Scheduler) assignedAgentIDs(ctx context.Context, m *store.Monitor) ([]string, error) {
	if m.AssignMode != store.AssignModeAll && m.AssignMode != store.AssignModeExclude {
		return m.AssignedAgentIds, nil
	}
	agents, err := s.st.FindAgentsByStatus(ctx, store.AgentApproved)
	if err != nil {
		return nil, err
	}
	excluded := make(map[string]bool, len(m.ExcludedAgentIds))
	for _, id := range m.ExcludedAgentIds {
		excluded[id] = true
	}
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		hex := a.ID.Hex()
		if excluded[hex] {
			continue
		}
		ids = append(ids, hex)
	}
	return ids, nil
}

// hasInFlightRound 该监控是否还有未定稿的轮次(下载速度监控据此避免两轮同时测速)。
func (s *Scheduler) hasInFlightRound(monitorID store.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rounds {
		if r.Spec.MonitorID == monitorID.Hex() {
			return true
		}
	}
	return false
}

// Kick 让某监控立刻进入下一轮:清掉它的建轮节流标记,下一个调度 tick(≤1s)就建轮,
// 而不是干等满一个周期(周期最大 3600 秒)。
//
// 用在两处:①监控配置被编辑保存后 —— 用户改完就想看到新配置的结果,而不是等一个周期;
// ②监控从暂停恢复后 —— 卡片上的状态与可用率都是暂停前的旧值,恢复后应当马上刷新。
//
// 不在这里直接建轮:轮次与下发只有调度协程一个写入者(ADR-0003),这里只把
// "该马上跑一轮"告诉它。若该监控已有在途轮次(下载速度监控的串行口径),调度器会等
// 它定稿后再建新轮,不会两轮同时测速。
func (s *Scheduler) Kick(monitorID store.ID) {
	s.KickAll([]store.ID{monitorID})
}

// KickAll 批量版(批量恢复用):一次清掉若干个监控的建轮节流标记。
func (s *Scheduler) KickAll(monitorIDs []store.ID) {
	if len(monitorIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range monitorIDs {
		delete(s.monitors, id)
	}
}

// planRound 决定这一拍该不该为某监控建轮,并给出本轮的**计划时间**与下一拍到期的状态。
//
//	seen=false(首见,或刚被 Kick 清掉槽位) → 立刻建轮,以此刻为基准起步
//	now <  due                                → 还没到点
//	now >= due 且上一轮未定稿                 → 保持"到期"状态,等它定稿后立刻建轮
//	now >= due                                → 建轮,计划时间就是 due,due += period
//
// 关键在 due 的推进方式:一律 `due += period`,而不是 `due = now + period`。
// 后者会把"这一拍晚了多久"吃进基准,每一拍都只会更晚 —— 线上 60 秒周期约 57% 的间隔
// 变成 61 秒、3 天累计漂移近 40 分钟,就是这么来的(见 .scratch/schedule-drift/spec.md)。
//
// 计划时间取 due(而不是实际建轮的时刻 now):轮次的计划时间于是稳定落在整拍网格上
// (60 秒周期就是 …11:11:11 / 11:12:11 / 11:13:11),实际的建轮与下发只会晚于它不到
// 一个 tick;小时聚合、延时曲线与状态条也以这条网格为准。
//
// 落后一整个周期以上(进程卡住、机器休眠、下载轮跑了很久)时不补跑错过的轮次,而是把
// 本轮的计划时间与下一拍基准一起锚到此刻 —— 否则定稿后会连开数轮,把目标打一波。
func planRound(slot monitorSlot, seen bool, now time.Time, period time.Duration,
	inflight bool) (fire bool, planAt time.Time, next monitorSlot) {
	stepped := monitorSlot{due: now.Add(period), period: int(period / time.Second)}
	if !seen || slot.period != stepped.period {
		// 首见,或周期被改过:以此刻为基准起步。上一轮还开着(下载速度监控)时先记下
		// "此刻就该跑",等它定稿后立刻建轮。
		if inflight {
			return false, time.Time{}, monitorSlot{due: now, period: stepped.period}
		}
		return true, now, stepped
	}
	if now.Before(slot.due) {
		return false, time.Time{}, slot
	}
	if inflight {
		// 到期了但上一轮还没定稿:槽位原样保留(仍处于到期状态)。这里不重锚,
		// 否则一轮比周期还长的下载监控会被一拍一拍地无限往后推。
		return false, time.Time{}, slot
	}
	due := slot.due.Add(period)
	if !due.After(now) {
		// 已经落后一整个周期以上:不补跑错过的轮次,本轮就按此刻重新起步。
		return true, now, monitorSlot{due: now.Add(period), period: slot.period}
	}
	return true, slot.due, monitorSlot{due: due, period: slot.period}
}

// createRound 建轮次记录并逐个在线节点下发任务。
//
// planAt 是本轮的计划时间(整拍网格上的到期时刻,写进轮次与结果的分桶口径);
// now 是此刻,只用来算截止时间 —— 节点侧的探测预算不该被 tick 抖动吃掉。
func (s *Scheduler) createRound(ctx context.Context, m *store.Monitor, planAt, now time.Time) {
	check, err := CheckPayload(m)
	if err != nil {
		g.Log().Errorf(ctx, "监控 %s 探测配置序列化失败: %v", m.Name, err)
		return
	}
	agentIDs, err := s.assignedAgentIDs(ctx, m)
	if err != nil {
		g.Log().Errorf(ctx, "监控 %s 解析指派节点失败: %v", m.Name, err)
		return
	}
	// 下载速度监控:一轮里逐节点串行测速(同一时刻只有一个节点在下载),
	// 因此截止时间必须按节点数顺延 —— 否则排在后面的节点还没轮到就被定稿判成缺样。
	serial := m.Type == checkconfig.TypeDownload
	deadline := now.Add(time.Duration(m.Timeout)*time.Second + s.grace + PongTimeout)
	if serial {
		deadline = now.Add(time.Duration(len(agentIDs))*s.perProbeBudget(m) + s.grace + PongTimeout)
		// 串行测速的一轮可能远长于周期(节点越多越长),留一条日志便于解释"这轮怎么这么久"。
		g.Log().Debugf(ctx, "监控 %s 逐节点串行测速:%d 个节点,本轮截止在 %s 后",
			m.Name, len(agentIDs), deadline.Sub(now).Round(time.Second))
	}
	round := &store.Round{
		MonitorID: m.ID, AssignedAgentIds: append([]string{}, agentIDs...),
		ScheduledAt: planAt, Deadline: deadline, State: store.RoundStateOpen,
		TotalAgents: len(agentIDs),
	}
	if err := s.st.InsertRound(ctx, round); err != nil {
		g.Log().Errorf(ctx, "轮次落库失败: %v", err)
		return
	}
	spec := RoundSpec{
		RoundID: round.ID.Hex(), MonitorID: m.ID.Hex(),
		AgentIDs: round.AssignedAgentIds, Deadline: deadline, ScheduledAt: planAt,
		Threshold: m.Threshold, Consecutive: m.Consecutive,
		Invert: m.InvertMode, SerialDispatch: serial,
		// 补发判断用(见 AgentOnline):剩余时间不足一次探测就不补发。
		ProbeTimeout: time.Duration(m.Timeout) * time.Second,
	}
	// 下载速度监控:判定值换成本次平均速度,阈值随之换算成同一单位(KB/s),
	// 于是状态机、状态条与通知都只比较数值大小,不必各写一套分支。
	if m.Type == checkconfig.TypeDownload {
		spec.Metric = MetricSpeed
		spec.Threshold = checkconfig.ToKbps(m.Threshold, m.SpeedUnit)
		spec.Invert = false
	}

	task, err := protocol.NewEnvelope(protocol.FrameProbeTask, protocol.ProbeTaskPayload{
		RoundID: spec.RoundID, MonitorID: spec.MonitorID,
		MonitorName: m.Name, MonitorType: m.Type,
		Check: check, DeadlineUnix: deadline.Unix(),
	})
	if err != nil {
		return
	}
	flight := NewRound(spec)
	// 任务帧留一份在轮次里:节点重连时靠它补发(见 AgentOnline)。
	flight.SetTask(task)
	s.mu.Lock()
	s.rounds[round.ID] = flight
	s.mu.Unlock()

	if serial {
		// 逐节点串行:先发第一个,之后由这个 goroutine 等结果/等超时再发下一个;
		// 轮次定稿(Close 关闭 closedCh)或进程退出时它自然结束,不会泄漏。
		go s.dispatchSerial(ctx, m, flight, task, agentIDs)
		return
	}
	for _, sid := range agentIDs {
		s.sendTask(ctx, m, flight, sid, task)
	}
}

// sendTask 向单个节点下发本轮探测任务并记录"已下发";失败只记日志并返回 false。
// 下发失败的节点在定稿时按「任务没交出去」参与判定(探活无响应 ⇒ 离线缺样,在线 ⇒
// 未派发任务,两者都不计分母,见 finalize),而且节点重连后还有一次补发机会
// (见 AgentOnline)。
func (s *Scheduler) sendTask(ctx context.Context, m *store.Monitor, flight *InFlightRound,
	agentHex string, task protocol.Envelope) bool {
	aid, err := store.IDFromHex(agentHex)
	if err != nil {
		return false
	}
	if err := s.h.SendToAgent(aid, task); err != nil {
		g.Log().Debugf(ctx, "轮次下发节点 %s 失败(监控 %s): %v", agentHex, m.Name, err)
		return false
	}
	flight.MarkDispatched(agentHex)
	return true
}

// AgentOnline 节点(重新)接入时调用:把还在途、余量够、且该节点从未收到任务的那些
// 轮次补发出去。
//
// 为什么必须有它:重启后的**首轮**任务是在"进程刚起、hub 里一条连接都没有"时下发的,
// 于是全部下发失败;十几秒后节点重连回来,若这一轮还开着,样本本来是可以救回来的。
// 不补发的后果(改动前)不只是少一个样本:定稿探活此时能 ping 通节点,于是这些轮次被
// 判成"节点活着却不报" ⇒ 0/N = 0% ⇒ 重启后几乎所有监控凭空多一轮 DOWN
// (见 .scratch/restart-first-round/spec.md)。
//
// 只补发**来得及**的:剩余时间不足一次探测(ProbeTimeout)时不发 —— 发出去也只能得到
// 一张不入聚合的晚到结果,却让节点白打一次目标。来不及的轮次由定稿按"未下发 ⇒ 不计
// 分母"收口,下个周期照常。
//
// 下载速度监控(SerialDispatch)跳过:它一轮里同一时刻只允许一个节点在测速
// (下载由 dispatchSerial 独占下发),补发会破坏该口径。
func (s *Scheduler) AgentOnline(agentID store.ID) {
	ctx := context.Background()
	hex := agentID.Hex()
	now := time.Now()
	s.mu.Lock()
	flights := make([]*InFlightRound, 0, len(s.rounds))
	for _, r := range s.rounds {
		if !r.Spec.SerialDispatch {
			flights = append(flights, r)
		}
	}
	s.mu.Unlock()
	for _, flight := range flights {
		if flight.Closed() || flight.Dispatched(hex) {
			continue
		}
		wanted := false
		for _, id := range flight.UndispatchedAgents() {
			if id == hex {
				wanted = true
				break
			}
		}
		if !wanted || flight.Spec.Deadline.Sub(now) < flight.Spec.ProbeTimeout {
			continue
		}
		task, ok := flight.Task()
		if !ok {
			continue
		}
		// 监控可能已被删除:补发只为记日志用它的名字,查不到就跳过。
		m, err := s.src.FindMonitorByID(ctx, mustID(flight.Spec.MonitorID))
		if err != nil {
			continue
		}
		// 再确认一次没被定稿:并发下这里仍可能与 finalize 擦肩而过,
		// 代价只是节点多跑一次、结果按晚到入库,不影响口径。
		if flight.Closed() {
			continue
		}
		if s.sendTask(ctx, m, flight, hex, task) {
			g.Log().Debugf(ctx, "节点 %s 重连,补发轮 %s 的探测任务(监控 %s)",
				hex, flight.Spec.RoundID[:6], m.Name)
		}
	}
}

// perProbeBudget 串行下发时单个节点的最坏耗时:探测超时 + 网络余量。
func (s *Scheduler) perProbeBudget(m *store.Monitor) time.Duration {
	return time.Duration(m.Timeout)*time.Second + serialDispatchSlack
}

// dispatchSerial 逐节点串行下发(下载速度监控):发一个,等它的结果回来或等到
// perProbeBudget 超时,再发下一个。等超时的节点不再等(结果若晚到按迟到入库,
// 一直没回则在定稿探活里判成缺样)—— 不能让一个不吭声的节点卡住整轮。
func (s *Scheduler) dispatchSerial(ctx context.Context, m *store.Monitor, flight *InFlightRound,
	task protocol.Envelope, agentIDs []string) {
	patience := s.perProbeBudget(m)
	for i, sid := range agentIDs {
		// 轮次已定稿或调度器已退出:不再往下发(轮次由 cycle 收口)。
		if flight.Closed() || ctx.Err() != nil {
			return
		}
		// 节点离线(下发失败)时直接跳到下一个:它不会回结果,等它只会白等一个耐心值。
		sent := s.sendTask(ctx, m, flight, sid, task)
		if i == len(agentIDs)-1 || !sent {
			continue
		}
		if !flight.AwaitResult(ctx, sid, patience) {
			g.Log().Debugf(ctx, "轮 %s 等节点 %s 测速超过 %s,继续下一个",
				flight.Spec.RoundID[:6], sid, patience)
		}
	}
}

// handleResult 节点回传:在途则聚合,定稿后入库标记 late。
func (s *Scheduler) handleResult(ctx context.Context, agentID store.ID, p protocol.ProbeResultPayload) {
	roundOID, err := store.IDFromHex(p.RoundID)
	if err != nil {
		return
	}
	monitorOID, err := store.IDFromHex(p.MonitorID)
	if err != nil {
		return
	}
	s.mu.Lock()
	flight := s.rounds[roundOID]
	s.mu.Unlock()
	// 有效判定 = 原始探测判定按本轮的 Invert 快照折算(非反转监控即原样)。
	// 结果表存有效判定,轮次聚合同口径;httpStatus/error 仍是 Agent 的原始观测,
	// 恰好解释了反转后这一行为什么是故障(见 spec 的「原始观测照旧留痕」)。
	// 轮次已定稿(晚到)时快照已不在内存,此时无从折算、也不参与聚合,
	// 按原始判定入库即可(绝不因快照缺失而把成功改写成失败)。
	effOK := p.OK
	if flight != nil {
		effOK = flight.Spec.EffOK(p.OK)
	}
	res := &store.CheckResult{
		RoundID: roundOID, MonitorID: monitorOID, AgentID: agentID,
		OK: effOK, LatencyMs: p.LatencyMs, HTTPStatus: p.HTTPStatus, Error: p.Error,
		SpeedKbps: p.SpeedKbps,
	}
	if flight != nil {
		// 分桶/窗口都以所属轮次的计划时间为准(小时聚合与延时曲线同一口径)。
		res.ScheduledAt = flight.Spec.ScheduledAt
	}

	if flight != nil && flight.AddResult(agentID.Hex(), p.OK, p.LatencyMs, p.SpeedKbps, p.Error) {
		// 按时结果:入库后如果这轮所有节点都回来了,提前定稿。
		if err := s.st.InsertResult(ctx, res); err != nil {
			g.Log().Errorf(ctx, "结果入库失败: %v", err)
			return
		}
		if len(flight.PendingAgents()) == 0 {
			s.finalize(ctx, flight)
		}
		return
	}
	// 轮次已定稿(或已被其他实例处理):晚到入库。
	res.Late = true
	if err := s.st.InsertResult(ctx, res); err != nil {
		g.Log().Errorf(ctx, "晚到结果入库失败: %v", err)
	}
}

// finalize 定稿:探活缺样节点→聚合→写轮次记录→移出在途。
//
// 没回结果的指派节点按决策表分三类(见 aggregate):
//
//	探活无响应(节点离线)                       ⇒ 不计分母,记 missing_dead;
//	探活有响应、但任务从未交给它                 ⇒ 不计分母,记 missing_undispatched;
//	探活有响应、任务也交给了它(在线却没报)      ⇒ 计失败,记 missing_alive。
//
// "任务有没有交出去"必须参与判定:重启后的首轮是在"进程刚起、hub 里一条连接都没有"时
// 下发的,任务全部没发出去;而定稿探活此时又能 ping 通刚回来的节点 —— 只看探活结果就会
// 把它们记成"节点活着却不报" ⇒ 0/N = 0% ⇒ 重启后几乎所有监控凭空多一轮 DOWN
// (见 .scratch/restart-first-round/spec.md)。
//
// 未下发的节点照样探活一次:探活回答的是"节点此刻在不在线",而这决定了这一格该怎么
// 留痕 —— 离线说「离线缺样」(节点侧的问题),在线却说「未派发任务」(我们没派出去)。
func (s *Scheduler) finalize(ctx context.Context, flight *InFlightRound) {
	roundOID, err := store.IDFromHex(flight.Spec.RoundID)
	if err != nil {
		return
	}
	dead := map[string]bool{}
	undispatched := map[string]bool{}
	for _, sid := range flight.PendingAgents() {
		aid, err := store.IDFromHex(sid)
		if err != nil {
			dead[sid] = true
			continue
		}
		// 探活:WS ping/pong;无响应=节点离线(不计分母)。
		online := s.h.Ping(ctx, aid, PongTimeout)
		switch {
		case !online:
			dead[sid] = true
		case !flight.Dispatched(sid):
			// 节点在线,但本轮从未把任务交给它:这一格没有样本是派发侧的问题。
			undispatched[sid] = true
		}
	}
	agg := flight.Close(dead, undispatched)

	// 先移出在途(flight 已 closed 不再接受结果),再写库;写库失败仅记日志,
	// 该轮次记录停留 OPEN,由下次进程启动的 CloseOrphanOpenRounds 兜底。
	s.mu.Lock()
	delete(s.rounds, roundOID)
	s.mu.Unlock()

	state := store.RoundStateClosed
	if agg.State == RoundUnknown {
		state = store.RoundStateUnknown
	}
	updated, err := s.st.CloseRound(ctx, roundOID, store.RoundClose{
		State: state, Success: agg.Success, Valid: agg.Valid,
		SuccessRate:  agg.SuccessRate,
		LatencySumMs: agg.LatencySumMs, LatencyCount: agg.LatencyCount,
		SpeedSumKbps: agg.SpeedSumKbps, SpeedCount: agg.SpeedCount,
		MissingAlive: agg.MissingAlive, MissingDead: agg.MissingDead,
		MissingUndispatched: agg.MissingUndispatched,
	})
	if err != nil {
		g.Log().Errorf(ctx, "轮 %s 定稿写入失败: %v", agg.RoundID[:6], err)
	}
	if updated {
		// 小时聚合(票 09):仅在首次定稿成功时累加,避免双计。
		if err := s.st.AddHourlyStat(ctx, mustID(agg.MonitorID), flight.Spec.ScheduledAt,
			1, agg.Valid, agg.Success, agg.LatencySumMs, agg.LatencyCount); err != nil {
			g.Log().Errorf(ctx, "小时聚合写入失败(轮 %s): %v", agg.RoundID[:6], err)
		}
		s.applyAlert(ctx, flight.Spec, agg)
	}
}

// applyAlert 轮次首次定稿后推进告警状态机;翻转时持久化状态、留一条状态变动记录
// (monitor_state_changes,详情页「最近状态变动记录」的数据源)并回调通知。
// UNKNOWN 轮:不推进破线/恢复计数,仅记录 lastRoundState。
func (s *Scheduler) applyAlert(ctx context.Context, spec RoundSpec, agg Aggregation) {
	m, err := s.src.FindMonitorByID(ctx, mustID(spec.MonitorID))
	if err != nil {
		return // 监控已删除
	}
	monOID := m.ID
	cur, _, err := s.st.GetMonitorState(ctx, monOID)
	if err != nil {
		g.Log().Errorf(ctx, "读取监控状态失败: %v", err)
		return
	}
	in := alert.RoundOutcome{
		Valid: agg.State != RoundUnknown, Value: agg.MetricValue, SuccessRate: agg.SuccessRate,
	}
	next, ev := alert.Apply(alert.State{AlertState: cur.AlertState, Consecutive: cur.Consecutive},
		in, spec.Threshold, spec.Consecutive)
	if ev != nil {
		// 通知要按节点列明细(谁正常、谁失败、谁离线),得告诉它这是哪一轮。
		ev.RoundID = spec.RoundID
	}
	from := cur.AlertState // 变动前状态(记录与通知都要用它)
	cur.AlertState = next.AlertState
	cur.Consecutive = next.Consecutive
	cur.LastRoundState = string(agg.State)
	if agg.State != RoundUnknown {
		cur.LastSuccessRate = agg.SuccessRate
		cur.LastSpeedKbps = agg.AvgSpeedKbps()
	}
	if err := s.st.PutMonitorState(ctx, cur); err != nil {
		g.Log().Errorf(ctx, "写监控状态失败: %v", err)
		return
	}
	// 真翻转才留痕:详情页的「最近状态变动记录」只认这些行。写在回调之前,
	// 保证浏览器收到翻转推送后立刻刷新时这条记录已经可查(见 spec 第 1 节)。
	if ev != nil {
		changedAt := time.Now()
		// 恢复(DOWN→UP)要带上"这次报警持续了多久":上一条报错记录的变动时间到
		// 此刻的间隔。顺着时间轴往回找最近一条 UP→DOWN,与列表页那张表同序,
		// 所以算出来的就是用户在页面上看到的那一对(见 .scratch/alert-duration/spec.md)。
		var durationSec int64
		if next.AlertState == store.MonitorUP {
			durationSec = s.alarmDurationSec(ctx, monOID, changedAt)
		}
		if err := s.st.InsertStateChange(ctx, &store.MonitorStateChange{
			MonitorID: monOID, RoundID: mustID(spec.RoundID),
			FromState: from, ToState: next.AlertState,
			SuccessRate: agg.SuccessRate, SpeedKbps: agg.AvgSpeedKbps(),
			DurationSec: durationSec, ChangedAt: changedAt,
		}); err != nil {
			g.Log().Errorf(ctx, "状态变动记录写入失败(监控 %s): %v", m.Name, err)
		}
		// 记录的身份、时间与持续时长回填到事件上:总览页的变动流水靠它把一行插进表头
		// (与上面 RoundID 的回填同一个位置、同一个理由 —— 记录与翻转是同一次定稿)。
		ev.FromState = from
		ev.ChangedAt = changedAt
		ev.DurationSec = durationSec
	}
	if s.OnRoundFinalized != nil {
		s.OnRoundFinalized(ctx, m, agg, ev, cur)
	}
}

// alarmDurationSec 恢复那一刻回查"这次报警持续了多久"(秒)。
//
// 起点是上一条报错记录(UP→DOWN)的变动时间,不是"第一次低于阈值那一轮":
// 报警真正挂出去的时刻就是状态机判 DOWN 的那一刻,页面上的两张变动记录表
// 也只有这两个时点,用户随手一减就是这个数(见 .scratch/alert-duration/spec.md)。
//
// 查不到就返回 0(前端不显示时长),不猜:恢复前的那条报错记录被级联删除、
// 或监控的开局就是 DOWN(没有配对起点)时,都得不到可信的时长。
// 负数同样归 0(库里的秒粒度 + 时钟回拨理论上可能让间隔为 0,但不该为负)。
func (s *Scheduler) alarmDurationSec(ctx context.Context, monitorID store.ID, at time.Time) int64 {
	prev, err := s.st.FindLastStateChangeByToState(ctx, monitorID, store.MonitorDOWN)
	if err != nil {
		return 0
	}
	sec := int64(at.Sub(prev.ChangedAt).Seconds())
	if sec < 0 {
		return 0
	}
	return sec
}

// mustID 解析主键;非法时返回空 ID(调用方按查不到处理)。
func mustID(hex string) store.ID {
	id, _ := store.IDFromHex(hex)
	return id
}

// RoundDebugCount 在途轮次数量(测试/监控用)。
func (s *Scheduler) RoundDebugCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rounds)
}
