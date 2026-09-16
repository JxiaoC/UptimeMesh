<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { http } from '../api/http'
import { useAgentsStore } from '../stores/agents'
import { connected, onRealtime } from '../api/realtime'
import { formatSpeed, fromKbps, toKbps } from '../utils/speed'
import { formatDuration } from '../utils/duration'
import { openMonitorDetail } from '../utils/monitorDetailDialog'

interface AgentTile { agentId: string; ok: boolean; latencyMs: number; error: string }
interface AgentFull { id: string; name: string; status: string; online: boolean }
interface OverviewItem {
  id: string; name: string; type: string; enabled: boolean; url: string; period: number
  group?: string
  /** 告警阈值:成功率监控是百分比,下载速度监控是速度(单位见 speedUnit) */
  threshold: number
  /** 下载速度监控:阈值单位与"配置单位下的阈值"(KB/s 换算后的比较用 thresholdKbps) */
  speedUnit?: string
  thresholdValue?: number
  displayState: string; latencyMs: number
  availability24h: number | null; availability7d: number | null; availability30d: number | null
  lastSuccessRate?: number; lastRoundState?: string; lastRoundAt?: string
  /** 下载速度监控:最近一轮平均速度(KB/s 与配置单位两份) */
  lastSpeedKbps?: number
  lastSpeed?: number
  agents: AgentTile[]
}
/** 总览页「最近状态变动记录」的一行:比详情页那张变动表轻,只带变动本身与监控身份。 */
interface OverviewChange {
  id: string; monitorId: string; monitorName: string; roundId: string
  fromState: string; toState: string; successRate: number; changedAt: string
  /** 下载速度监控:这一行展示速度而不是成功率(单位随监控配置)。 */
  monitorType?: string; speedKbps?: number; speedUnit?: string
  /** 报警持续时长(秒):只有恢复(DOWN→UP)那条有值,0/缺省表示无配对报错记录。 */
  durationSec?: number
}
/** 最近状态变动记录固定看的条数(与详情页那张表一致,后端缺省也是 20)。 */
const CHANGE_LIMIT = 20

/** round_finalized 载荷:总览卡片就地更新所需的字段(见 docs/protocol.md)。 */
interface RoundPush {
  monitorId: string
  state: string
  successRate: number
  /** 下载速度监控:本轮平均速度(KB/s);其余类型为 0。 */
  speedKbps?: number
  scheduledAt: string
  displayState: string
  /** 本轮按时回传样本的平均延时(无样本为 0):卡片的「最新延时」那一格。 */
  latencyMs: number
  /** 本轮各指派节点的展示(顺序 = 监控配置的指派列表):卡片的「节点明细」那一排。 */
  agents: AgentTile[]
}
/** monitor_flipped 载荷:状态 + 这次翻转留下的那条状态变动记录(总览流水的数据源)。 */
interface FlipPush {
  monitorId: string
  name: string
  alertState: string
  roundSuccessRate: number
  displayState: string
  roundId: string
  fromState: string
  changedAt: string
  /** 判定值按监控类型展示:速度监控是速度(单位见 speedUnit),其余是成功率。 */
  type: string
  speedUnit: string
  speedKbps: number
  /** 报警持续时长(秒):恢复那条才有值,与 /state-changes 的同名字段同源。 */
  durationSec: number
}
/** monitor_changed / monitors_changed 的载荷:与 GET /monitors 的一行同构(这里只取总览要用的)。 */
interface MonitorRowPush {
  id: string
  name: string
  type: string
  group?: string
  enabled: boolean
  period: number
  threshold: number
  speedUnit?: string
  url?: string
  method?: string
  targetHost?: string
  port?: number
  displayState?: string
}

const { t } = useI18n()
const agentsStore = useAgentsStore()
// rows 保留**全部**行(含暂停):卡片只渲染 enabled 的,「已暂停 N」由总数推出 ——
// 实时推送把一条监控改成暂停时,只要就地改 enabled,卡片与计数会同时跟着变。
const rows = ref<OverviewItem[]>([])
const items = computed(() => rows.value.filter((m) => m.enabled))
// 暂停监控的数量:接口仍会为暂停监控返回一行身份信息(不含统计),
// 总览不展示它们的卡片,只把这个数量放进顶部汇总当提示。由全量行推出,
// 于是"某条监控被暂停"的推送一到,卡片消失与计数 +1 是同一件事。
const pausedCount = computed(() => rows.value.length - items.value.length)
// 最近状态变动记录:跨所有监控的变更流水(新→旧,固定 20 条)。
// 与详情页那张同名的表不同 —— 它一行只回答"谁、什么时候、从什么变成什么、
// 当时成功率多少",不带节点明细(见 .scratch/overview-sort-and-recent-changes/spec.md)。
// 初始由快照灌入,之后由 monitor_flipped 就地往表头插行。
const changes = ref<OverviewChange[]>([])
// 节点汇总口径与「节点」页一致:全部未删除节点(含待审批/已吊销),在线数按连接判定。
const allAgents = ref<AgentFull[]>([])
const loading = ref(true)
let timer: number | undefined

