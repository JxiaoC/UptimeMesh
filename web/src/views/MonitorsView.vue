<script setup lang="ts">
import { computed, markRaw, nextTick, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref, shallowRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { ElMessage, type TableInstance } from 'element-plus'
import { http } from '../api/http'
import { connected, connectionId, onRealtime, sendRealtime } from '../api/realtime'
import { confirmBox } from '../utils/confirm'
import { openMonitorDetail } from '../utils/monitorDetailDialog'
import MonitorFormDialog from './MonitorFormDialog.vue'
import MonitorStateTag from './MonitorStateTag.vue'
import StatusStrip from './StatusStrip.vue'
import { flagClass, regionName } from '../utils/region'

// 组件名是 MainLayout 里 <keep-alive :include="['MonitorsView']"> 的匹配依据:
// 显式声明,免得将来改文件名或构建期推断失灵,列表页就悄悄丢了缓存。
defineOptions({ name: 'MonitorsView' })

interface RoundCell {
  // up=达标(绿);breach=破线但未判 DOWN(黄);down=已判 DOWN 的破线(红);unknown=无样本(灰)。
  status: 'up' | 'breach' | 'down' | 'unknown'
  scheduledAt: string
  successRate: number
  // speedKbps 仅下载速度监控有值(本轮平均速度,KB/s):状态条悬停显示速度而不是成功率。
  speedKbps?: number
  // roundId 只用于去重(同一轮可能被重复推送),不展示。
  roundId?: string
}
// Monitor 是"配置行":只在配置变化时整行替换,行内不放随轮次变化的状态。
interface Monitor {
  id: string
  type: string
  name: string
  group?: string
  enabled: boolean
  period: number
  timeout: number
  threshold: number
  consecutive: number
  url?: string
  method?: string
  targetHost?: string
  port?: number
  // speedUnit 仅 download 类型有值:threshold 的解释单位(KB/s | MB/s)。
  speedUnit?: string
  pushUrl?: string
  lastPushAt?: string
  invertMode?: boolean
  // ipVersion 是探测使用的 IP 协议族:auto(默认)/ ipv4 / ipv6;push 类型恒为空。
  ipVersion?: string
  assignMode?: 'selected' | 'all' | 'exclude'
  excludedAgentIds?: string[]
  assignedAgentIds: string[]
  channelIds: string[]
}
// MonitorRow 是 GET /monitors、monitor_changed、批量操作响应的原始行:配置 + 活状态。
interface MonitorRow extends Monitor {
  displayState?: string
  alertState?: string
  consecutiveBreaches?: number
  recentRounds?: RoundCell[]
}
// LiveState 是页面上"会随推送变化"的那部分:展示状态 + 状态条。
interface LiveState {
  displayState: string
  rounds: RoundCell[]
}
interface Agent {
  id: string
  name: string
  status: string
  online: boolean
  /** 节点自报的本机网络族可用性;null/缺省 = 未上报(老版本节点)。 */
  ipv4Available?: boolean | null
  ipv6Available?: boolean | null
}
interface Channel {
  id: string
  name: string
  /** 启用状态:false = 禁用,勾选着也不会收到告警(弹窗里标「已禁用」)。 */
  enabled?: boolean
}

const { t, locale: i18nLocale } = useI18n()
const router = useRouter()

// ---- 数据模型:配置与活状态分家 ----
//
// 为什么不在 monitors 的行对象里直接改 displayState / recentRounds(那样写起来更直观):
// 列表页在 200+ 监控时会被压垮。实测(headless Chrome,100 行 × 50 格状态条,
// Element Plus 2.14)一次 round_finalized 要 ~280ms,原因是两处"改一行、渲染全表":
//   1) el-table 对 data 做 deep watch,行内任何字段变化都会触发布局重算(doLayout)+
//      整表重渲染;deep watch 还会把整份 data(行数 × 50 格)遍历一遍;
//   2) 表格插槽里读到的响应式字段会被 TableBody 的渲染副作用收集,于是"某一行的
//      displayState 变了"会连带重渲染整表(219 行 × 11 列)的全部单元格与标签、按钮。
// 所以这里做成:
//   - monitors 是 shallowRef,行对象 markRaw(deep watch 的 traverse 会跳过 __v_skip);
//   - 活状态放在独立的响应式 live 表里,只有叶子组件(MonitorStateTag / StatusStrip)
//     按 id 自己去读 —— 一次轮次定稿只重渲染那一行的状态标签与状态条(DOM 改动 6 处,
//     对照改造前同场景 811 处)。
const monitors = shallowRef<Monitor[]>([])
const live = reactive<Record<string, LiveState>>({})
const agents = ref<Agent[]>([])
// 名称映射含已删除节点:指派/排除列表里引用的历史节点仍要显示名称。
const agentNames = ref<Record<string, string>>({})
// 地域映射与名称同源:指派/排除标签里在节点名前画国旗(与详情页节点明细同口径)。
const agentRegions = ref<Record<string, string>>({})
const channels = ref<Channel[]>([])
const loading = ref(false)
const dialogVisible = ref(false)
const editing = ref<Monitor | null>(null)
// 多选:表格勾选列 + 批量操作。selectedIds 是我们自己的事实源(勾选列会带着行对象来回,
// 而那些行对象会被实时推送换掉),行被删掉时同步清理。
const tableRef = ref<TableInstance>()
const selectedIds = ref<string[]>([])
const bulkAction = ref<'' | 'pause' | 'resume' | 'delete'>('')
const bulkRunning = computed(() => bulkAction.value !== '')
// 分组过滤:ALL = 全部分组(默认);UNGROUPED = 未分组;其余为具体分组名。
const ALL = '__all__'
const UNGROUPED = '__ungrouped__'
const groupFilter = ref(ALL)
// 启用状态过滤:同一个 ALL 表示「全部状态」;暂停的监控不产生新轮次,
// 单筛出来既能看清单,也方便全选后批量恢复。
const ENABLED = '__enabled__'
const PAUSED = '__paused__'
const stateFilter = ref(ALL)
const groups = computed(() =>
  Array.from(new Set(monitors.value.map((m) => m.group).filter((g): g is string => !!g))).sort(),
)
const hasUngrouped = computed(() => monitors.value.some((m) => !m.group))
// 搜索:按名称或目标匹配(不区分大小写、包含即可)。目标文本与列表里显示的一致
// (HTTP=方法+URL、TCP=主机:端口、push=上报地址),所以"看到什么就能搜什么"。
// 输入即时回显,过滤用防抖后的关键字:200+ 行时每敲一个字都整表重渲染会卡
// (见文件顶部说明),150ms 之后落位足够跟手。
const keyword = ref('')
const appliedKeyword = ref('')
let keywordTimer: number | undefined
watch(keyword, (v) => {
  if (keywordTimer !== undefined) clearTimeout(keywordTimer)
  keywordTimer = window.setTimeout(() => {
    appliedKeyword.value = v.trim().toLowerCase()
  }, 150)
})
const filteredMonitors = computed(() => {
  let list = monitors.value
  // 启用状态在前:它是"这些监控此刻是否在跑"的粗筛,分组与关键字再在其上细筛。
  if (stateFilter.value === ENABLED) list = list.filter((m) => m.enabled)
  else if (stateFilter.value === PAUSED) list = list.filter((m) => !m.enabled)
  if (groupFilter.value === UNGROUPED) list = list.filter((m) => !m.group)
  else if (groupFilter.value !== ALL) list = list.filter((m) => m.group === groupFilter.value)
  const kw = appliedKeyword.value
  if (!kw) return list
  return list.filter(
    (m) => m.name.toLowerCase().includes(kw) || targetText(m).toLowerCase().includes(kw),
  )
})
// 列表为空时的提示要区分原因:搜不到 / 当前状态或分组下没有 / 一个监控都没有。
const emptyText = computed(() => {
  if (appliedKeyword.value) return t('monitors.emptyNoMatch')
  if (!monitors.value.length) return t('monitors.emptyNone')
  if (stateFilter.value === PAUSED) return t('monitors.emptyPaused')
  if (stateFilter.value === ENABLED) return t('monitors.emptyEnabled')
  return t('monitors.emptyGroup')
})

// ---- 多选与批量操作 ----
// 选择用 id 集合表达:实时推送会就地替换行对象、还会按状态分档重排,靠行对象引用会丢选择。
const selectedSet = computed(() => new Set(selectedIds.value))
const selectedMonitors = computed(() => monitors.value.filter((m) => selectedSet.value.has(m.id)))
// 批量暂停/恢复只作用于"状态确实要变"的那些:已暂停的再暂停没有意义,计数也要如实。
const pausableMonitors = computed(() => selectedMonitors.value.filter((m) => m.enabled))
const resumableMonitors = computed(() => selectedMonitors.value.filter((m) => !m.enabled))
// 不在当前筛选里、却被选中的行数(分组切换后仍保留选择,提示一下免得"看不见却被操作")。
const hiddenSelected = computed(
  () => selectedIds.value.length - filteredMonitors.value.filter((m) => selectedSet.value.has(m.id)).length,
)
const allVisibleSelected = computed(
  () => filteredMonitors.value.length > 0 && hiddenSelected.value === 0
    && selectedIds.value.length === filteredMonitors.value.length,
)
let timer: number | undefined
let offs: (() => void)[] = []

// 列表页在 200+ 监控时不能靠"整表重拉"跟进实时状态:每个轮次定稿都拉一次
// /monitors(过去每个监控还带几十轮状态条)会立刻把页面压垮。改为**增量更新**:
// 服务端在 round_finalized / monitor_flipped / monitor_changed / monitor_deleted
// 事件里带上列表页就地更新所需的字段(状态 + 状态条单元),这里直接改那一行。
//
// 「最近状态」那一列(状态条)与列表请求是分开的两条道:
//   - GET /monitors 只给配置 + 展示状态,不带 recentRounds —— 色块是每监控 N 格历史
//     状态(默认 50、上限 200),200+ 监控时上万个格子、约 1MB,整张表要等它到齐才
//     渲染,进页面因此要等好几秒;
//   - 色块在本页挂载/重连后用 WS 主动要一次快照(monitor_strips,服务端分块推送,
//     首块几十毫秒就到,色块一片片长出来),之后的新轮次由 round_finalized 追加;
//   - WS 不可用(被代理挡掉/断线)时退回 GET /monitors/strips,载荷同形。
//
// 状态条格数是**后台设置**(「设置 → 最近状态格数」,默认 50):本页要多少格
// (?rounds= 传给 /monitors/strips 与 WS 快照请求)、收下推送的色块后裁到多少格
// (clampStrip)、新轮次追加后裁到多少格(applyRound)用的是同一个数,服务端也按它
// 取数 —— 两边不一致就会出现"编辑保存后色块凭空变多"(见 .scratch/monitors-strip-length)。
const DEFAULT_STRIP_ROUNDS = 50
const stripRounds = ref(DEFAULT_STRIP_ROUNDS)
// 状态条列宽跟着格数走:列宽固定时,格数一多色块就溢出列外(实测 100 格已装不下),
// 看起来就像"色块变多了"。每格 6px + 1px 间隙,其余留给表头图例与列内边距:
//   50 格 ⇒ 390px,100 格 ⇒ 740px,200 格 ⇒ 1440px。
//
// 下限不是拍出来的常数,而是**运行时实测表头**(标题 + 四个色点图例)需要的宽度:
// 表头宽度随语言变(实测真实页面:中文 306px、英文 459px —— 英文长在
// 「Not up to standard」187px 那一段),写死一个数必然会有一边被切或被撑宽。
// 所以拿实测值当保底:格数一多就由格数公式说了算,格数少时也不会窄到切掉表头。
// 实测发生在渲染后(见 measureStripHeader),字体就绪与切语言时会重新测。
const STRIP_HEADER_FALLBACK = 360
const stripHeaderFloor = ref(0)
const stripColumnWidth = computed(() => Math.max(
  40 + stripRounds.value * 7,
  stripHeaderFloor.value || STRIP_HEADER_FALLBACK,
))
// stripRoundsLoading 让挂载与"回到本页"共用同一次设置请求(两者相邻触发)。
let stripRoundsLoading: Promise<boolean> | null = null

// measureStripHeader 量出「最近状态」列表头真正需要的宽度(标题 + 图例 + 单元格内边距),
// 作为列宽的下限。为什么要运行时量而不是写死常数:
//   - 表头宽度随**语言**变(实测:中文 306px、英文 459px —— 差在英文图例里的
//     「Not up to standard」);
//   - 也随**字体**变(首屏字体未就绪时量出来偏窄)。
// 所以挂载后、字体就绪后、以及切语言后都重测一次(见 onMounted / document.fonts.ready)。
// 量法:表头 .strip-head 是 nowrap 的 inline 内容,直接取它的 BoundingClientRect 宽度
// 即为内容宽度;再加单元格左右内边距。列宽还没铺开时(表格首帧)量不到就保持缺省。
function measureStripHeader() {
  const head = tableRef.value?.$el?.querySelector?.('.strip-head') as HTMLElement | null
  if (!head) return
  const cell = head.closest('.cell') as HTMLElement | null
  const cs = cell ? getComputedStyle(cell) : null
  const pad = cs ? parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight) : 24
  // ceil 向上取整:避免亚像素差让最后一段文字被切。
  const need = Math.ceil(head.getBoundingClientRect().width + pad)
  if (need > 0 && need !== stripHeaderFloor.value) stripHeaderFloor.value = need
}
// refreshStripRounds 读回「最近状态格数」,返回格数是否变化。读不到就沿用当前值
// (默认 50 与服务端默认一致)。变化意味着服务端会按新的格数给色块:已经渲染的
// 旧数组长短不一,调用方要按新格数再要一次快照(requestStrips(force)),而不是静默对齐。
function refreshStripRounds(): Promise<boolean> {
  if (!stripRoundsLoading) {
    stripRoundsLoading = (async () => {
      const before = stripRounds.value
      try {
        const s = (await http.get('/settings')) as unknown as { statusStripRounds?: number }
        const n = Math.floor(Number(s.statusStripRounds))
        if (Number.isFinite(n) && n > 0) stripRounds.value = n
      } catch { /* 读不到就用当前值(拦截器已提示) */ }
      return stripRounds.value !== before
    })().finally(() => { stripRoundsLoading = null })
  }
  return stripRoundsLoading
}
// clampStrip 把服务端给的色块裁到本页要的格数。服务端推来的整行(monitor_changed)
// 里的 recentRounds 是按设置取的,前端不裁就会出现"编辑保存后色块凭空变多"
// (实测:保存后色块长度翻倍,列宽装不下、悬停还对不上;要等下一次轮次定稿
// 被 applyRound 的裁剪拉回来)。这里按本页的格数收口,设置改动也不会漏到页面上。
function clampStrip(rounds: RoundCell[]): RoundCell[] {
  return rounds.length > stripRounds.value ? rounds.slice(-stripRounds.value) : rounds
}
let monitorsLoading = false
// loadedOnce 区分「第一次进本页」和「从详情页/别的菜单切回来」:前者必须空表 + 转圈,
// 后者直接吃 keep-alive 留下的那份数据,再静默对齐(见 onActivated)。
let loadedOnce = false

