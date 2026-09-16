// Package scheduler 实现探测轮次(Probe Round)的创建、下发与收口(ADR-0003)。
//
// 轮次生命周期:调度器按监控周期创建轮次并下发 probe_task;轮次在
// “探测超时+宽限期”到点定稿。定稿时对缺样(未回结果)的指派节点逐个 WS 探活:
//
//	探活有响应(活着却没报)⇒ 该样本计失败
//	探活无响应(节点真离线)⇒ 不计入分母
//	整轮有效样本数为 0 ⇒ 轮次状态 UNKNOWN(票 06/07:不告警)
//
// 本次成功率 = 成功样本 ÷ 有效样本。定稿后的晚到结果入库并标记 late,不参聚。
//
// 判定口径(见 RoundMetric):成功率监控的判定值就是本次成功率;下载速度监控的判定值
// 是本次平均下载速度(KB/s)—— 两种口径都"越低越差",故破线判定与状态机共用一套逻辑。
//
// 反转模式(监控级 InvertMode):Agent 回报的原始判定先经 RoundSpec.EffOK 折算为
// 有效判定再聚合与入库——探测失败算正常、探测成功算故障;缺样与 UNKNOWN 不受影响。
// 详见 .scratch/invert-mode/spec.md(下载速度监控不支持反转)。
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/uptimemesh/shared/protocol"
)

// RoundSpec 创建轮次时的不可变快照(含告警判定所需的阈值)。
type RoundSpec struct {
	RoundID     string
	MonitorID   string
	AgentIDs    []string
	Deadline    time.Time
	ScheduledAt time.Time
	// Threshold 是告警阈值,与 Metric 同单位:
	//   MetricSuccess ⇒ 本次成功率下限(%);
	//   MetricSpeed   ⇒ 下载速度下限(KB/s,已由监控配置的 KB/s / MB/s 换算而来)。
	Threshold   float64
	Consecutive int // 连续破线轮数(状态机用)
	// Metric 决定本轮的判定值取自哪个口径(见 RoundMetric)。
	Metric RoundMetric
	// SerialDispatch 串行下发:一轮里同一时刻只让一个节点执行探测,收到它的结果
	// (或等它到 patience 超时)后才下发下一个。
	//
	// 下载速度监控必须如此:多个节点同时下载同一个文件会互相压带宽、也会让目标文件
	// 服务器过载,测出来的都不是这条链路的真实速度。代价是一轮耗时随节点数变长,
	// 因此建轮时的截止时间要按节点数顺延(见 Scheduler.createRound)。
	SerialDispatch bool
	// Invert 反转模式快照:探测判定取反(失败算正常、成功算故障)。
	// 与阈值/连续轮数同处快照,故一轮之内口径恒定,编辑只对下一轮生效。
	// 下载速度监控不支持反转(判定值是速度而非成败),其快照恒为 false;
	// push 的**静默轮**同样恒为 false —— 反转的是"上报说了什么",不是"有没有上报"
	// (见 push.go 的 cyclePush)。
	Invert bool
	// ProbeTimeout 是本轮探测的节点侧执行上限(check.timeout_seconds)。
	// 只用于判断"节点重连后补发还来不来得及":剩余时间不足一次探测时不再补发
	// (发出去也只能得到一张不入聚合的晚到结果)。见 Scheduler.AgentOnline。
	ProbeTimeout time.Duration
}

// RoundMetric 轮次判定值的口径。两种口径都是「越低越差」,故状态机只比较数值大小。
type RoundMetric string

const (
	// MetricSuccess:本次成功率(%)—— 全部非下载类监控。
	MetricSuccess RoundMetric = "success_rate"
	// MetricSpeed:本次平均下载速度(KB/s)—— 下载速度监控。
	MetricSpeed RoundMetric = "speed"
)