// ---- 实时推送:合并落地,不再"事件到了就整页重拉" ----
//
// 改动前每次 round_finalized(每个监控每个周期一次)都调 load() —— 那是 4 个 HTTP 请求
// (/overview、/state-changes、/agents、/agents?includeDeleted=1),20 个监控就是每分钟
// 80 个请求,200 个就是 800 个。现在服务端在推送里带上"卡片就地更新所需的全部字段",
// 前端只改那一行;HTTP 只留给进页面与兜底对齐的快照。
//
// 为什么要合并(而不是每条事件立刻落地):一次"全量扫描"(所有监控同一周期一起定稿)
// 会产生上百条事件,逐条写响应式状态就是逐次整页渲染。这里按 250ms 防抖 / 1s 上限
// 收口成一次同步批量写入 —— Vue 对同一批同步写入只渲染一次,与列表页 scheduleSort 同理。
const FLUSH_DEBOUNCE_MS = 250
const FLUSH_MAX_WAIT_MS = 1000
interface Push {
  kind: 'round' | 'flip' | 'upsert' | 'remove'
  round?: RoundPush
  flip?: FlipPush
  upserts?: MonitorRowPush[]
  removed?: string[]
}
const pending: Push[] = []
let flushTimer: number | undefined
let flushFirstAt = 0
// nodesDirty:节点上下线/审批变化只置脏标记,真正的重拉放在同一次 flush 里 ——
// 一批节点同时重连时(push 管理端的常见形态)只多发一次请求,而不是每个节点一次。
let nodesDirty = false

// scheduleFlush 与列表页的 scheduleSort 同款:防抖 250ms,但持续不断的事件下
// 至少每 1s 落位一次(否则一直有事件就永远不刷新)。
function scheduleFlush() {
  const now = Date.now()
  if (!flushFirstAt) flushFirstAt = now
  if (flushTimer !== undefined) clearTimeout(flushTimer)
  const wait = Math.max(0, Math.min(FLUSH_DEBOUNCE_MS, flushFirstAt + FLUSH_MAX_WAIT_MS - now))
  flushTimer = window.setTimeout(flush, wait)
}

function flush() {
  flushTimer = undefined
  flushFirstAt = 0
  const batch = pending.splice(0, pending.length)
  for (const p of batch) {
    if (p.kind === 'round' && p.round) applyRound(p.round)
    else if (p.kind === 'flip' && p.flip) applyFlip(p.flip)
    else if (p.kind === 'upsert') upsertMonitors(p.upserts || [])
    else if (p.kind === 'remove') removeMonitors(p.removed || [])
  }
  if (nodesDirty) {
    nodesDirty = false
    void loadNodes()
  }
}

// applyRound 一轮定稿:整轮一起定稿时同一监控只会留最后一条(卡片显示的就是"最近一轮"),
// 故 flush 里同 id 的多条后者覆盖前者即可 —— 不必去重,直接按顺序写,最后一条胜出。
function applyRound(p: RoundPush) {
  const row = rows.value.find((m) => m.id === p.monitorId)
  // 快照还没包含这张卡片(或它已暂停):忽略这一轮。
  // 不新建半张卡片 —— 缺身份与可用率,看起来像一条坏数据;下一次快照会补齐。
  if (!row || !row.enabled) return
  row.displayState = p.displayState || row.displayState
  row.lastSuccessRate = round2(p.successRate)
  row.latencyMs = p.latencyMs
  row.agents = p.agents || []
  // lastRoundState / lastRoundAt 卡片不展示(排序与状态标签只吃 displayState 与判定值),
  // 故不在这里写:推送的 scheduledAt 用的是服务端本地时间,而快照那份来自库里(UTC),
  // 混着写会让同一字段在同一行里出现两种口径 —— 不写就不会有歧义。
  if (row.type === 'download') {
    // 推送里的速度一律 KB/s;卡片第 4 格与"低于阈值"判定都按监控配置的单位展示。
    row.lastSpeedKbps = p.speedKbps
    row.lastSpeed = p.speedKbps == null ? undefined : round2(fromKbps(p.speedKbps, row.speedUnit || 'KB/s'))
  }
}