// configOf 把接口行拆成"配置行":活状态一并摘掉(它们进 live 表,见 syncLive)。
// markRaw 让这行不进响应式系统:配置变化一律走整行替换 + 换数组,行内绝不就地改。
function configOf(row: MonitorRow): Monitor {
  const { displayState, alertState, consecutiveBreaches, recentRounds, ...config } = row
  // 摘出来的活状态字段这里用不上(已进 live 表),显式 void 掉以过 noUnusedLocals。
  void displayState
  void alertState
  void consecutiveBreaches
  void recentRounds
  return markRaw(config as Monitor)
}

// syncLive 用接口行的活状态对齐 live 表:有则就地更新(只重渲染对应叶子组件),
// 没有的条目(监控已被别处删掉)顺手清掉。
//
// 行里**没有**色块(GET /monitors 只给配置),所以这里绝不碰 rounds:
// 存量色块由推送与快照维护,一次兜底对齐不该把它们清空。
function syncLive(rows: MonitorRow[]) {
  const seen = new Set<string>()
  for (const row of rows) {
    const id = row.id
    seen.add(id)
    const cur = live[id]
    const state = row.displayState || 'UNKNOWN'
    if (!cur) {
      // 快照可能比列表先到(两条道是并发的):那时 live 里已经有色块了,别覆盖。
      live[id] = { displayState: state, rounds: clampStrip(row.recentRounds ?? []) }
      continue
    }
    if (cur.displayState !== state) cur.displayState = state
    if (row.recentRounds) cur.rounds = clampStrip(row.recentRounds)
  }
  for (const id of Object.keys(live)) if (!seen.has(id)) delete live[id]
}