// EffOK 把 Agent 回报的原始探测判定折算为**有效判定**:
// 反转模式下取反,否则原样。轮次聚合与结果入库共用本函数,保证
// 「结果表里的一条 ok」与「轮次里的一次成功计数」永远同口径。
// 缺样与执行失败不在此列(见 spec:它们不代表目标状态)。
func (s RoundSpec) EffOK(rawOK bool) bool {
	if s.Invert {
		return !rawOK
	}
	return rawOK
}

// InFlightRound 未定稿轮次的可变聚合状态;用 NewRound 构造。
type InFlightRound struct {
	Spec RoundSpec

	mu      sync.Mutex
	results map[string]sample // agentID -> 已按时回传的样本
	closed  bool
	agg     *Aggregation

	// task 是本轮下发的任务帧:载荷只有轮次/监控身份与探测配置,与具体节点无关,
	// 因此可以原样补发给刚重连的节点(见 Scheduler.AgentOnline)。
	task *protocol.Envelope
	// dispatched 是"任务真的交到过它手上"的节点集合。不在集合里的节点本轮从未收到
	// 任务,定稿时既不算失败也不计入分母(见 scheduler.finalize)。
	dispatched map[string]bool

	// 串行下发(下载速度监控)用:每个等待方按 agentID 挂一个 channel,
	// 该节点的结果一到就关闭;轮次定稿时关闭 closedCh 唤醒所有等待方。
	waiters  map[string]chan struct{}
	closedCh chan struct{}
}

// sample 按时回传样本:成败与耗时(缺样无耗时);speedKbps 仅下载速度监控有值。
type sample struct {
	ok        bool
	latencyMs float64
	speedKbps float64
	// errText 是失败原因(成功为空):总览卡片的节点明细要显示"这个节点为什么挂了",
	// 而原因只随这一次回传到达,出了轮次就只剩结果表里那一行。
	errText string
}

func NewRound(spec RoundSpec) *InFlightRound {
	return &InFlightRound{
		Spec:       spec,
		results:    make(map[string]sample, len(spec.AgentIDs)),
		dispatched: map[string]bool{},
		waiters:    map[string]chan struct{}{},
		closedCh:   make(chan struct{}),
	}
}

// SetTask 记录本轮下发的任务帧(建轮时调用一次),供节点重连补发。
func (r *InFlightRound) SetTask(env protocol.Envelope) {
	r.mu.Lock()
	r.task = &env
	r.mu.Unlock()
}

// Task 返回本轮的任务帧(建轮时未设置则 ok=false)。
func (r *InFlightRound) Task() (protocol.Envelope, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.task == nil {
		return protocol.Envelope{}, false
	}
	return *r.task, true
}

// MarkDispatched 标记任务已交给该节点(下发成功时调用,含重连补发)。
func (r *InFlightRound) MarkDispatched(agentID string) {
	r.mu.Lock()
	r.dispatched[agentID] = true
	r.mu.Unlock()
}

// Dispatched 该节点本轮是否收到过任务(定稿判缺样口径的依据)。
func (r *InFlightRound) Dispatched(agentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dispatched[agentID]
}

// UndispatchedAgents 仍"没收到任务、也还没回结果"的指派节点:节点重连时补发的对象。
func (r *InFlightRound) UndispatchedAgents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, id := range r.Spec.AgentIDs {
		if r.dispatched[id] {
			continue
		}
		if _, reported := r.results[id]; reported {
			continue
		}
		out = append(out, id)
	}
	return out
}

// AddResult 记录按时回传的结果;晚于定稿返回 false(调用方标记 late 入库)。
// errText 是失败原因(成功传空串),只用于推送里的节点明细,不参与聚合判定。
func (r *InFlightRound) AddResult(agentID string, ok bool, latencyMs, speedKbps float64, errText string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if _, dup := r.results[agentID]; dup {
		return true // 同一节点同一轮只认第一份
	}
	r.results[agentID] = sample{ok: ok, latencyMs: latencyMs, speedKbps: speedKbps, errText: errText}
	// 唤醒正在等这个节点的串行下发方(通道只关一次,关完即摘掉)。
	if ch, waiting := r.waiters[agentID]; waiting {
		close(ch)
		delete(r.waiters, agentID)
	}
	return true
}