// applyFlip 状态翻转:改卡片状态,并把这次翻转插进变动流水表头。
function applyFlip(p: FlipPush) {
  const row = rows.value.find((m) => m.id === p.monitorId)
  if (row && row.enabled) row.displayState = p.displayState || p.alertState
  const change: OverviewChange = {
    id: p.roundId, monitorId: p.monitorId, monitorName: p.name, roundId: p.roundId,
    fromState: p.fromState, toState: p.alertState,
    successRate: round2(p.roundSuccessRate), changedAt: p.changedAt,
    monitorType: p.type, speedKbps: p.speedKbps, speedUnit: p.speedUnit,
    // 报警持续时长(恢复那条才有值):推送与 /state-changes 同源,插进来的行与
    // 快照拉回的行显示完全一致。
    durationSec: p.durationSec,
  }
  // 同一次翻转可能因重连/重复推送再来一遍(roundId 唯一),按 roundId 去重。
  const next = [change, ...changes.value.filter((c) => c.roundId !== p.roundId)]
  changes.value = next.slice(0, CHANGE_LIMIT)
}

// upsertMonitors 监控配置变化(新建/编辑/暂停恢复/批量):身份与配置字段就地合并,
// 统计字段(可用率/最近轮/节点明细)留空由下一次快照补齐 —— 新建的监控本来就还没有。
function upsertMonitors(list: MonitorRowPush[]) {
  if (!list.length) return
  const next = [...rows.value]
  for (const row of list) {
    const at = next.findIndex((m) => m.id === row.id)
    if (at < 0) {
      next.push({
        id: row.id, name: row.name, type: row.type, group: row.group, enabled: row.enabled,
        url: targetOf(row), period: row.period, threshold: row.threshold,
        speedUnit: row.speedUnit, thresholdValue: thresholdValueOf(row),
        displayState: row.displayState || 'UNKNOWN', latencyMs: 0,
        availability24h: null, availability7d: null, availability30d: null,
        agents: [],
      })
      continue
    }
    const cur = next[at]
    cur.name = row.name
    cur.type = row.type
    cur.group = row.group
    cur.enabled = row.enabled
    cur.period = row.period
    cur.threshold = row.threshold
    cur.speedUnit = row.speedUnit
    cur.thresholdValue = thresholdValueOf(row)
    cur.url = targetOf(row)
    if (row.displayState) cur.displayState = row.displayState
  }
  rows.value = next
}

// removeMonitors 监控被删:卡片与它的变动流水一起清掉(与 /state-changes 的口径一致 ——
// 那里也会跳过监控已不存在的记录)。
function removeMonitors(ids: string[]) {
  if (!ids.length) return
  const gone = new Set(ids)
  rows.value = rows.value.filter((m) => !gone.has(m.id))
  changes.value = changes.value.filter((c) => !gone.has(c.monitorId))
}

// targetOf 列表行 → 卡片副标题的目标文本(后端 urlOf 的同口径:http/download 是 URL、
// tcp 是主机:端口、push 没有目标)。
function targetOf(row: MonitorRowPush): string {
  if (row.type === 'http' || row.type === 'download') return row.url || ''
  if (row.type === 'tcp') return `${row.targetHost || ''}:${row.port || ''}`
  return ''
}

// thresholdValueOf 下载速度监控的阈值(换算成 KB/s):卡片按同单位与最新速度比较。
function thresholdValueOf(row: MonitorRowPush): number | undefined {
  if (row.type !== 'download') return undefined
  return round2(toKbps(row.threshold, row.speedUnit || 'KB/s'))
}

// round2 保留两位小数:与后端 round2 同口径,免得成功率显示成 66.66666666666667。
function round2(v: number): number {
  return Math.round(v * 100) / 100
}

// ---- 快照:进页面 / 刷新按钮 / 兜底轮询 / 重连对齐 ----
// 可用率窗口(24h/7d/30d)来自小时聚合,是慢变量,不在推送里 —— 它们是唯一"只能靠快照
// 刷新"的字段(见 .scratch/overview-websocket/spec.md 的非目标)。
// loadedOnce 区分"本次挂载的首次快照"与"断线重连后的对齐":WS 刚连上时 connected 由
// false 变 true,那不是重连,而是本次挂载的连接建立 —— 此时首屏快照刚要/已经发出,
// 再对齐一次就是白发的 4 个请求(实测首屏会因此翻倍)。只有首屏成功过之后,
// false→true 才意味着"断连期间可能漏了事件",那时才需要补一次。
let loadedOnce = false
async function loadSnapshot(silent = false) {
  if (!silent) loading.value = true
  try {
    // 变动流水、总览与节点并行拉:三者互不依赖,串行只会让首屏慢一拍。
    const [overview, recent, agents] = await Promise.all([
      http.get('/overview'),
      http.get(`/state-changes?limit=${CHANGE_LIMIT}`),
      http.get('/agents'),
    ])
    changes.value = recent as unknown as OverviewChange[]
    // 暂停的监控也留在 rows 里(卡片不渲染,只进「已暂停」计数)。
    rows.value = overview as unknown as OverviewItem[]
    allAgents.value = agents as unknown as AgentFull[]
    await agentsStore.load()
    loadedOnce = true
  } catch {
    // 一次快照失败(后端重启/网络抖动)不该让页面停在半空:下一轮兜底轮询会重试,
    // 期间推送仍在驱动卡片。这里吞掉异常只为免掉"未处理的 Promise 拒绝"噪音。
  } finally {
    loading.value = false
  }
}