// loadMonitors 首次/兜底拉取整表(WS 断线时也靠 30s 轮询兜底)。
// 只拉配置与展示状态:色块另走 requestStrips(见文件顶部说明),响应体因此只随
// 监控条数增长 —— 200+ 监控时也从 MB 级降到几十 KB。
async function loadMonitors(silent = false) {
  if (monitorsLoading) return
  monitorsLoading = true
  if (!silent) loading.value = true
  try {
    const rows = (await http.get('/monitors')) as unknown as MonitorRow[]
    monitors.value = rows.map((r) => configOf(r))
    syncLive(rows)
    loadedOnce = true
  } finally {
    monitorsLoading = false
    if (!silent) loading.value = false
  }
}

// loadAgents 节点列表与名称映射(挂载时、回到本页时、节点变化时刷新)。
async function loadAgents() {
  agents.value = (await http.get('/agents?status=approved')) as unknown as Agent[]
  // 表单选择器只用已批准节点;此处的全量(含已删除)仅用于回显引用过的节点名。
  const all = (await http.get('/agents?includeDeleted=1')) as unknown as {
    id: string
    name: string
    region?: string
  }[]
  agentNames.value = Object.fromEntries(all.map((a) => [a.id, a.name]))
  // 地域与名称共用这一次拉取(不再多发一个请求);节点页改地域后回到本页即刷新(见 onActivated)。
  agentRegions.value = Object.fromEntries(all.map((a) => [a.id, a.region || '']))
}

// loadChannels 通知渠道(挂载与回到本页时取、打开表单时取:列表页本身不展示渠道)。
async function loadChannels() {
  channels.value = (await http.get('/channels')) as unknown as Channel[]
}

// ---- 最近状态(状态条):进页面单独取,之后由推送增量维护 ----

// StripsData 是状态条快照的一块,与 webhub.MonitorStripsData 同构(WS 分块推送与
// HTTP 兜底 GET /monitors/strips 两条道共用同一份载荷)。
interface StripsData {
  rounds: number
  total: number
  seq: number
  strips: { monitorId: string; cells: RoundCell[] }[]
}

// STRIP_WS_GRACE_MS 是"等 WS 连上"的宽限:首屏刚 mount 时连接可能还没建立,
// 直接走 HTTP 会把最慢的那条路当默认;超过这个时间还没连上才退回 HTTP。
const STRIP_WS_GRACE_MS = 2000
// STRIP_HTTP_MIN_INTERVAL_MS 是断线期间 HTTP 兜底的最小间隔:此时没有增量推送,
// 只能靠它刷新,但 5s 一次的轮询拉一遍上万格色块并不划算(轮次是分钟级的)。
const STRIP_HTTP_MIN_INTERVAL_MS = 30000
// stripsReqConn 是最近一次发起请求时的连接代次:同一条连接上只要一次
// (首屏 mount 与 onActivated 相邻触发,不该要两遍上万格)。
let stripsReqConn = -1
let stripsGraceTimer: number | undefined
let stripsHttpAt = 0
// stripsGot/stripsTotal 是这一批快照的收帧进度:分块推送是"发完才算数"的,
// 连接在中间断掉会只剩前半截(后面几块永远不来),靠它在下一次兜底轮询里补一次。
let stripsGot = 0
let stripsTotal = 0

// requestStrips 要一次状态条快照(WS 优先,WS 不可用则 HTTP 兜底)。
//
// 先等「最近状态格数」设置回来:不然会先用默认格数要一次,设置回来发现不同又得再要
// 一次(200+ 监控时每次都是 MB 级流量)。设置请求本身是并发去重的(refreshStripRounds),
// 所以首屏 mount / connected 变化 / onActivated 同时触发也只发一次。
async function requestStrips(force = false) {
  await refreshStripRounds()
  stripsGot = 0
  stripsTotal = 0
  if (!connected.value) {
    // WS 还没连上(首屏刚开始,或正在重连):稍等一会;一直连不上才退回 HTTP,
    // 免得把最慢的那条路当默认。
    if (stripsGraceTimer !== undefined) window.clearTimeout(stripsGraceTimer)
    stripsGraceTimer = window.setTimeout(() => { if (!connected.value) void loadStripsHttp() }, STRIP_WS_GRACE_MS)
    return
  }
  if (!force && stripsReqConn === connectionId.value) return
  stripsReqConn = connectionId.value
  if (!sendRealtime('monitor_strips', { rounds: stripRounds.value })) void loadStripsHttp()
}

// loadStripsHttp 用 HTTP 兜底拉一次状态条(WS 不可用时;载荷与推送同形)。
async function loadStripsHttp() {
  stripsHttpAt = Date.now()
  try {
    const data = (await http.get(`/monitors/strips?rounds=${stripRounds.value}`)) as unknown as StripsData
    applyStrips(data)
  } catch { /* 拦截器已提示;下一轮兜底会再试 */ }
}

// applyStrips 把一块快照落到 live 表。
// 快照可能比列表先到(两条道并发发出,色块那条还更小):这时先占位建条目,
// 等列表回来由 syncLive 补上展示状态 —— 直接丢弃的话这一列会空着,要等下一次
// 重连/刷新才有机会补。
function applyStrips(data: StripsData) {
  for (const s of data.strips || []) {
    const state = live[s.monitorId] ?? (live[s.monitorId] = { displayState: 'UNKNOWN', rounds: [] })
    state.rounds = mergeStrip(s.cells || [], state.rounds)
    stripsGot++
  }
  stripsTotal = Math.max(stripsTotal, data.total || 0)
  stripsHttpAt = Date.now() // 收到快照就算刚对齐过,断线兜底不必紧接着再来一次
}