// AwaitResult 等到 agentID 的结果回传;返回 false 表示等到超时、轮次已定稿或 ctx 结束
// (三种情况调用方都不必再等它)。串行下发用它实现"等上一个节点测完再发下一个"。
func (r *InFlightRound) AwaitResult(ctx context.Context, agentID string, timeout time.Duration) bool {
	r.mu.Lock()
	if _, done := r.results[agentID]; done {
		r.mu.Unlock()
		return true
	}
	if r.closed {
		r.mu.Unlock()
		return false
	}
	ch, waiting := r.waiters[agentID]
	if !waiting {
		ch = make(chan struct{})
		r.waiters[agentID] = ch
	}
	closedCh := r.closedCh
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	case <-closedCh:
		return false
	case <-ctx.Done():
		return false
	}
}

// PendingAgents 返回仍未回结果的指派节点(定稿探活对象)。
func (r *InFlightRound) PendingAgents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, id := range r.Spec.AgentIDs {
		if _, seen := r.results[id]; !seen {
			out = append(out, id)
		}
	}
	return out
}

// Close 收口。deadAgentIDs=探活无响应的节点,undispatchedAgentIDs=任务从未交到手上的
// 节点;两者都**不计分母**(区别只在留痕:前者是节点测不出,后者是我们没派出去)。
// 重复 Close 返回相同结果。
func (r *InFlightRound) Close(deadAgentIDs, undispatchedAgentIDs map[string]bool) Aggregation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return *r.agg
	}
	agg := aggregate(r.Spec, r.results, deadAgentIDs, undispatchedAgentIDs)
	r.closed = true
	r.agg = &agg
	// 唤醒还在等结果的串行下发方(订阅者拿到它就会退出,不再往下发)。
	close(r.closedCh)
	return agg
}

// Closed 是否已定稿。
func (r *InFlightRound) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// RoundState 轮次定稿后的粗状态(破线/告警判定属票 07 状态机,不在轮次层)。
type RoundState string

const (
	RoundClosed  RoundState = "CLOSED"  // 有有效样本,成功率已算出
	RoundUnknown RoundState = "UNKNOWN" // 整轮无有效样本(全部节点离线缺样)
)

// Aggregation 轮次定稿结果。
type Aggregation struct {
	RoundID      string
	MonitorID    string
	ScheduledAt  time.Time // 该轮的计划时间(浏览器推送的状态条单元要用)
	Success      int       // 成功样本数
	Valid        int       // 有效样本数(分母)
	TotalAgents  int       // 指派节点数
	SuccessRate  float64   // 本次成功率 0~100
	LatencySumMs float64   // 按时回传样本的耗时之和(平均延迟统计用)
	LatencyCount int       // 有耗时数据的样本数
	// Metric 是本轮判定值,与监控阈值同单位比较(见 RoundSpec.Threshold)。
	MetricValue float64
	// SpeedSumKbps/SpeedCount 是本次平均下载速度的中间量(仅 MetricSpeed 口径累加):
	// 下载失败的样本速度为 0 但同样计数 —— "文件下不下来"必须把整轮拉下来。
	SpeedSumKbps float64
	SpeedCount   int
	State        RoundState
	MissingAlive []string // 活着却没报 ⇒ 计失败
	MissingDead  []string // 节点离线缺样 ⇒ 不计分母
	// MissingUndispatched 是任务从未交到节点手上的指派节点(建轮时它不在线,重连补发
	// 也没赶上)⇒ 同"不计分母",但成因在派发侧:这一格没有样本既不是节点的失败,
	// 也不是目标的失败。单列一组是为了页面/通知能说清这一点(见
	// .scratch/restart-first-round/spec.md)。
	MissingUndispatched []string
	// Samples 是按时回传样本的展示视图(顺序与 RoundSpec.AgentIDs 一致;缺样不在这里,
	// 它们在 MissingAlive/MissingDead 里)。浏览器推送总览卡片的节点明细直接取它,
	// 不必为每个轮次定稿回查结果表。
	Samples []AgentSample
}