// loadNodes 只重拉节点汇总与名称映射(agent_changed 的合并出口)。
async function loadNodes() {
  try {
    allAgents.value = (await http.get('/agents')) as unknown as AgentFull[]
    await agentsStore.load()
  } catch {
    /* 同上:下一轮兜底轮询会重试 */
  }
}

// 兜底轮询 + 重连对齐。推送只是"尽快",并不保证送达:服务端对慢客户端丢帧不背压
// (见 docs/protocol.md),浏览器 WS 也会重连。只靠推送的话,断连窗口里错过的轮次/
// 翻转/节点变更会让页面**永久**停在过期值,所以这条兜底必须有。
let offs: (() => void)[] = []
function startPolling(live: boolean) {
  if (timer) clearInterval(timer)
  timer = window.setInterval(() => void loadSnapshot(true), live ? 30000 : 5000)
}
onMounted(() => {
  void loadSnapshot()
  offs = [
    onRealtime('round_finalized', (d) => {
      pending.push({ kind: 'round', round: d as RoundPush })
      scheduleFlush()
    }),
    onRealtime('monitor_flipped', (d) => {
      pending.push({ kind: 'flip', flip: d as FlipPush })
      scheduleFlush()
    }),
    onRealtime('monitor_changed', (d) => {
      pending.push({ kind: 'upsert', upserts: [d as MonitorRowPush] })
      scheduleFlush()
    }),
    onRealtime('monitors_changed', (d) => {
      const rows = (d as { monitors?: MonitorRowPush[] }).monitors || []
      if (!rows.length) return
      pending.push({ kind: 'upsert', upserts: rows })
      scheduleFlush()
    }),
    onRealtime('monitor_deleted', (d) => {
      pending.push({ kind: 'remove', removed: [(d as { monitorId: string }).monitorId] })
      scheduleFlush()
    }),
    onRealtime('monitors_deleted', (d) => {
      pending.push({ kind: 'remove', removed: (d as { monitorIds?: string[] }).monitorIds || [] })
      scheduleFlush()
    }),
    onRealtime('agent_changed', () => {
      nodesDirty = true
      scheduleFlush()
    }),
  ]
  startPolling(connected.value)
})
watch(connected, (live) => {
  startPolling(live)
  if (live && loadedOnce) void loadSnapshot(true) // 断线期间可能漏了事件,重连后先对齐一次
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (flushTimer !== undefined) clearTimeout(flushTimer)
  offs.forEach((f) => f())
})

// 卡片状态标签:与列表页 MonitorStateTag 同一份 status.* 词条,故放进 computed 跟随语言。
// 比列表页多一个 BREACH(低于阈值):最后一次定稿轮没达标、但还没攒够连续轮数的黄色预警
// —— 排序把它排在 DOWN 与 UP 之间,卡片上就必须有同样看得见的标签,否则一张绿标签的卡片
// 夹在红卡与绿卡之间,顺序看起来像随机的。判据与状态条的黄色口径同源。
const stateMap = computed<
  Record<string, { type: 'success' | 'danger' | 'info' | 'warning'; text: string }>
>(() => ({
  UP: { type: 'success', text: t('status.UP') },
  DOWN: { type: 'danger', text: t('status.DOWN') },
  UNKNOWN: { type: 'info', text: t('status.UNKNOWN') },
  PAUSED: { type: 'info', text: t('status.PAUSED') },
  BREACH: { type: 'warning', text: t('status.breach') },
}))
const agentName = (id: string) => agentsStore.nameOf(id)

