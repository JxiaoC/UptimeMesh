// 监控详情页时间线的 API 载荷类型。
// 「最近轮次时间线」与「最近状态变动记录」共用同一份行结构(TimelineRow):
// 变动记录行 = 轮次行 + 变动字段,后端也是同一个 roundView() 拼的,
// 前端两块面板因此能共用一张表(见 .scratch/state-change-history/spec.md)。

/** 一轮内某节点的回传结果。 */
export interface RoundResult {
  agentId: string
  ok: boolean
  late: boolean
  latencyMs: number
  httpStatus: number
  error: string
  createdAt: string
  /** 下载速度监控:该节点本轮测得的速度(KB/s);其余类型为 0。 */
  speedKbps?: number
}

/** 时间线的一行(轮次聚合 + 各节点结果);两张表展示的字段就是这些。 */
export interface TimelineRow {
  state: string
  scheduledAt: string
  success: number
  valid: number
  totalAgents?: number
  successRate: number
  /** 下载速度监控:本轮平均速度(KB/s);前端按监控的 speedUnit 换算展示。 */
  avgSpeedKbps?: number
  missingAlive: string[]
  missingDead: string[]
  /** 任务从未交到节点手上的指派节点(与离线同"不计分母")。 */
  missingUndispatched?: string[]
  results: RoundResult[]
}

/** 轮次时间线的一行(rounds 接口的原始载荷)。 */
export interface Round extends TimelineRow {
  id: string
  closedAt: string
  totalAgents: number
}

/** 状态变动记录的一行:轮次行 + 变动信息。 */
export interface StateChange extends TimelineRow {
  id: string
  roundId: string
  fromState: string
  toState: string
  changedAt: string
  /**
   * 报警持续时长(秒):只有恢复(DOWN→UP)那条有值,= 上一条报错记录到它的间隔
   * (后端落记录时算好)。报错那条、以及"没有配对报错记录"的恢复都是 0/缺省,
   * 展示时按"无时长"处理,不显示「持续 0 秒」(见 .scratch/alert-duration/spec.md)。
   */
  durationSec?: number
}

/** 状态标签(MonitorsView 的 stateTag 同构)。 */
export interface StateTag {
  type: 'info' | 'success' | 'danger'
  text: string
}

/** 趋势桶:后端按所选颗粒度返回,bucket 为展示文本,bucketAt 为 UTC Unix 秒(对齐用)。 */
export interface StatsBucket {
  bucket: string
  bucketAt: number
  availability: number | null
  avgLatencyMs: number | null
  /** 下载速度监控:桶内平均速度(KB/s)与参与平均的样本数;其余类型为 null / 0。 */
  avgSpeedKbps: number | null
  speedCount?: number
  valid: number
  success: number
  rounds: number
}

/** 分节点延时点:与 StatsBucket 同颗粒度、同桶边界。 */
export interface AgentLatencyPoint {
  agentId: string
  bucketAt: number
  avgLatencyMs: number
  count: number
}