// AgentSample 本轮某指派节点回传样本的展示视图(总览卡片「节点明细」用)。
// OK 是**有效判定**(已按本轮 Invert 快照折算),与结果表里那一行同口径。
type AgentSample struct {
	AgentID   string
	OK        bool
	LatencyMs float64
	Error     string // 失败原因;成功为空
}

// AvgSpeedKbps 本轮平均下载速度(KB/s);没有速度样本时为 0。
func (a Aggregation) AvgSpeedKbps() float64 {
	if a.SpeedCount <= 0 {
		return 0
	}
	return a.SpeedSumKbps / float64(a.SpeedCount)
}

// aggregate 纯函数:缺样决策表(spec:Q16 决策表)。
// 已回传样本的成败先经 Spec.EffOK 折算(反转模式),缺样样本与判定无关。
// 判定值按本轮 Metric 口径计算:成功率口径用本次成功率,速度口径用平均下载速度。
//
// 缺样分三种(前两种由调用方探活/派发情况给出,见 scheduler.finalize):
//
//	dead:        探活无响应(节点离线)                  ⇒ 不计分母
//	undispatched:探活有响应,但任务从未交到它手上        ⇒ 不计分母
//	其余:        探活有响应、任务也交出去了却没回        ⇒ 计失败
func aggregate(spec RoundSpec, results map[string]sample, dead, undispatched map[string]bool) Aggregation {
	agg := Aggregation{
		RoundID: spec.RoundID, MonitorID: spec.MonitorID,
		ScheduledAt: spec.ScheduledAt,
		TotalAgents: len(spec.AgentIDs),
	}
	for _, id := range spec.AgentIDs {
		if s, reported := results[id]; reported {
			eff := spec.EffOK(s.ok)
			agg.Valid++
			agg.LatencySumMs += s.latencyMs
			agg.LatencyCount++
			if eff {
				agg.Success++
			}
			// 推送用的展示视图与聚合同一份样本:OK 也是折算后的有效判定。
			agg.Samples = append(agg.Samples, AgentSample{
				AgentID: id, OK: eff, LatencyMs: s.latencyMs, Error: s.errText,
			})
			if spec.Metric == MetricSpeed {
				// 速度口径按**原始观测**累计(与反转无关:速度监控不支持反转);
				// 下载失败(ok=false)的样本速度为 0,照样进分母。
				if s.ok {
					agg.SpeedSumKbps += s.speedKbps
				}
				agg.SpeedCount++
			}
			continue
		}
		if dead[id] {
			agg.MissingDead = append(agg.MissingDead, id)
			continue // 不计分母
		}
		if undispatched[id] {
			// 节点在线却没收到任务(我们没派出去):与离线同处置(不计分母),
			// 但留痕在另一组,页面才能区分"节点测不出目标"与"我们没派出去"。
			agg.MissingUndispatched = append(agg.MissingUndispatched, id)
			continue
		}
		agg.MissingAlive = append(agg.MissingAlive, id)
		agg.Valid++ // 活着却没报 ⇒ 失败样本
		if spec.Metric == MetricSpeed {
			agg.SpeedCount++ // 速度为 0:活着没回结果就是这一轮没测出速度
		}
	}
	if agg.Valid == 0 {
		agg.State = RoundUnknown
		return agg
	}
	agg.State = RoundClosed
	agg.SuccessRate = float64(agg.Success) / float64(agg.Valid) * 100
	agg.MetricValue = agg.SuccessRate
	if spec.Metric == MetricSpeed {
		agg.MetricValue = agg.AvgSpeedKbps()
	}
	return agg
}