// isBreach 最近一轮低于阈值(还没判 DOWN):状态条上那一格是黄的,即"预警"。
// 判定值随监控类型而变:成功率监控比最近成功率,下载速度监控比最近平均速度
// (两者都在监控配置的速度单位下比较,与后端把阈值换算成 KB/s 后比较等价)。
function isBreach(m: OverviewItem): boolean {
  if (m.type === 'download') {
    return m.lastSpeed != null && m.thresholdValue != null && m.lastSpeed < m.thresholdValue
  }
  return m.lastSuccessRate != null && m.lastSuccessRate < m.threshold
}

// lastMetricText 卡片第 4 格的展示值:速度监控显示"12.34 MB/s",其余显示成功率百分比。
function lastMetricText(m: OverviewItem): string {
  if (m.type === 'download') {
    return m.lastSpeedKbps == null ? '—' : formatSpeed(m.lastSpeedKbps, m.speedUnit || 'KB/s')
  }
  return m.lastSuccessRate == null ? '—' : String(m.lastSuccessRate)
}

// changeMetricText 变动流水里的判定值展示(下载速度监控展示速度,单位随监控配置)。
function changeMetricText(row: OverviewChange): string | null {
  if (row.monitorType === 'download') {
    return row.speedKbps == null ? null : formatSpeed(row.speedKbps, row.speedUnit || 'KB/s')
  }
  return row.successRate == null ? null : `${row.successRate}%`
}

// changeDuration 该行要显示的「报警持续时长」文本;没有则返回空串。
// 与详情页那张表同一套口径:只有恢复行、且后端算出了时长才显示 —— 恢复却没有时长
// (监控开局就是 DOWN、配对记录被删)时什么都不显示,不写「持续 0 秒」。
function changeDuration(row: OverviewChange): string {
  if (row.toState === 'UP' && row.durationSec) return formatDuration(row.durationSec)
  return ''
}

// changeRowClass 给整行加状态类:左侧色条让"报错 / 恢复"在整行尺度上也一眼可辨
// (颜色与名称旁的标签、卡片上的状态点同源)。
function changeRowClass({ row }: { row: OverviewChange }): string {
  return row.toState === 'DOWN' ? 'row-down' : 'row-up'
}

// cardRank 卡片分档:DOWN > 低于阈值 > UNKNOWN > UP(越需要被看到的越靠前)。
// 档位一律以 displayState 为准(DOWN 期间最后一轮若为 UNKNOWN,DisplayState 本来就
// 返回 UNKNOWN,与列表页同款口径),只把 UP 里的"低于阈值"提出来单独一档。
function cardRank(m: OverviewItem): number {
  if (m.displayState === 'DOWN') return 0
  if (m.displayState === 'UNKNOWN') return 2
  return isBreach(m) ? 1 : 3
}

// cardState 卡片展示的状态:UP 且最近一轮低于阈值时显示为 BREACH。
function cardState(m: OverviewItem): string {
  if (m.displayState !== 'UP') return m.displayState
  return isBreach(m) ? 'BREACH' : 'UP'
}

// sortBySeverity 组内排序:先按档位,同档按 24h 可用率升序(越低越靠前);
// 没有 24h 样本(null,如刚创建)的排在该档最后 —— 没有数据不参与"谁更差"的比较。
// Array#sort 是稳定的,档位与可用率都相同的保持原有顺序。
function sortBySeverity(list: OverviewItem[]): OverviewItem[] {
  return [...list].sort((a, b) => {
    const rank = cardRank(a) - cardRank(b)
    if (rank !== 0) return rank
    const av = (m: OverviewItem) => m.availability24h ?? Number.POSITIVE_INFINITY
    return av(a) - av(b)
  })
}

// openChange 点一行变动记录打开该监控的详情弹窗(总览看不到它的卡片时尤其有用 ——
// 暂停的监控本来就没有卡片)。不再跳 /monitors/:id 独立页:弹窗读完关掉就回到总览,
// 位置与滚动都不动,看"刚刚发生了什么"时不必来回跳页。
function openChange(row: OverviewChange) {
  openMonitorDetail(row.monitorId)
}

// openDetail 从总览卡片打开监控详情弹窗(与变动记录同一条路,两处入口行为一致)。
function openDetail(id: string) {
  openMonitorDetail(id)
}

// 顶部汇总:节点总数/在线/离线,监控总数/正常/报警/未知/已暂停。
// 「共 N 个」是全部监控(含暂停),四个格子相加正好是 N;只是下方卡片列表只列在跑的那些,
// 暂停的要看「监控」页。这样总数不会因为列表过滤而变小,暂停多少个也一眼可见。
const nodeCounts = computed(() => {
  const total = allAgents.value.length
  const online = allAgents.value.filter((a) => a.online).length
  return { total, online, offline: total - online }
})
const monitorCounts = computed(() => {
  const c = {
    total: items.value.length + pausedCount.value,
    up: 0,
    down: 0,
    unknown: 0,
    paused: pausedCount.value,
  }
  for (const m of items.value) {
    if (m.displayState === 'UP') c.up++
    else if (m.displayState === 'DOWN') c.down++
    else if (m.displayState === 'UNKNOWN') c.unknown++
  }
  return c
})