// mergeStrip 归并"拉回来的快照"与"实时推来的色块":两者必然交叠 —— 快照查库之后、
// 到达之前定稿的轮次,推送已经追加进 rounds,而快照里没有。直接覆盖会把这一格丢掉
// (要等下一个周期才补回来),所以按计划时间归并:以快照为准,只保留比快照最后一格
// 更新的那些(rounds 内部按时间升序,scheduledAt 是定宽文本,可直接比大小)。
function mergeStrip(snapshot: RoundCell[], current: RoundCell[]): RoundCell[] {
  if (!snapshot.length) return current
  const last = snapshot[snapshot.length - 1].scheduledAt
  const newer = current.filter((c) => c.scheduledAt > last)
  return clampStrip(newer.length ? [...snapshot, ...newer] : snapshot)
}

// ---- 实时增量更新 ----

// RoundFinalizedData / MonitorFlippedData:与 webhub 的载荷字段一致;
// monitor_changed 直接推来与 GET /monitors 一行同构的对象。
interface StatePush {
  monitorId: string
  displayState?: string
  alertState?: string
  consecutiveBreaches?: number
}
interface RoundPush extends StatePush {
  roundId: string
  state: string
  successRate: number
  // speedKbps 仅下载速度监控有值(本轮平均速度,KB/s)。
  speedKbps?: number
  scheduledAt: string
  roundStatus: RoundCell['status']
}

// stateRank 与后端 monitorStateRank 同口径:故障优先、暂停最后。
function stateRank(state: string): number {
  switch (state) {
    case 'DOWN': return 0
    case 'UNKNOWN': return 1
    case 'UP': return 2
    default: return 3
  }
}

// 重排 = 换 data 数组 = 整表重渲染(200+ 行时一次上百毫秒),所以必须合并:
// 单独一次翻转等一个防抖窗口再落位(用户看到的顺序最多晚 200ms),
// 一整轮同时定稿/一波故障则只重排一次;持续抖动时至少每 1s 落位一次。
// 这里读 live 是在事件回调/定时器里,不产生渲染依赖。
const SORT_DEBOUNCE_MS = 200
const SORT_MAX_WAIT_MS = 1000
let sortTimer: number | undefined
let sortFirstAt = 0
function scheduleSort() {
  const now = Date.now()
  if (!sortFirstAt) sortFirstAt = now
  if (sortTimer !== undefined) clearTimeout(sortTimer)
  const wait = Math.max(0, Math.min(SORT_DEBOUNCE_MS, sortFirstAt + SORT_MAX_WAIT_MS - now))
  sortTimer = window.setTimeout(() => {
    sortTimer = undefined
    sortFirstAt = 0
    sortMonitors()
  }, wait)
}

// byRank 与后端 monitorStateRank 同口径:故障优先、暂停最后。
function byRank(a: Monitor, b: Monitor): number {
  return stateRank(live[a.id]?.displayState || '') - stateRank(live[b.id]?.displayState || '')
}

// sortMonitors 重排整表(仅在分档变化时调用)。换 data 数组 = 整表重渲染,
// 所以顺序真变了才换:"整批一起翻转"(整机房掉线/整批恢复)常常所有行落到同一分档,
// 相对顺序其实没动,这种时候一次重渲染都不该发生。
function sortMonitors() {
  const sorted = [...monitors.value].sort(byRank)
  for (let i = 0; i < sorted.length; i++) {
    if (sorted[i] !== monitors.value[i]) {
      monitors.value = sorted
      return
    }
  }
}

// applyState 把推送里的状态字段落到 live 表,返回该行的分档是否变化(需要重排)。
// 载荷里的 alertState / consecutiveBreaches 列表页不展示,不再往页面状态里存。
function applyState(state: LiveState, push: StatePush): boolean {
  const before = stateRank(state.displayState)
  if (push.displayState) state.displayState = push.displayState
  return before !== stateRank(state.displayState)
}

// applyRound 轮次定稿:更新状态并把这一轮追加到状态条(同一轮重复推送则覆盖)。
// rounds 是就地 push:响应式数组的内容变化只会重渲染读它的那个 StatusStrip。
function applyRound(push: RoundPush) {
  const state = live[push.monitorId]
  if (!state) return
  const reorder = applyState(state, push)
  const cell: RoundCell = {
    status: push.roundStatus,
    scheduledAt: push.scheduledAt,
    successRate: push.successRate,
    speedKbps: push.speedKbps,
    roundId: push.roundId,
  }
  const list = state.rounds
  if (list.length && list[list.length - 1].roundId === cell.roundId) list[list.length - 1] = cell
  else list.push(cell)
  if (list.length > stripRounds.value) list.splice(0, list.length - stripRounds.value)
  if (reorder) scheduleSort()
}

// upsertMonitors 配置变化(新建/编辑/暂停恢复/批量):**一次**换成新数组,
// 而不是逐行写数组(逐行写会让整表重渲染 N 次);顺带按分档落位,免得再排一次。
function upsertMonitors(rows: MonitorRow[]) {
  if (!rows.length) return
  const next = new Map(monitors.value.map((m) => [m.id, m]))
  for (const row of rows) {
    next.set(row.id, configOf(row))
    const state = live[row.id]
    if (!state) live[row.id] = { displayState: row.displayState || 'UNKNOWN', rounds: clampStrip(row.recentRounds ?? []) }
    else {
      if (row.displayState) state.displayState = row.displayState
      // 批量启停推送不带 recentRounds:此时保留原有色块。
      if (row.recentRounds) state.rounds = clampStrip(row.recentRounds)
    }
  }
  // 已有行保持原相对顺序,新增行先追加在末尾,排序后落到各自分档位置。
  monitors.value = [...next.values()].sort(byRank)
}

function removeMonitor(monitorId: string) {
  const row = monitors.value.find((m) => m.id === monitorId)
  monitors.value = monitors.value.filter((m) => m.id !== monitorId)
  selectedIds.value = selectedIds.value.filter((id) => id !== monitorId)
  delete live[monitorId]
  // reserve-selection 会把"已从数据里消失的行"留在表格内部的选择里(幽灵选中项):
  // 用删除前的行对象显式摘掉它,否则计数里会一直挂着一个不存在的监控。
  if (row) tableRef.value?.toggleRowSelection(row, false)
}

// onOpenDetail 点监控名打开详情弹窗。打开前先把滚动位置记下来只是顺手 ——
// 弹窗不改变列表页的滚动位置(页面还在原地),这里记一次是为了"以后回到本页"的兜底
// (见 captureScroll 的说明:那是进详情页的主路径,再记一层不依赖最后一次 scroll 事件)。
function onOpenDetail(id: string) {
  captureScroll()
  openMonitorDetail(id)
}

// detailHref 监控名的 href:用 router.resolve 生成真正的 /monitors/:id 地址,
// 让中键 / Ctrl+点击 / 右键"复制链接"仍然指向独立详情页(它还在,深链有效)。
// 普通左键点击被 @click.prevent 拦下、改开弹窗。
const detailHref = (id: string) => router.resolve({ name: 'monitorDetail', params: { id } }).href

// ---- 滚动位置:回到本页时回到离开前的地方 ----
//
// keep-alive 留住了整棵 DOM,但**纵向滚动的是 MainLayout 里的 el-main**(它常驻,不随本页
// 缓存):详情页比列表页短,浏览器会把 el-main.scrollTop 夹到详情页能滚到的位置
// (实测 693 → 101),回来自然弹不回原处。表格自己的横向滚动容器是组件内部 DOM,
// 跟着缓存一起被摘下/挂回,位置本来就保得住;这里一并记/还,免得将来表格重排把它冲掉。
//
// 为什么用滚动监听记录、而不是在 onDeactivated 里读一次:那一刻 DOM 已经在换,读到的多半
// 是被夹过的值。监听里带 active 闸门:本页不可见之后浏览器夹出来的那次 scroll 事件不会再
// 覆盖记录(鼠标滚轮的每一次滚动都记过,离开的那一刻记录已经是最新的)。
const rootRef = ref<HTMLElement>()
let mainScroller: HTMLElement | null = null
let tableScroller: HTMLElement | null = null
const savedScroll = { main: 0, table: 0 }
// 记录不进响应式系统:滚动是高频事件,只写普通对象,一次重渲染都不该引起。
let scrollActive = true
let scrollOffs: (() => void)[] = []