// 全是暂停监控时 items 为空,此时别报「暂无监控」——监控是有的,只是都不在跑。
const emptyText = computed(() =>
  pausedCount.value > 0
    ? t('overview.emptyWithPaused', { count: pausedCount.value })
    : t('overview.empty'),
)

// 按分组聚合:命名分组按名称排序在前,未分组(空)殿后。
// 组内按严重度重排(见 sortBySeverity):分组是"这些监控归谁管"的容器,
// 排序回答"组里谁最需要先看",两者互不冲突,故不取消分组。
const groupedItems = computed(() => {
  const byGroup = new Map<string, OverviewItem[]>()
  for (const m of items.value) {
    const key = m.group?.trim() || ''
    const arr = byGroup.get(key)
    if (arr) arr.push(m)
    else byGroup.set(key, [m])
  }
  const named = [...byGroup.keys()].filter((k) => k).sort()
  const order = byGroup.has('') ? [...named, ''] : named
  return order.map((key) => ({
    key: key || '__ungrouped__',
    name: key || t('common.ungrouped'),
    items: sortBySeverity(byGroup.get(key) ?? []),
  }))
})
</script>

<template>
  <div v-loading="loading">
    <div style="display: flex; justify-content: space-between; align-items: center">
      <h3>{{ t('nav.overview') }}</h3>
      <el-button size="small" @click="() => loadSnapshot()">{{ t('common.refresh') }}</el-button>
    </div>
    <el-row :gutter="16" class="stats-row">
      <el-col :span="8">
        <el-card shadow="never" class="stats-card">
          <template #header>
            <div class="stats-head">
              <b>{{ t('nav.agents') }}</b>
              <span class="stats-total">{{ t('overview.totalCount', { count: nodeCounts.total }) }}</span>
            </div>
          </template>
          <div class="stats">
            <div class="stat">
              <span class="stat-val ok">{{ nodeCounts.online }}</span>
              <span class="stat-lbl">{{ t('common.online') }}</span>
            </div>
            <div class="stat">
              <span class="stat-val off">{{ nodeCounts.offline }}</span>
              <span class="stat-lbl">{{ t('common.offline') }}</span>
            </div>
          </div>
        </el-card>
      </el-col>
      <el-col :span="16">
        <el-card shadow="never" class="stats-card">
          <template #header>
            <div class="stats-head">
              <b>{{ t('nav.monitors') }}</b>
              <span class="stats-total">{{ t('overview.totalCount', { count: monitorCounts.total }) }}</span>
            </div>
          </template>
          <div class="stats">
            <div class="stat">
              <span class="stat-val ok">{{ monitorCounts.up }}</span>
              <span class="stat-lbl">{{ t('overview.statUp') }}</span>
            </div>
            <div class="stat">
              <span class="stat-val bad">{{ monitorCounts.down }}</span>
              <span class="stat-lbl">{{ t('overview.statDown') }}</span>
            </div>
            <div class="stat">
              <span class="stat-val unknown">{{ monitorCounts.unknown }}</span>
              <span class="stat-lbl">{{ t('overview.statUnknown') }}</span>
            </div>
            <div class="stat" :title="t('overview.pausedHint')">
              <span class="stat-val paused">{{ monitorCounts.paused }}</span>
              <span class="stat-lbl">{{ t('overview.statPaused') }}</span>
            </div>
          </div>
        </el-card>
      </el-col>
    </el-row>
    <el-card shadow="never" class="changes-card">
      <template #header>
        <div class="stats-head">
          <b>{{ t('monitorDetail.stateChangesTitle') }}</b>
          <span class="stats-total">{{ t('overview.recentChangesTip', { count: changes.length }) }}</span>
        </div>
      </template>
      <el-table
        :data="changes"
        size="small"
        :empty-text="t('overview.recentChangesEmpty')"
        :row-class-name="changeRowClass"
        @row-click="openChange"
      >
        <!-- 监控名与"报错 / 恢复"放在同一格:这两件事必须挨着,扫一眼就知道刚发生了什么
             (原先"变动"是最右一列,名称在最左,中间隔着宽度自适应的列,方向要来回找)。
             标签只写结论,完整翻转(UP → DOWN)放进悬停提示,不在列里读箭头。
              「恢复」标签右边再跟着一张本次报警的持续时长卡 —— 它属于"刚发生了什么"
              这件事的一部分(挂了 5 分钟 vs 挂了 5 小时,处置复盘完全两回事),
              放在别处又得来回找。 -->
        <el-table-column :label="t('overview.columnMonitor')" min-width="280">
          <template #default="{ row }">
            <div class="change-cell">
              <span class="change-dot" :class="row.toState === 'DOWN' ? 'down' : 'up'" />
              <!-- 名称过长时省略,原生 title 保留全名(标签与圆点不参与收缩)。 -->
              <span class="change-name" :title="row.monitorName">{{ row.monitorName }}</span>
              <el-tooltip
                :content="t('overview.changeFromTo', { from: row.fromState, to: row.toState })"
                placement="top"
              >
                <el-tag
                  size="small"
                  disable-transitions
                  :type="row.toState === 'DOWN' ? 'danger' : 'success'"
                >
                  {{ row.toState === 'DOWN' ? t('overview.changeDown') : t('overview.changeUp') }}
                </el-tag>
              </el-tooltip>
              <!-- 报警持续时长:只有恢复行有值(后端在恢复落记录时算好)。报错行不显示 ——
                   "要挂多久"在报错那一刻没人知道;恢复却没有时长(监控开局就是 DOWN、
                   配对记录被删)同样不显示,免得编出一个 0 秒。
                   悬停提示说清这个数从哪算起(判报警 → 恢复),两张表同一条提示。 -->
              <el-tooltip
                v-if="changeDuration(row)"
                :content="t('common.alertDurationTip')"
                placement="top"
              >
                <el-tag size="small" type="info" effect="plain" disable-transitions class="duration-tag">
                  {{ t('common.durationTag', { duration: changeDuration(row) }) }}
                </el-tag>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="changedAt" :label="t('overview.columnChangedAt')" width="170" />
        <el-table-column :label="t('overview.columnMetric')" width="140">
          <template #default="{ row }">
            <!-- 这张表混着各种类型:成功率监控显示本次成功率,下载速度监控显示本轮平均速度。 -->
            <span v-if="changeMetricText(row) != null">{{ changeMetricText(row) }}</span>
            <span v-else>{{ t('common.none') }}</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
    <el-empty v-if="!loading && items.length === 0" :description="emptyText" />
    <div v-for="g in groupedItems" :key="g.key" class="group-wrap">
      <el-card shadow="never" class="group-card">
        <template #header>
          <div class="group-head">
            <b>{{ g.name }}</b>
            <el-tag size="small" type="info">
              {{ t('overview.groupMonitorCount', { count: g.items.length }) }}
            </el-tag>
          </div>
        </template>
        <el-row :gutter="16">
          <el-col v-for="m in g.items" :key="m.id" :span="8" style="margin-bottom: 16px">
            <el-card shadow="hover" class="mon-card" @click="openDetail(m.id)">
              <div class="card-head">
                <span class="dot" :class="cardState(m).toLowerCase()" />
                <b>{{ m.name }}</b>
                <el-tag size="small" :type="stateMap[cardState(m)]?.type || 'info'" style="margin-left:auto">
                  {{ stateMap[cardState(m)]?.text || cardState(m) }}
                </el-tag>
              </div>
              <div class="card-sub">
                {{ m.type.toUpperCase() }} · {{ m.url }} · {{ t('overview.everyPeriod', { period: m.period }) }}
              </div>
              <div class="card-metrics">
                <div>
                  <span class="val">{{ m.availability24h ?? '—' }}</span><span class="unit">%</span><br />
                  <span class="lbl">{{ t('overview.availability24h') }}</span>
                </div>
                <div>
                  <span class="val">{{ m.availability7d ?? '—' }}</span><span class="unit">%</span><br />
                  <span class="lbl">{{ t('overview.availability7d') }}</span>
                </div>
                <div>
                  <span class="val">{{ m.availability30d ?? '—' }}</span><span class="unit">%</span><br />
                  <span class="lbl">{{ t('overview.availability30d') }}</span>
                </div>
                <div>
                  <span class="val">{{ lastMetricText(m) }}</span><span class="unit">{{ m.type === 'download' ? '' : '%' }}</span><br />
                  <span class="lbl">{{ m.type === 'download' ? t('overview.lastSpeed') : t('overview.lastSuccessRate') }}</span>
                </div>
                <div>
                  <span class="val">{{ m.latencyMs || '—' }}</span><span class="unit">ms</span><br />
                  <span class="lbl">{{ t('overview.lastLatency') }}</span>
                </div>
              </div>
              <div class="card-agents">
                <el-tooltip v-for="a in m.agents" :key="a.agentId"
                  :content="a.ok ? `${a.latencyMs}ms` : (a.error || t('common.failed'))">
                  <span class="adot" :class="{ bad: !a.ok }">{{ agentName(a.agentId) }}</span>
                </el-tooltip>
              </div>
            </el-card>
          </el-col>
        </el-row>
      </el-card>
    </div>
  </div>
</template>

<style scoped>
.stats-row { margin-bottom: 16px; }
.stats-card :deep(.el-card__header) { padding: 12px 20px; }
.stats-card :deep(.el-card__body) { padding: 16px 20px; }
.stats-head { display: flex; align-items: baseline; gap: 8px; }
.stats-head b { font-size: 15px; }
.stats-total { color: #909399; font-size: 12px; }
.stats { display: flex; gap: 40px; }
.stat { display: flex; flex-direction: column; align-items: flex-start; }
.stat-val { font-size: 26px; font-weight: 600; line-height: 1.2; }
.stat-lbl { font-size: 12px; color: #909399; margin-top: 2px; }
.stat-val.ok { color: #67c23a; }
.stat-val.bad { color: #f56c6c; }
.stat-val.unknown { color: #909399; }
.stat-val.paused { color: #c0c4cc; }
.stat-val.off { color: #e6a23c; }
/* 最近状态变动记录:紧跟顶部汇总,是"刚刚发生了什么"的第一落点。 */
.changes-card { margin-bottom: 16px; }
.changes-card :deep(.el-card__header) { padding: 12px 20px; }
.changes-card :deep(.el-card__body) { padding: 8px 12px 4px; }
/* 点一行进该监控详情(卡片列表里看不到的暂停监控,也从这里进得去)。 */
.changes-card :deep(.el-table__row) { cursor: pointer; }
/* 整行左侧色条:红=报错、绿=恢复(与名称旁的状态点/标签同色)。 */
.changes-card :deep(.el-table__row.row-down td:first-child) { box-shadow: inset 3px 0 0 #f56c6c; }
.changes-card :deep(.el-table__row.row-up td:first-child) { box-shadow: inset 3px 0 0 #67c23a; }
/* 监控名 + 状态点 + 结论标签挤在一格:名称过长时省略,标签与圆点不参与收缩。
   刻意**不加 flex-wrap**:加了之后浏览器按"内容原宽"决定换行,长监控名会把
   「恢复 + 持续时长」两张卡顶到第二行(名称明明可以省略号收掉)。不换行 + 名称
   min-width:0 收起,才是这一格原本的语义;标签 flex:none 保证它们先于名称被保住。 */
.change-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.change-cell > .el-tag { flex: none; }
/* 时长卡:等宽数字(时长在表里成列时才不会跳),颜色更淡 ——
   它是补充信息,不抢「报错 / 恢复」这个结论的注意力。 */
.duration-tag { flex: none; font-variant-numeric: tabular-nums; }
.change-name {
  font-weight: 500;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.change-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
.change-dot.up { background: #67c23a; }
.change-dot.down { background: #f56c6c; }
.group-wrap { margin-bottom: 16px; }
.group-card :deep(.el-card__header) { padding: 12px 20px; background: #fafafa; }
.group-card :deep(.el-card__body) { padding-bottom: 0; }
.group-head { display: flex; align-items: center; gap: 8px; }
.group-head b { font-size: 15px; }
.mon-card { cursor: pointer; }
.card-head { display: flex; align-items: center; gap: 8px; }
.card-sub {
  color: #909399; font-size: 12px; margin: 6px 0 10px;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.dot { width: 10px; height: 10px; border-radius: 50%; background: #909399; }
.dot.up { background: #67c23a; }
/* 黄色预警:低于阈值但还没判 DOWN(与列表页状态条图例同一个 #e6a23c)。 */
.dot.breach { background: #e6a23c; }
.dot.down { background: #f56c6c; }
.dot.unknown { background: #909399; }
.dot.paused { background: #c0c4cc; }
/* 三个可用率窗口一行、成功率/延时第二行:固定三列网格让每张卡片高度一致。 */
.card-metrics {
  display: grid; grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px 8px; margin-bottom: 10px;
}
.card-metrics > div { min-width: 0; }
.card-metrics .val { font-size: 19px; font-weight: 600; color: #303133; }
.card-metrics .unit { font-size: 12px; color: #909399; }
.card-metrics .lbl { font-size: 12px; color: #909399; }
.card-agents { display: flex; gap: 6px; flex-wrap: wrap; }
.adot {
  font-size: 12px; color: #67c23a; border: 1px solid #e4e7ed;
  border-radius: 3px; padding: 0 6px;
}
.adot.bad { color: #f56c6c; }
</style>