// captureScroll 记下当前滚动位置(滚动监听与点击监控链接时调用)。
function captureScroll() {
  if (!scrollActive) return
  if (mainScroller) savedScroll.main = mainScroller.scrollTop
  if (tableScroller) savedScroll.table = tableScroller.scrollLeft
}

// bindScrollHosts 找到两个滚动容器并挂上被动监听;重复调用会先摘掉旧的(激活时重绑一次)。
function bindScrollHosts() {
  const root = rootRef.value
  if (!root) return
  mainScroller = root.closest('.el-main')
  // el-table 的横向滚动容器(固定列与主体共用同一个 scrollbar 容器)。
  tableScroller = root.querySelector('.el-table__body-wrapper .el-scrollbar__wrap')
  scrollOffs.forEach((off) => off())
  scrollOffs = []
  for (const host of [mainScroller, tableScroller]) {
    if (!host) continue
    host.addEventListener('scroll', captureScroll, { passive: true })
    scrollOffs.push(() => host.removeEventListener('scroll', captureScroll))
  }
}

// restoreScroll 把位置写回去:行数与离开时一致(keep-alive 留下的 DOM),高度已经是对的,
// 直接写即可;再 nextTick 补一次,是因为随后那次静默对齐会让表格重排一遍。
function restoreScroll() {
  if (mainScroller) mainScroller.scrollTop = savedScroll.main
  if (tableScroller) tableScroller.scrollLeft = savedScroll.table
}

onMounted(() => {
  // 列表与色块分头发出,互不阻塞:列表不再依赖「最近状态格数」设置(它只影响色块),
  // 色块由 requestStrips 内部先等设置回来再要(见该函数),所以首屏不必串行等。
  void loadMonitors()
  void requestStrips()
  void loadAgents()
  void loadChannels()
  void nextTick(bindScrollHosts) // 表格 DOM 就位后再找滚动容器
  // 表头宽度决定「最近状态」列宽的下限:表头 DOM 就位后量一次;
  // 字体是异步加载的,首帧量出来的可能偏窄,等 fonts 就绪后再补一次。
  void nextTick(() => {
    measureStripHeader()
    if (typeof document !== 'undefined' && document.fonts?.ready) {
      void document.fonts.ready.then(() => measureStripHeader())
    }
  })
  offs = [
    onRealtime('round_finalized', (d) => applyRound(d as RoundPush)),
    onRealtime('monitor_flipped', (d) => {
      const push = d as StatePush
      const state = live[push.monitorId]
      if (state && applyState(state, push)) scheduleSort()
    }),
    onRealtime('monitor_changed', (d) => upsertMonitors([d as MonitorRow])),
    onRealtime('monitor_deleted', (d) => removeMonitor((d as { monitorId: string }).monitorId)),
    // 批量操作(多选后暂停/恢复/删除)服务端合成一帧,不是逐行发。
    onRealtime('monitors_changed', (d) => upsertMonitors((d as { monitors?: MonitorRow[] }).monitors || [])),
    onRealtime('monitors_deleted', (d) => {
      const ids = (d as { monitorIds?: string[] }).monitorIds || []
      ids.forEach(removeMonitor)
    }),
    onRealtime('agent_changed', () => { void loadAgents() }),
    // 状态条快照(应答本页的请求,分块到达):一块一块地落,页面上的色块随之长出来。
    onRealtime('monitor_strips', (d) => applyStrips(d as StripsData)),
  ]
  // 兜底轮询:WS 在线时 30s 对齐一次,断线时 5s(此时增量推送收不到)。
  // 配置对齐只走 /monitors(几十 KB),色块另有节流的 HTTP 兜底(见 pollAlign)。
  timer = window.setInterval(pollAlign, 30000)
})
// pollAlign 是 WS 推送的兜底对齐:配置与状态每次轮询都比一遍(便宜),色块只在
// 断线时按节流补 —— 在线时它由 round_finalized 增量维护,不必每轮重拉上万格。
// 另外补一次"上一批快照没收全"(连接在分块推送中间断了)的情况。
function pollAlign() {
  void loadMonitors(true)
  if (stripsGot < stripsTotal) { void loadStripsHttp(); return }
  if (!connected.value && Date.now() - stripsHttpAt > STRIP_HTTP_MIN_INTERVAL_MS) void loadStripsHttp()
}
// 切语言后表头文案变了(中英表头宽度差 150px 以上),列宽下限要跟着重测,
// 否则会停在上一语言量出来的宽度上(英文切中文会白白宽一截,反之则被切)。
watch(i18nLocale, () => {
  void nextTick(() => {
    measureStripHeader()
    void tableRef.value?.doLayout?.()
  })
})
watch(connected, (online) => {
  if (timer) clearInterval(timer)
  timer = window.setInterval(pollAlign, online ? 30000 : 5000)
  // 连接状态一变(重连或断线)就重新对齐一次:断线窗口里的推送收不到,配置与色块
  // 都可能过期。色块那条不必 force —— 连接代次变了,requestStrips 本来就会重发;
  // 而首屏那次"还没连上"的请求是延后到这里的(连接建立时),force 只会让它发两遍。
  void loadMonitors(true)
  void requestStrips()
})
// onActivated 只在被 keep-alive 缓存的实例上触发(见 MainLayout):第一次挂载时
// 它紧跟 onMounted 触发 —— 那时首次拉取已经发出,loadMonitors 会被 monitorsLoading 去重;
// 之后每次"回到本页"(从详情页返回、切菜单回来)都走这里:
// 页面先用缓存的数据渲染(不转圈、不闪空表),这里再后台静默对齐一次。
// 传 loadedOnce 当 silent:正常回来不置 loading;只有"首次加载就没成功过"
// (loadedOnce 还是 false)才退回带转圈的加载,不然页面会一直空着。
// 节点/渠道顺带刷新:节点页改过的名称与地域要能带回列表页。
// 滚动位置在这里还回去:先同步写一次(回来的第一帧就在原位,看不出跳动),
// 再 nextTick 补一次(静默对齐会重排表格,补一次最稳)。
onActivated(() => {
  scrollActive = true
  bindScrollHosts()
  restoreScroll()
  void nextTick(restoreScroll)
  // 设置页可能刚改过「最近状态格数」:格数变了就得按新格数再要一次快照(服务端按新
  // 格数给色块,留着旧的会长短不一)。列表本身与格数无关,照老样子静默对齐即可。
  void refreshStripRounds().then((changed) => {
    void loadMonitors(loadedOnce)
    // 色块一直在被推送维护(keep-alive 期间订阅没摘),只有格数变了才需要重来一次;
    // 否则 requestStrips 会因为"这条连接上已经要过"而跳过 —— 断线时它自己会走 HTTP。
    if (changed) void requestStrips(true)
    else void requestStrips()
  })
  void loadAgents()
  void loadChannels()
})
// onDeactivated 只关闸门,不读位置:此刻 DOM 已经在换,读到的是被夹过的值;
// 位置由滚动监听一直记录着(见 captureScroll)。
onDeactivated(() => {
  scrollActive = false
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (sortTimer !== undefined) clearTimeout(sortTimer)
  if (keywordTimer !== undefined) clearTimeout(keywordTimer)
  if (stripsGraceTimer !== undefined) window.clearTimeout(stripsGraceTimer)
  offs.forEach((f) => f())
  scrollOffs.forEach((off) => off())
})

const agentName = (id: string) =>
  agentNames.value[id] || id.slice(0, 6)
// agentRegion 节点生效地域码(空 = 未知/未设置),旗子与地域名的唯一取值口。
const agentRegion = (id: string) => agentRegions.value[id] || ''

// targetText 列表里的目标展示:HTTP 与下载速度监控是"方法 URL";TCP 是"主机:端口";
// push(外部上报)没有目标,显示上报地址的相对路径(方便对照)。
function targetText(row: Monitor): string {
  if (row.type === 'http' || row.type === 'download') return `${row.method} ${row.url}`
  if (row.type === 'tcp') return `${row.targetHost}:${row.port}`
  if (row.type === 'push') return row.pushUrl || t('type.push')
  return row.targetHost || ''
}

// refresh 手动刷新:配置整表拉一次,色块也要重新要一次(用户点"刷新"就是想要
// 眼前这份数据是最新的,不能只刷新一半)。
function refresh() {
  void loadMonitors()
  void requestStrips(true)
}

function create() {
  editing.value = null
  dialogVisible.value = true
}
function edit(m: Monitor) {
  editing.value = { ...m }
  dialogVisible.value = true
}
async function toggle(m: Monitor) {
  await http.post(`/monitors/${m.id}/${m.enabled ? 'pause' : 'resume'}`)
  ElMessage.success(m.enabled ? t('monitors.msgPaused') : t('monitors.msgResumed'))
  await loadMonitors()
}
async function remove(m: Monitor) {
  await confirmBox(
    t('monitors.deleteConfirm', { name: m.name }),
    t('monitors.deleteConfirmTitle'),
    { type: 'warning' },
  )
  await http.delete(`/monitors/${m.id}`)
  ElMessage.success(t('common.deleted'))
  await loadMonitors()
}

// ---- 选择 ----

// onSelectionChange 勾选列变化:只认还在列表里的行(被删掉的行可能在表格内部的选择里
// 残留一拍,不能进入计数)。
function onSelectionChange(rows: Monitor[]) {
  const alive = new Set(monitors.value.map((m) => m.id))
  selectedIds.value = rows.map((r) => r.id).filter((id) => alive.has(id))
}

// selectAll / invertSelection 只翻当前列表里的行:有分组筛选时,"全选"不该把整库都选上,
// "反选"也不该动看不见的选择。只 toggle 真正需要变的行(而不是先全清再全勾),
// 免得 200 行时发出几百次 selection-change。
function selectAll() {
  const table = tableRef.value
  if (!table) return
  const current = selectedSet.value
  for (const m of filteredMonitors.value) {
    if (!current.has(m.id)) table.toggleRowSelection(m, true)
  }
}

function invertSelection() {
  const table = tableRef.value
  if (!table) return
  const current = selectedSet.value
  for (const m of filteredMonitors.value) table.toggleRowSelection(m, !current.has(m.id))
}

function clearSelection() {
  tableRef.value?.clearSelection()
}

// ---- 批量操作 ----

// 批量接口的响应:affected 是实际生效的行数(选中的监控可能刚被别处删掉)。
interface BatchResult {
  action: string
  requested: number
  affected: number
  monitors?: MonitorRow[]
}

// batch 把一批 id 交给服务端一次处理,并用响应里的最新行就地更新列表 ——
// 不重拉整表(200+ 监控时那是一次 MB 级请求),也不必等实时推送回来。
async function batch(action: 'pause' | 'resume' | 'delete', ids: string[]) {
  if (!ids.length) return
  bulkAction.value = action
  try {
    const res = (await http.post('/monitors/batch', { action, ids })) as unknown as BatchResult
    if (action === 'delete') {
      ids.forEach(removeMonitor)
      ElMessage.success(t('monitors.batchDeleted', { count: res.affected }))
    } else {
      upsertMonitors(res.monitors || [])
      ElMessage.success(t(action === 'pause' ? 'monitors.batchPaused' : 'monitors.batchResumed', {
        count: res.affected,
      }))
      // 推送里没覆盖到的行(例如刚被别处删掉)以服务端为准对齐一次。
      if (res.affected > (res.monitors || []).length) void loadMonitors(true)
    }
  } finally {
    bulkAction.value = ''
  }
}

async function bulkToggle(enabled: boolean) {
  await batch(enabled ? 'resume' : 'pause', (enabled ? resumableMonitors.value : pausableMonitors.value).map((m) => m.id))
}

async function bulkRemove() {
  const ids = selectedIds.value.slice()
  if (!ids.length) return
  try {
    await confirmBox(
      t('monitors.bulkDeleteConfirm', { count: ids.length }),
      t('monitors.bulkDeleteConfirmTitle'),
      { type: 'warning' },
    )
  } catch {
    return // 用户取消
  }
  await batch('delete', ids)
}
</script>

<template>
  <div ref="rootRef">
    <div style="display: flex; justify-content: space-between; align-items: center">
      <h3>{{ t('nav.monitors') }}</h3>
      <div style="display: flex; align-items: center">
        <el-input
          v-model="keyword"
          :placeholder="t('monitors.searchPlaceholder')"
          clearable
          style="width: 200px; margin-right: 12px"
        />
        <el-select v-model="groupFilter" style="width: 160px; margin-right: 12px" :placeholder="t('monitors.groupPlaceholder')">
          <el-option :label="t('monitors.allGroups')" :value="ALL" />
          <el-option v-for="g in groups" :key="g" :label="g" :value="g" />
          <el-option v-if="hasUngrouped" :label="t('common.ungrouped')" :value="UNGROUPED" />
        </el-select>
        <el-select v-model="stateFilter" style="width: 120px; margin-right: 12px" :placeholder="t('monitors.statePlaceholder')">
          <el-option :label="t('monitors.allStates')" :value="ALL" />
          <el-option :label="t('monitors.stateEnabled')" :value="ENABLED" />
          <el-option :label="t('monitors.statePaused')" :value="PAUSED" />
        </el-select>
        <el-button :loading="loading" @click="refresh">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" @click="create">{{ t('monitors.newMonitor') }}</el-button>
      </div>
    </div>
    <div class="bulk-bar">
      <span class="bulk-count">
        <!-- 计数里「已选 N 项」的 N 是加粗的,故走 i18n-t 的具名插槽 -->
        <i18n-t keypath="monitors.selectedSummary" scope="global">
          <template #selected><b>{{ selectedIds.length }}</b></template>
          <template #total>{{ monitors.length }}</template>
        </i18n-t>
        <span
          v-if="groupFilter !== ALL || stateFilter !== ALL || appliedKeyword"
          class="muted"
        >{{ t('monitors.filteredCount', { count: filteredMonitors.length }) }}</span>
        <span v-if="hiddenSelected > 0" class="muted">{{ t('monitors.hiddenSelected', { count: hiddenSelected }) }}</span>
      </span>
      <div>
        <el-button size="small" :disabled="!filteredMonitors.length || allVisibleSelected" @click="selectAll">
          {{ t('monitors.selectAll') }}
        </el-button>
        <el-button size="small" :disabled="!filteredMonitors.length" @click="invertSelection">{{ t('monitors.invertSelection') }}</el-button>
        <el-button size="small" :disabled="!selectedIds.length" @click="clearSelection">{{ t('monitors.clearSelection') }}</el-button>
        <el-button
          size="small"
          type="warning"
          plain
          :loading="bulkAction === 'pause'"
          :disabled="bulkRunning || !pausableMonitors.length"
          @click="bulkToggle(false)"
        >
          {{ t('common.pause') }} ({{ pausableMonitors.length }})
        </el-button>
        <el-button
          size="small"
          type="success"
          plain
          :loading="bulkAction === 'resume'"
          :disabled="bulkRunning || !resumableMonitors.length"
          @click="bulkToggle(true)"
        >
          {{ t('common.resume') }} ({{ resumableMonitors.length }})
        </el-button>
        <el-button
          size="small"
          type="danger"
          plain
          :loading="bulkAction === 'delete'"
          :disabled="bulkRunning || !selectedIds.length"
          @click="bulkRemove"
        >
          {{ t('common.delete') }} ({{ selectedIds.length }})
        </el-button>
      </div>
    </div>
    <el-table
      ref="tableRef"
      class="mon-table"
      :data="filteredMonitors"
      row-key="id"
      v-loading="loading"
      :empty-text="emptyText"
      @selection-change="onSelectionChange"
    >
      <el-table-column type="selection" width="42" :reserve-selection="true" />
      <!-- 状态列 120px:实测(真实页面字体 Noto Sans SC)最长的一种标签是英文 push 监控的
           「Awaiting push」—— 标签自身 ≈ 95px(文字 79 + 标签内边距/边框 16),加上单元格
           左右内边距 24px ⇒ 需要 ≈ 119px。缩到 116px 实测这一种标签就出省略号了,
           120px 留了一点富余。 -->
      <el-table-column :label="t('monitors.columnState')" width="120">
        <template #default="{ row }">
          <!-- 状态标签自己做叶子组件:插槽里只读稳定的 id/type,实时状态由组件内部查 live 表,
               这样"某行状态变了"不会连累整张表重渲染(见脚本顶部说明)。 -->
          <MonitorStateTag :live="live" :monitor-id="row.id" :monitor-type="row.type" />
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnName')" min-width="130" show-overflow-tooltip>
        <template #default="{ row }">
          <!-- 监控名点开详情弹窗(不再跳 /monitors/:id 独立页):列表里来回看几个监控时,
               跳页会把列表整页换掉、回来还得等恢复;弹窗读完关掉,位置与筛选原样还在。
               但 href 仍是**真实地址**:中键 / Ctrl+点击 / 右键"复制链接"照旧能拿到
               /monitors/:id 这个深链(独立页还在),只有普通左键点击被 .prevent 拦下改开弹窗。 -->
          <a
            class="mon-link"
            :href="detailHref(row.id)"
            @click.prevent="onOpenDetail(row.id)"
          >
            {{ row.name }}
          </a>
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnGroup')" width="110" show-overflow-tooltip>
        <template #default="{ row }">
          <el-tag v-if="row.group" size="small" type="info">{{ row.group }}</el-tag>
          <span v-else class="muted">{{ t('common.ungrouped') }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnType')" width="110">
        <template #default="{ row }">
          <!-- 最多同时出现三个标签(类型 + 反转 + IP 协议族),始终同一行、各自不折行:
               el-tag 没有 white-space:nowrap,列窄的时候"反转"两个字会在卡片里竖着断行。
               110px 是刻意的紧凑列宽:实测(真实页面字体)单个标签最宽 107px 放得下,
               但任两个标签就要 151px、三个全上要 196px(中)/219px(英),所以带附加标签的
               行会被列边缘切掉一部分。类型标签永远排第一个且完整 ——"是什么类型"始终看得见;
               被切的附加标签挂着 el-tooltip,悬停仍能看到完整说明。 -->
          <div class="type-cell">
            <el-tag size="small" :type="row.type === 'http' ? 'primary' : 'success'">
              {{ row.type.toUpperCase() }}
            </el-tag>
            <el-tooltip v-if="row.invertMode" :content="t('monitors.invertTip')" placement="top">
              <el-tag size="small" type="warning">{{ t('monitors.invertTag') }}</el-tag>
            </el-tooltip>
            <!-- 非 auto 才显示:auto 是默认值,标出来只会让每一行都多一个无信息的标签。 -->
            <el-tooltip v-if="row.ipVersion && row.ipVersion !== 'auto'" :content="t('monitors.ipVersionTip')" placement="top">
              <el-tag size="small" type="info" effect="plain">{{ row.ipVersion.toUpperCase() }}</el-tag>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnTarget')" min-width="130" show-overflow-tooltip>
        <template #default="{ row }">
          {{ targetText(row) }}
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnPeriod')" width="76">
        <template #default="{ row }">{{ row.period }}s</template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnThreshold')" width="150">
        <template #default="{ row }">
          <!-- 下载速度监控的阈值是速度:数值后面跟单位(KB/s | MB/s)而不是 %。 -->
          <template v-if="row.type === 'download'">
            {{ t('monitors.thresholdCellSpeed', {
              threshold: row.threshold, unit: row.speedUnit || 'KB/s', consecutive: row.consecutive,
            }) }}
          </template>
          <!-- 外部上报没有可用率阈值(每轮只有一个样本,非成功即失败):这一列对它
               表示的是"连续几轮没有成功上报就告警",失联判定 = 周期 × 该轮数。 -->
          <template v-else-if="row.type === 'push'">
            {{ t('monitors.thresholdCellPush', { consecutive: row.consecutive }) }}
          </template>
          <template v-else>
            {{ t('monitors.thresholdCell', { threshold: row.threshold, consecutive: row.consecutive }) }}
          </template>
        </template>
      </el-table-column>
      <el-table-column :label="t('monitors.columnRecentStatus')" :width="stripColumnWidth">
        <template #header>
          <span class="strip-head">
            {{ t('monitors.columnRecentStatus') }}
            <!-- 色块图例:红/黄的含义(故障 vs 低于阈值预警)在页面上要有出处,不能只靠悬停提示。 -->
            <span class="strip-legend">
              <i class="dot up" />{{ t('status.up') }}
              <i class="dot breach" />{{ t('status.breach') }}
              <i class="dot down" />{{ t('monitors.legendDown') }}
              <i class="dot unknown" />{{ t('status.noSample') }}
            </span>
          </span>
        </template>
        <template #default="{ row }">
          <!-- 状态条只吃 live 里的数组本身:数组内容就地变化只重渲染这一个 StatusStrip。
               下载速度监控额外传单位:悬停提示显示的是速度而不是成功率。 -->
          <StatusStrip
            :rounds="live[row.id]?.rounds || []"
            :speed-unit="row.type === 'download' ? (row.speedUnit || 'KB/s') : ''"
          />
        </template>
      </el-table-column>
      <!-- 指派节点列最大 220px:el-table-column 没有 max-width 属性,固定 width=220
           就是"最多 220px"的唯一落实方式 —— 用 min-width 的话,表格还有富余空间时
           这一列会跟着弹性列一起变宽,宽度不封顶(指派节点多的时候整列能吃掉半张表)。
           放不下的内容照旧由 show-overflow-tooltip 悬停看全。 -->
      <el-table-column :label="t('monitors.columnAgents')" width="220" show-overflow-tooltip>
        <template #default="{ row }">
          <template v-if="row.assignMode === 'all'">
            <el-tag size="small" type="warning">{{ t('monitors.allAgents') }}</el-tag>
          </template>
          <template v-else-if="row.assignMode === 'exclude'">
            <el-tag size="small" type="warning">{{ t('monitors.allAgents') }}</el-tag>
            <el-tag
              v-for="id in row.excludedAgentIds"
              :key="id"
              size="small"
              type="info"
              style="margin-left: 4px"
              :title="regionName(agentRegion(id))"
            >
              {{ t('monitors.excludePrefix') }} <span
                v-if="flagClass(agentRegion(id))"
                :class="flagClass(agentRegion(id))"
                class="agent-flag"
              />{{ agentName(id) }}
            </el-tag>
          </template>
          <template v-else>
            <el-tag
              v-for="id in row.assignedAgentIds"
              :key="id"
              size="small"
              style="margin-right: 4px"
              :title="regionName(agentRegion(id))"
            >
              <span
                v-if="flagClass(agentRegion(id))"
                :class="flagClass(agentRegion(id))"
                class="agent-flag"
              />{{ agentName(id) }}
            </el-tag>
          </template>
        </template>
      </el-table-column>
      <!-- 操作列不再用 el-table 的 fixed="right":那会让 EP 把整张表判成「复杂表」
           (isComplex),从而关掉原生 tr:hover、改用 JS(防抖 30ms + rAF)加 .hover-row 类，
           鼠标扫过列表时高亮不跟手。改为自己把这列贴住右边(class-name 见样式区),
           表格保持「非复杂表」:原生 :hover 即时高亮,EP 那条 JS 路径整条不再触发。 -->
      <el-table-column
        :label="t('monitors.columnActions')"
        width="230"
        class-name="col-actions"
        label-class-name="col-actions"
      >
        <template #default="{ row }">
          <div class="row-actions">
            <el-button size="small" @click="edit(row)">{{ t('common.edit') }}</el-button>
            <el-button
              size="small"
              :type="row.enabled ? 'warning' : 'success'"
              plain
              @click="toggle(row)"
            >
              {{ row.enabled ? t('common.pause') : t('common.resume') }}
            </el-button>
            <el-button size="small" type="danger" plain @click="remove(row)">
              {{ t('common.delete') }}
            </el-button>
          </div>
        </template>
      </el-table-column>
    </el-table>
    <MonitorFormDialog
      v-model="dialogVisible"
      :editing="editing"
      :agents="agents"
      :channels="channels"
      :groups="groups"
      @saved="() => { if (!connected) loadMonitors(); void loadChannels(); void loadAgents() }"
    />
  </div>
</template>

<style scoped>
.bulk-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin: 12px 0;
  padding: 8px 12px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
}
.bulk-count {
  font-size: 13px;
  color: var(--el-text-color-regular);
}
.mon-link {
  color: var(--el-color-primary);
  text-decoration: none;
  font-weight: 500;
}
.mon-link:hover {
  text-decoration: underline;
}
.muted {
  color: #909399;
  font-size: 12px;
}
/* 操作列自己贴住右边(替代 el-table 的 fixed="right"):只要表里有一列 fixed,EP 就把整张表
   判成「复杂表」(store/watcher.mjs 的 isComplex),于是既不挂 .el-table--enable-row-hover
   (原生 tr:hover 规则整条失效),又改用 JS 给行加 .hover-row —— events-helper 里那是
   lodash debounce(…, 30),table-body 还要再等一个 requestAnimationFrame 才落类,鼠标扫过
   列表时高亮要停下 30ms 才落位,看起来就是"拖尾/跳行"。
   这里照 EP 内部对固定列的做法自己贴一列(theme-chalk 里 el-table-fixed-column--right
   就是 position:sticky + background:inherit + 抬高 z-index),表格因此保持「非复杂表」:
   原生 :hover 由浏览器即时生效,EP 那条 JS 路径整条不再触发。实测两方案的列宽、滚动后
   贴边位置、悬停变灰完全一致。注意:本页任何一列都不要再加 fixed,否则高亮又会退回慢路径。 */
.el-table :deep(td.col-actions) {
  position: sticky;
  right: 0;
  z-index: calc(var(--el-table-index) + 1);
  background: inherit; /* 取行背景(不透明白底):横向滚动时下层内容不会透出来 */
}
.el-table :deep(th.col-actions) {
  position: sticky;
  right: 0;
  z-index: calc(var(--el-table-index) + 1);
  background-color: var(--el-table-header-bg-color);
}
/* 监控少于 100 行时 EP 会加上 .el-table--enable-row-transition(行单元格 250ms 背景过渡),
   那会把「即时」变成「淡入」;高亮的开关本来就该跟着指针走,故一并关掉。 */
.el-table :deep(.el-table__body td.el-table__cell) {
  transition: none;
}
/* 监控列表:所有列一律单行 —— 只要有一列折行,整行行高就被撑起来,
   行高参差不齐(英文文案比中文长得多,尤其明显)。列宽按「中文与英文里更宽的那个」
   配足,配不足的长内容(名称/目标/分组/指派节点)由 .cell 自带的
   overflow:hidden + text-overflow:ellipsis 省略,并用 show-overflow-tooltip 悬停看全。 */
.mon-table :deep(.cell) {
  white-space: nowrap;
}
/* 标签单元格(分组/类型/指派节点)放不下时,让省略号出现在标签内部 ——
   el-tag 是 inline-flex,只靠 .cell 的 text-overflow 会被硬切掉半截文字。 */
.mon-table :deep(.cell .el-tag) {
  max-width: 100%;
}
.mon-table :deep(.cell .el-tag .el-tag__content) {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* 操作列固定单行排列:英文下 Edit / Pause / Delete 比中文宽,列宽不足时按钮会换行错位。 */
.row-actions {
  display: flex;
  align-items: center;
  flex-wrap: nowrap;
}
:deep(.row-actions .el-button + .el-button) { margin-left: 8px; }
/* 类型列:最多三个标签(类型 + 反转 + IP 协议族)固定一行,标签内文字也不折行。
   列宽 110px(单元格内容只有 86px)是刻意收窄的,实测(真实页面字体 Noto Sans SC,
   所需列宽 = 标签内容 + 标签间距 4px + 单元格内边距 24px):
     · 单个标签最宽是 DOWNLOAD(标签 82.69px)⇒ 86px 放得下,单标签的行永远完整;
     · 类型 + 反转/IP 协议族任意两个组合需 151px,最宽(DOWNLOAD + Inverted)174px;
     · 三个全上需 196px(中)/219px(英)⇒ 带附加标签的行会被列边缘切掉一部分。
   这是本次"类型列就是 110px"有意接受的取舍:类型标签永远排在第一个、列内放得下,
   所以"这一行是什么类型"始终看得见;被切的附加标签(反转 / IP 协议族)本身挂着
   el-tooltip,悬停仍能看到完整说明(见 template 里的 invertTip / ipVersionTip)。
   不做"让标签收缩出省略号"之类的花活:实测那样会把附加标签压成只剩内边距的空壳
   (标签内文字宽度为 0,连省略号都显示不出来),反倒比直接切掉更难看。 */
.type-cell {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-wrap: nowrap;
}
.type-cell :deep(.el-tag) {
  white-space: nowrap;
}
/* 指派节点标签里的国旗:与节点页、详情页节点明细同口径(flag-icons 类名),
   地域未知时不渲染旗子,只显示名称。 */
.agent-flag {
  border-radius: 2px;
  font-size: 14px;
  line-height: 1;
  margin-right: 4px;
}
/* 状态条图例:与 StatusStrip 的色块同色 */
.strip-legend {
  margin-left: 10px;
  font-size: 11px;
  font-weight: 400;
  color: var(--el-text-color-secondary);
}
.strip-legend .dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 2px;
  margin: 0 3px 0 8px;
  vertical-align: middle;
}
.strip-legend .dot.up { background: #67c23a; }
.strip-legend .dot.breach { background: #e6a23c; }
.strip-legend .dot.down { background: #f56c6c; }
.strip-legend .dot.unknown { background: #c0c4cc; }
</style>
