<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { http, TOKEN_KEY } from '../api/http'
import { alertBox, confirmBox } from '../utils/confirm'

interface Settings {
  resultRetentionDays: number
  // 小时级聚合的固定保留天数(不可配,只读展示):由后端给出,说明文案不写死数字。
  hourlyStatsRetentionDays: number
  roundGraceSeconds: number
  // 监控列表页「最近状态」的格数,以及可设范围与默认值(范围由后端给出,页面不写死数字)。
  statusStripRounds: number
  statusStripMin: number
  statusStripMax: number
  statusStripDefault: number
  enrollmentKey: string
  notifyTemplates: Record<string, NotifyTemplate>
  notifyPlaceholders: string[]
}
interface NotifyTemplate { title: string; content: string }

const { t } = useI18n()

// 事件类型 → 当前语言的标签(与后端 notifytmpl.Events 对应)。
// 放 computed:模块级常量只求值一次,切语言后 event 按钮不会跟着变。
const eventLabels = computed<Record<string, string>>(() => ({
  DOWN: t('settings.events.DOWN'),
  UP: t('settings.events.UP'),
  TEST: t('settings.events.TEST'),
}))

// SQLite 表占用明细(字段名沿用后端 DBStats/TableSize 的 JSON 契约)。
interface TableSize {
  name: string
  count: number
  dataSize: number
  storageSize: number
  indexSize: number
  totalSize: number
}

interface DBStats {
  name: string
  collections: number
  objects: number
  dataSize: number
  storageSize: number
  indexSize: number
  totalSize: number
  // freeSize/freePages:库文件里的空闲页(已删除数据留下的空洞)= 压缩能回收的上限。
  freeSize: number
  freePages: number
  items: TableSize[]
  // dailyGrowth:按当前启用监控与其检测周期预估的每日增长(后端算,量级参考)。
  dailyGrowth?: DailyGrowth | null
}

// 每日增长预估(字段名沿用后端 store.DailyGrowth 的 JSON 契约)。
interface DailyGrowth {
  rounds: number
  results: number
  bytes: number
  enabledMonitors: number
}

// 压缩结果(字段名沿用后端 store.CompactResult 的 JSON 契约)。
interface CompactResult {
  beforeBytes: number
  afterBytes: number
  savedBytes: number
  freePages: number
  walBytes: number
  durationMs: number
}

// 压缩是同步接口(VACUUM 期间独占写连接),库大时可能远超默认的 15s 超时:
// 这里单独放宽,否则请求会被前端提前判死,而服务端其实还在压缩。
const COMPACT_TIMEOUT_MS = 600000

const settings = ref<Settings | null>(null)
const dbStats = ref<DBStats | null>(null)
const loadingDb = ref(false)
// 占用统计正在后台重算(刷新按钮按下去之后的轮询态)。
const computingDb = ref(false)
const compacting = ref(false)
const savingDays = ref(false)
// 最近状态格数单独一个 saving 标记:与保留期各自一键保存,互不阻塞。
const savingStrip = ref(false)
const rotating = ref(false)
// 五个分区各占一个标签页,避免设置/安全/模板/占用/导入挤在一屏。
const activeTab = ref<'general' | 'security' | 'templates' | 'database' | 'import'>('general')

// 通知模板编辑态(与 settings 分开,便于「取消/保存」)。
const templates = ref<Record<string, NotifyTemplate>>({})
const placeholders = ref<string[]>([])
// 预拼好 {{name}} 形式的展示 token:在模板里直接插值 {{ 会破坏 Vue 的解析。
const placeholderTokens = computed(() =>
  placeholders.value.map((p) => `{{${p}}}`),
)
const events = ref<string[]>([])
const activeEvent = ref('')
const savingTemplates = ref(false)

// 预览用示例变量,让占位符效果一目了然(示例里的监控名也是展示文案,故按语言给)。
const sampleVars = computed<Record<string, string>>(() => ({
  monitorName: t('settings.templates.samples.monitorName'),
  monitorId: '6aa3eaea721b7851dac0d93e',
  monitorType: 'HTTP',
  url: 'https://example.com/health',
  event: 'DOWN',
  successRate: '42.5',
  speed: '12.34 MB/s',
  // 节点明细是多行文本:预览里也要能看出"每个节点一行"的排版效果。
  agents: t('settings.templates.samples.agents'),
  errorCount: '3',
  // 持续时长只有恢复(UP)事件有值:预览给个示例,让 {{duration}} 的效果看得见。
  duration: t('settings.templates.samples.duration'),
  timestamp: '2026-09-12 10:00:00',
}))

// 与后端 notifytmpl.Render 同语义:替换已知占位符,未知的原样保留。
function renderTpl(text: string, ev: string): string {
  const vars: Record<string, string> = { ...sampleVars.value, event: ev }
  return text.replace(/\{\{(\w+)\}\}/g, (m, name: string) =>
    name in vars ? vars[name] : m,
  )
}

/** 人类可读字节数(保留一位小数)。 */
function fmtBytes(n: number): string {
  if (!n || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

async function load() {
  settings.value = (await http.get('/settings')) as unknown as Settings
  templates.value = JSON.parse(JSON.stringify(settings.value.notifyTemplates || {}))
  placeholders.value = settings.value.notifyPlaceholders || []
  events.value = Object.keys(templates.value)
  if (!activeEvent.value && events.value.length) activeEvent.value = events.value[0]
}

// GET /settings/db-stats 的响应:stats 是占用数据,computing 表示后台正在重算
// (刷新走异步:POST refresh 触发,GET 轮询直到 computing=false)。
interface DBStatsResponse { stats: DBStats; computing: boolean }

// 轮询间隔与上限:重算通常一两秒内完成(全库 dbstat 扫描),但赶上写排队可能更久;
// 30 次 ≈ 30 秒仍没算完就停,避免页面被无意义的请求一直打。
const DBSTATS_POLL_INTERVAL_MS = 1000
const DBSTATS_POLL_MAX = 30

let dbStatsPollTimer: ReturnType<typeof setTimeout> | null = null
let dbStatsPollTries = 0

function stopDBStatsPoll() {
  if (dbStatsPollTimer) {
    clearTimeout(dbStatsPollTimer)
    dbStatsPollTimer = null
  }
}

/** 拉一次占用快照;返回 computing(后台是否在重算)。首次/失效后 stats 为 null,只更新非空值。 */
async function fetchDBStatsOnce(): Promise<boolean> {
  const res = (await http.get('/settings/db-stats')) as unknown as DBStatsResponse
  if (res.stats) dbStats.value = res.stats
  return res.computing
}

// 点「查看占用」或再次点「刷新」:GET 永不阻塞 —— 后端立即返回(可能 stats=null),
// 重算在后台跑,前端每秒轮询到 computing=false / stats 非空为止。
// 首次点击也曾在这里同步扫全库,库大或赶上写排队时被前端 15s 超时掐死,
// 现在统计永远在后台跑,前端 15s 超时/取消的问题从根上消失。
async function loadDBStats() {
  stopDBStatsPoll()
  loadingDb.value = true
  try {
    if (await fetchDBStatsOnce()) pollDBStats()
  } catch { /* 拦截器已提示 */ } finally {
    loadingDb.value = false
  }
}

// 重算期间的轮询:每秒拉一次,computing=false 时把「计算中」按钮态收掉。
function pollDBStats() {
  stopDBStatsPoll()
  dbStatsPollTries = 0
  computingDb.value = true
  const tick = async () => {
    dbStatsPollTries++
    try {
      if (!(await fetchDBStatsOnce())) {
        computingDb.value = false
        return
      }
    } catch {
      computingDb.value = false
      return // 拉取失败(拦截器已提示):退出轮询,用户可再点刷新
    }
    if (dbStatsPollTries >= DBSTATS_POLL_MAX) {
      computingDb.value = false
      return
    }
    dbStatsPollTimer = setTimeout(tick, DBSTATS_POLL_INTERVAL_MS)
  }
  dbStatsPollTimer = setTimeout(tick, DBSTATS_POLL_INTERVAL_MS)
}

// 点「查看占用」:首次点击拉一次快照并展示;已有数据后再点等效「刷新」——
// POST 触发后台重算(立即返回),然后轮询 GET 拿新值。
// 重算不占用 HTTP 请求,前端 15s 超时/取消的问题从根上消失。
async function refreshDBStats() {
  stopDBStatsPoll()
  if (!dbStats.value) {
    await loadDBStats()
    return
  }
  try {
    await http.post('/settings/db-stats/refresh')
  } catch { /* 拦截器已提示 */ return }
  pollDBStats()
}

/** compactDB 压缩数据库(后端 VACUUM):回收空闲页、重建库文件并截断 WAL。 */
async function compactDB() {
  try {
    await confirmBox(
      t('settings.db.compactConfirm', {
        free: fmtBytes(dbStats.value?.freeSize || 0),
      }),
      t('settings.db.compactConfirmTitle'),
      { type: 'warning' },
    )
  } catch {
    return // 用户取消
  }
  compacting.value = true
  try {
    const res = (await http.post('/settings/db-compact', null, {
      timeout: COMPACT_TIMEOUT_MS,
    })) as unknown as CompactResult
    // 结果走弹框而不是一闪而过的 message:管理员要看清释放了多少(已经紧凑时也要说清楚)。
    await alertBox(
      res.savedBytes > 0
        ? t('settings.db.compactDone', {
          before: fmtBytes(res.beforeBytes),
          after: fmtBytes(res.afterBytes),
          saved: fmtBytes(res.savedBytes),
        })
        : t('settings.db.compactIdle', { size: fmtBytes(res.afterBytes) }),
      t('settings.db.compactDoneTitle'),
      { type: res.savedBytes > 0 ? 'success' : 'info' },
    )
    // 压缩作废了后端缓存:GET 会同步重算或返回 computing=true,轮询到新值为止,
    // 表格与汇总句跟着刷新到压缩后的占用。
    if (await fetchDBStatsOnce()) pollDBStats()
  } catch { /* 拦截器已提示 */ } finally {
    compacting.value = false
  }
}

// ---- UptimeKuma 导入 ----

// 预览条目:与后端 kumaItemView 的字段一一对应。
interface KumaItem {
  kumaId: string; kumaType: string; name: string
  type: string; target: string; status: string
  // port 仅 tcp 候选项有值(展示为"主机:端口")。
  port: number
  importable: boolean
  // duplicate:目标已存在或本次导入内重复,导入时会被跳过。
  duplicate: boolean
  // paused:Kuma 侧处于暂停,导入后同样保持暂停。
  paused: boolean
  warnings: string[]; reason: string
  // method/period/invertMode/group 仅账号密码方式有值(来自 UptimeKuma 的实际配置)。
  method: string; period: number; invertMode: boolean
  // group 是 Kuma 侧的分组(UptimeKuma 的分组容器),空表示未分组。
  group: string
}

interface KumaPreview {
  endpoint: string; authMode: string; total: number; importable: number
  duplicate: number; unsupported: number
  // paused 是"可导入里在 Kuma 侧处于暂停"的数量:它们会以暂停状态建出来。
  paused: number
  // groupContainers 是 UptimeKuma 里的「分组」容器数:它们不计入 total。
  groupContainers: number
  items: KumaItem[]
}

interface KumaImportResult {
  endpoint: string; total: number; created: number; skipped: number; failed: number
  // synced 是"对齐暂停状态"时被改动的已存在监控数量;updated 是"覆盖"重写的数量。
  synced: number
  updated: number
  items: { name: string; ok: boolean; message: string }[]
}

interface AgentRow { id: string; name: string; status: string; online: boolean }

const agents = ref<AgentRow[]>([])

// 导入参数:UptimeKuma 的检测间隔/阈值等无法从指标还原,由这里统一指定。
const kuma = ref({
  baseUrl: '',
  // apikey:/metrics + API 密钥,最省事但配置有损;
  // password:账号密码登录 Socket.IO 管理接口,取回全量配置。
  authMode: 'apikey' as 'apikey' | 'password',
  apiKey: '',
  username: '',
  password: '',
  twoFACode: '',
  allowInsecureTLS: false,
  group: '',
  period: 60,
  timeout: 10,
  threshold: 100,
  consecutive: 3,
  enabled: true,
  // syncPaused:已存在的同目标监控是否把启用/暂停对齐到 UptimeKuma 的当前状态。
  syncPaused: false,
  // overwrite:已存在的同目标监控是否用 Kuma 的配置整体重写(修复历史错误配置)。
  overwrite: false,
  assignMode: 'all' as 'all' | 'selected' | 'exclude',
  assignedAgentIds: [] as string[],
  excludedAgentIds: [] as string[],
})

const previewing = ref(false)
const importing = ref(false)
const kumaPreview = ref<KumaPreview | null>(null)
const kumaResult = ref<KumaImportResult | null>(null)

async function loadAgents() {
  try {
    agents.value = (await http.get('/agents')) as unknown as AgentRow[]
  } catch { /* 拦截器已提示 */ }
}

/** kumaBody 导入请求体(预览与导入共用,保证两次口径一致)。 */
function kumaBody() {
  const k = kuma.value
  return {
    baseUrl: k.baseUrl.trim(), authMode: k.authMode,
    apiKey: k.apiKey.trim(),
    username: k.username.trim(), password: k.password, twoFACode: k.twoFACode.trim(),
    allowInsecureTLS: k.allowInsecureTLS,
    group: k.group.trim(), period: k.period, timeout: k.timeout,
    threshold: k.threshold, consecutive: k.consecutive, enabled: k.enabled,
    syncPaused: k.syncPaused, overwrite: k.overwrite,
    assignMode: k.assignMode,
    assignedAgentIds: k.assignedAgentIds, excludedAgentIds: k.excludedAgentIds,
  }
}

/** fetchPreview 拉取 /metrics 并刷新预览(不改动导入结果)。 */
async function fetchPreview() {
  kumaPreview.value = (await http.post(
    '/settings/import/uptimekuma/preview', kumaBody(),
  )) as unknown as KumaPreview
}

/** resetKumaPreview 切换认证方式等参数后清空上一次的预览与结果,避免误读。 */
function resetKumaPreview() {
  kumaPreview.value = null
  kumaResult.value = null
}

async function previewKuma() {
  if (!kuma.value.baseUrl.trim()) {
    ElMessage.warning(t('settings.import.warnBaseUrl'))
    return
  }
  if (kuma.value.authMode === 'apikey' && !kuma.value.apiKey.trim()) {
    ElMessage.warning(t('settings.import.warnApiKey'))
    return
  }
  if (kuma.value.authMode === 'password' && (!kuma.value.username.trim() || !kuma.value.password)) {
    ElMessage.warning(t('settings.import.warnCredentials'))
    return
  }
  if (kuma.value.assignMode === 'selected' && !kuma.value.assignedAgentIds.length) {
    ElMessage.warning(t('settings.import.warnAssign'))
    return
  }
  previewing.value = true
  kumaResult.value = null
  try {
    await fetchPreview()
  } catch {
    kumaPreview.value = null // 拦截器已提示失败原因
  } finally {
    previewing.value = false
  }
}

async function importKuma() {
  if (!kumaPreview.value) return
  try {
    await confirmBox(
      t('settings.import.confirmImport', {
        importable: kumaPreview.value.importable,
        duplicate: kumaPreview.value.duplicate,
        unsupported: kumaPreview.value.unsupported,
      }),
      t('settings.import.confirmImportTitle'),
      { type: 'warning' },
    )
  } catch {
    return // 用户取消
  }
  importing.value = true
  try {
    kumaResult.value = (await http.post(
      '/settings/import/uptimekuma', kumaBody(),
    )) as unknown as KumaImportResult
    const r = kumaResult.value
    ElMessage.success(
      t('settings.import.importDone', { created: r.created, skipped: r.skipped })
      + (r.updated ? t('settings.import.importDoneUpdated', { count: r.updated }) : '')
      + (r.synced ? t('settings.import.importDoneSynced', { count: r.synced }) : ''),
    )
    await fetchPreview() // 新建的监控随刷新转为「已存在」状态
  } catch { /* 拦截器已提示 */ } finally {
    importing.value = false
  }
}

/** kumaOutcome 预览行的结论:可导入 / 重复目标 / 不支持。 */
function kumaOutcome(row: KumaItem): { type: 'success' | 'info' | 'warning'; text: string } {
  if (!row.importable) return { type: 'warning', text: t('settings.import.outcomeUnsupported') }
  if (row.duplicate) return { type: 'info', text: t('settings.import.outcomeDuplicate') }
  return { type: 'success', text: t('settings.import.outcomeImportable') }
}

// ---- 配置文件导入导出(UptimeMesh → UptimeMesh)----
//
// 导出本实例的可迁移配置(监控 / 通知渠道 / 通知模板 / 面板设置)成一份 JSON,
// 或把这样一份文件导进来。后端契约与版本规则见 .scratch/config-import-export/spec.md:
// 文件带 configVersion,旧文件可直接导入,高版本文件被拒绝。

interface ConfigFileMeta {
  kind: string
  configVersion: number
  exportedAt: string
  supportedVersion: number
}
interface ConfigMonitorItem {
  name: string; type: string; target: string; group: string; status: string; message: string
}
interface ConfigChannelItem {
  name: string; url: string; status: string; message: string
}
interface ConfigSettingChange {
  key: string; current: number; next: number
}
interface ConfigTemplateAction {
  event: string; action: string
}
interface ConfigPreview {
  file: ConfigFileMeta
  assignMode: string
  summary: {
    monitors: number; channels: number
    create: number; update: number; duplicate: number; invalid: number
    channelsCreate: number; channelsReuse: number; channelsInvalid: number
  }
  settings: { present: boolean; changes: ConfigSettingChange[]; templates: ConfigTemplateAction[] }
  monitors: ConfigMonitorItem[]
  channels: ConfigChannelItem[]
  warnings: string[]
}
interface ConfigImportResult {
  file: { configVersion: number; exportedAt: string }
  created: number; updated: number; skipped: number; invalid: number; failed: number
  channelsCreated: number; channelsReused: number; channelsFailed: number
  settingsApplied: string[]
  warnings: string[]
  monitors: { name: string; type?: string; target?: string; ok: boolean; message: string }[]
  channels: { name: string; ok: boolean; message: string }[]
}

// 导入是同步接口:数百个监控时可能远超默认 15s 超时,这里单独放宽(与数据库压缩同款做法)。
const CONFIG_IMPORT_TIMEOUT_MS = 300000

const exportingConfig = ref(false)
// 选中的文件与解析结果:后端要的是文件对象本身(它做严格解码与版本闸门)。
const configFile = ref<{ name: string; config: unknown } | null>(null)
const configFileInput = ref<HTMLInputElement | null>(null)
const configPreview = ref<ConfigPreview | null>(null)
const configResult = ref<ConfigImportResult | null>(null)
const previewingConfig = ref(false)
const importingConfig = ref(false)
const configImport = ref({
  overwrite: false,
  importSettings: true,
  assignMode: 'all' as 'all' | 'selected' | 'exclude',
  assignedAgentIds: [] as string[],
  excludedAgentIds: [] as string[],
})

// 面板设置项的内部字段名 → 当前语言的标签(结果提示里用)。
const settingLabels = computed<Record<string, string>>(() => ({
  resultRetentionDays: t('settings.retention.label'),
  statusStripRounds: t('settings.stripRounds.label'),
  notifyTemplates: t('settings.tabs.templates'),
}))

/** 逗号/顿号按语言给:中文用「、」,英文用「, 」。 */
function joinList(items: string[]): string {
  return items.join(t('settings.exportImport.listSeparator'))
}

function settingsKeysText(keys: string[]): string {
  return joinList(keys.map((k) => settingLabels.value[k] || k))
}

/** exportConfig 下载配置文件:后端给信封,前端拼文件名与缩进后落盘。 */
async function exportConfig() {
  exportingConfig.value = true
  try {
    const data = (await http.get('/settings/export/config')) as unknown as {
      filename: string
      counts: { monitors: number; channels: number; templates: number }
      warnings: string[]
      config: unknown
    }
    const blob = new Blob([JSON.stringify(data.config, null, 2) + '\n'], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = data.filename
    a.click()
    URL.revokeObjectURL(url)
    ElMessage.success(t('settings.exportImport.exportDone', {
      monitors: data.counts.monitors, channels: data.counts.channels, templates: data.counts.templates,
    }))
    if (data.warnings?.length) ElMessage.warning(data.warnings.join('; '))
  } catch { /* 拦截器已提示 */ } finally {
    exportingConfig.value = false
  }
}

function pickConfigFile() {
  configFileInput.value?.click()
}

/** onConfigFileChange 读文件:JSON 解析失败在本地就拦掉(后端只做结构与版本校验)。 */
async function onConfigFileChange(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = '' // 清空 value:同一个文件改完还能再次选择
  if (!file) return
  configPreview.value = null
  configResult.value = null
  let parsed: unknown
  try {
    parsed = JSON.parse(await file.text())
  } catch {
    configFile.value = null
    ElMessage.error(t('settings.exportImport.badJson'))
    return
  }
  configFile.value = { name: file.name, config: parsed }
}

/** configImportBody 预览与导入共用同一份请求体,两次口径不会漂移。 */
function configImportBody() {
  return {
    config: configFile.value?.config,
    assignMode: configImport.value.assignMode,
    assignedAgentIds: configImport.value.assignedAgentIds,
    excludedAgentIds: configImport.value.excludedAgentIds,
    overwrite: configImport.value.overwrite,
    importSettings: configImport.value.importSettings,
  }
}

function resetConfigPreview() {
  configPreview.value = null
  configResult.value = null
}

async function previewConfigImport() {
  if (!configFile.value) {
    ElMessage.warning(t('settings.exportImport.needFile'))
    return
  }
  if (configImport.value.assignMode === 'selected' && !configImport.value.assignedAgentIds.length) {
    ElMessage.warning(t('settings.import.warnAssign'))
    return
  }
  previewingConfig.value = true
  configResult.value = null
  try {
    configPreview.value = (await http.post(
      '/settings/import/config/preview', configImportBody(),
    )) as unknown as ConfigPreview
  } catch {
    configPreview.value = null // 拦截器已提示(版本过高、文件损坏等)
  } finally {
    previewingConfig.value = false
  }
}

async function importConfig() {
  if (!configFile.value || !configPreview.value) return
  const s = configPreview.value.summary
  try {
    await confirmBox(
      t('settings.exportImport.confirmImport', {
        create: s.create, update: s.update, duplicate: s.duplicate, invalid: s.invalid,
        settings: configPreview.value.settings.present
          ? t('settings.exportImport.confirmSettings')
          : t('settings.exportImport.confirmNoSettings'),
      }),
      t('settings.exportImport.confirmImportTitle'),
      { type: 'warning' },
    )
  } catch {
    return // 用户取消
  }
  importingConfig.value = true
  try {
    configResult.value = (await http.post(
      '/settings/import/config', configImportBody(), { timeout: CONFIG_IMPORT_TIMEOUT_MS },
    )) as unknown as ConfigImportResult
    const r = configResult.value
    ElMessage.success(
      t('settings.exportImport.importDone', { created: r.created, updated: r.updated, skipped: r.skipped })
      + (r.invalid ? t('settings.exportImport.importDoneInvalid', { count: r.invalid }) : '')
      + t('settings.exportImport.importDoneChannels', { created: r.channelsCreated, reused: r.channelsReused })
      + (r.settingsApplied.length
        ? t('settings.exportImport.importDoneSettings', { keys: settingsKeysText(r.settingsApplied) })
        : ''),
    )
    // 面板设置可能刚被改写:回读设置页,再刷一次预览(新建的监控转为「重复目标」)。
    await load()
    await previewConfigImport()
  } catch { /* 拦截器已提示 */ } finally {
    importingConfig.value = false
  }
}

/** configStatusLabel 预览/结果里的结论标签。 */
function configStatusLabel(status: string): string {
  switch (status) {
    case 'create': return t('settings.exportImport.statusCreate')
    case 'update': return t('settings.exportImport.statusUpdate')
    case 'duplicate': return t('settings.exportImport.statusDuplicate')
    default: return t('settings.exportImport.statusInvalid')
  }
}

function configStatusType(status: string): 'success' | 'warning' | 'info' | 'danger' {
  switch (status) {
    case 'create': return 'success'
    case 'update': return 'warning'
    case 'duplicate': return 'info'
    default: return 'danger'
  }
}

/** channelStatusLabel 渠道的结论:create/dedupe(复用)/invalid。 */
function channelStatusLabel(status: string): string {
  switch (status) {
    case 'create': return t('settings.exportImport.channelCreate')
    case 'duplicate': return t('settings.exportImport.channelReuse')
    default: return t('settings.exportImport.channelInvalid')
  }
}

/** configFileMeta 文件版本信息:导出时间可能缺(手工拼的文件)。 */
function configFileMeta(file: ConfigFileMeta): string {
  const key = file.exportedAt ? 'fileMeta' : 'fileMetaNoTime'
  return t(`settings.exportImport.${key}`, {
    version: file.configVersion, supported: file.supportedVersion, exportedAt: file.exportedAt,
  })
}

/** settingChangeText 面板设置的一行变更。 */
function settingChangeText(change: ConfigSettingChange): string {
  if (change.key === 'notifyTemplates') {
    return t('settings.exportImport.settingsTplCount', { count: change.next })
  }
  return t('settings.exportImport.settingsCurrent', {
    label: settingLabels.value[change.key] || change.key,
    current: change.current,
    next: change.next,
  })
}

onMounted(() => {
  load()
  loadAgents()
  loadAccount()
})

// 离开页面时停掉占用轮询,定时器不越过组件生命周期。
onBeforeUnmount(stopDBStatsPoll)

// ---- 安全:后台登录账号与密码 ----
// 账号是单管理员(users 表只有一行),所以这里既是"改账号"也是"改密码"。
// 保存必须带当前密码:只凭 JWT 就能改密码等于令牌泄露即账号被接管(后端同样强制校验)。
const account = ref({
  username: '',
  currentPassword: '',
  newPassword: '',
  confirmPassword: '',
})
const savingAccount = ref(false)

async function loadAccount() {
  try {
    const data = (await http.get('/settings/account')) as unknown as { username: string }
    account.value.username = data.username || ''
  } catch { /* 拦截器已提示 */ }
}

async function saveAccount() {
  const a = account.value
  if (!a.username.trim()) {
    ElMessage.warning(t('settings.security.needUsername'))
    return
  }
  if (!a.currentPassword) {
    ElMessage.warning(t('settings.security.needCurrent'))
    return
  }
  if (a.newPassword && a.newPassword !== a.confirmPassword) {
    ElMessage.warning(t('settings.security.mismatch'))
    return
  }
  savingAccount.value = true
  try {
    const data = (await http.put('/settings/account', {
      username: a.username.trim(),
      currentPassword: a.currentPassword,
      newPassword: a.newPassword,
    })) as unknown as { username: string; token: string }
    // 后端重签了令牌(subject 就是用户名):换掉本地这份,当前会话继续有效。
    if (data.token) localStorage.setItem(TOKEN_KEY, data.token)
    account.value = { username: data.username, currentPassword: '', newPassword: '', confirmPassword: '' }
    ElMessage.success(t('settings.security.saved'))
  } catch { /* 拦截器已提示(当前密码错误等) */ } finally {
    savingAccount.value = false
  }
}

async function copyKey() {
  if (!settings.value?.enrollmentKey) return
  try {
    await navigator.clipboard.writeText(settings.value.enrollmentKey)
    ElMessage.success(t('settings.key.copied'))
  } catch {
    ElMessage.warning(t('common.clipboardDenied'))
  }
}

async function saveDays() {
  if (!settings.value) return
  savingDays.value = true
  try {
    await http.put('/settings/retention', { resultRetentionDays: settings.value.resultRetentionDays })
    ElMessage.success(t('settings.retention.saved'))
  } catch { /* 拦截器已提示 */ } finally {
    savingDays.value = false
  }
}

// saveStatusStrip 保存监控列表页「最近状态」的格数。保存在后台,页面下次回到
// 监控列表页时生效(那边每次激活都会重读这个设置,见 MonitorsView.refreshStripRounds)。
async function saveStatusStrip() {
  if (!settings.value) return
  savingStrip.value = true
  try {
    await http.put('/settings/status-strip', { statusStripRounds: settings.value.statusStripRounds })
    ElMessage.success(t('settings.stripRounds.saved'))
  } catch { /* 拦截器已提示 */ } finally {
    savingStrip.value = false
  }
}

async function rotateKey() {
  await confirmBox(
    t('settings.key.rotateConfirm'),
    t('settings.key.rotateConfirmTitle'),
    { type: 'warning' },
  )
  rotating.value = true
  try {
    const data = (await http.post('/settings/rotate-key')) as unknown as { enrollmentKey: string }
    if (settings.value) settings.value.enrollmentKey = data.enrollmentKey
    await alertBox(
      t('settings.key.rotated', { key: data.enrollmentKey }),
      t('settings.key.rotatedTitle'),
      { dangerouslyUseHTMLString: true, confirmButtonText: t('settings.key.savedIt') },
    )
  } catch { /* 拦截器已提示 */ } finally {
    rotating.value = false
  }
}

// 点击变量插入到当前聚焦的输入框末尾;无法定位光标时不打断(追加即可)。
const activeField = ref<'title' | 'content'>('content')
function insertPlaceholder(name: string) {
  const tpl = templates.value[activeEvent.value]
  if (!tpl) return
  const key = activeField.value
  tpl[key] = (tpl[key] || '') + `{{${name}}}`
}

async function saveTemplates() {
  savingTemplates.value = true
  try {
    const data = (await http.put('/settings/notify-templates', {
      templates: templates.value,
    })) as unknown as { notifyTemplates: Record<string, NotifyTemplate> }
    // 回填服务端解析结果:清空的事件会带回默认模板。
    templates.value = JSON.parse(JSON.stringify(data.notifyTemplates))
    ElMessage.success(t('settings.templates.saved'))
  } catch { /* 拦截器已提示 */ } finally {
    savingTemplates.value = false
  }
}

// 恢复默认:清空当前事件后保存,服务端按默认模板回落。
async function resetTemplate() {
  const tpl = templates.value[activeEvent.value]
  if (!tpl) return
  tpl.title = ''
  tpl.content = ''
  await saveTemplates()
}
</script>

<template>
  <div v-if="settings">
    <h3>{{ t('settings.title') }}</h3>
    <el-tabs v-model="activeTab" class="settings-tabs">
      <el-tab-pane :label="t('settings.tabs.general')" name="general">
        <el-form label-width="180px" style="max-width: 560px">
          <el-form-item :label="t('settings.retention.label')">
            <div style="width:100%">
              <el-input-number v-model="settings.resultRetentionDays" :min="1" :max="365" />
              <el-button style="margin-left:12px" type="primary" plain
                :loading="savingDays" @click="saveDays">{{ t('common.save') }}</el-button>
              <p style="color:#909399;font-size:12px;margin:6px 0 0">
                {{ t('settings.retention.help', { days: settings.hourlyStatsRetentionDays }) }}
              </p>
            </div>
          </el-form-item>
          <el-form-item :label="t('settings.stripRounds.label')">
            <div style="width:100%">
              <el-input-number
                v-model="settings.statusStripRounds"
                :min="settings.statusStripMin"
                :max="settings.statusStripMax"
              />
              <el-button style="margin-left:12px" type="primary" plain
                :loading="savingStrip" @click="saveStatusStrip">{{ t('common.save') }}</el-button>
              <p style="color:#909399;font-size:12px;margin:6px 0 0">
                {{ t('settings.stripRounds.help', {
                  min: settings.statusStripMin,
                  max: settings.statusStripMax,
                  def: settings.statusStripDefault,
                }) }}
              </p>
            </div>
          </el-form-item>
          <el-form-item :label="t('settings.grace.label')">
            <span>{{ t('settings.grace.value', { seconds: settings.roundGraceSeconds }) }}</span>
          </el-form-item>
          <el-form-item :label="t('settings.key.label')">
            <div style="width:100%">
              <div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">
                <el-input
                  v-if="settings.enrollmentKey"
                  :model-value="settings.enrollmentKey"
                  readonly
                  style="max-width:360px;font-family:monospace"
                />
                <span v-else style="color:#909399">
                  {{ t('settings.key.hashOnly') }}
                </span>
                <el-button v-if="settings.enrollmentKey" @click="copyKey">{{ t('common.copy') }}</el-button>
                <el-button type="danger" plain :loading="rotating" @click="rotateKey">
                  {{ t('settings.key.rotate') }}
                </el-button>
              </div>
              <p style="color:#909399;font-size:12px;margin:6px 0 0">
                {{ t('settings.key.help') }}
              </p>
            </div>
          </el-form-item>
        </el-form>
        <p style="color:#909399;font-size:12px;max-width:560px;margin-top:8px">
          {{ t('settings.misc.help') }}
        </p>
      </el-tab-pane>

      <el-tab-pane :label="t('settings.tabs.security')" name="security">
        <el-form label-width="180px" style="max-width: 560px">
          <el-form-item :label="t('settings.security.username')" required>
            <el-input v-model="account.username" :placeholder="t('settings.security.usernamePlaceholder')" />
            <p class="field-help">{{ t('settings.security.usernameHelp') }}</p>
          </el-form-item>
          <el-form-item :label="t('settings.security.currentPassword')" required>
            <el-input
              v-model="account.currentPassword"
              type="password"
              show-password
              autocomplete="current-password"
              :placeholder="t('settings.security.currentPlaceholder')"
            />
            <p class="field-help">{{ t('settings.security.currentHelp') }}</p>
          </el-form-item>
          <el-form-item :label="t('settings.security.newPassword')">
            <el-input
              v-model="account.newPassword"
              type="password"
              show-password
              autocomplete="new-password"
              :placeholder="t('settings.security.newPlaceholder')"
            />
            <p class="field-help">{{ t('settings.security.newHelp') }}</p>
          </el-form-item>
          <el-form-item :label="t('settings.security.confirmPassword')">
            <el-input
              v-model="account.confirmPassword"
              type="password"
              show-password
              autocomplete="new-password"
              :placeholder="t('settings.security.confirmPlaceholder')"
            />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="savingAccount" @click="saveAccount">
              {{ t('settings.security.save') }}
            </el-button>
          </el-form-item>
        </el-form>
        <p class="muted-help">{{ t('settings.security.note') }}</p>
      </el-tab-pane>

      <el-tab-pane :label="t('settings.tabs.templates')" name="templates">
        <p style="color:#606266;font-size:13px;max-width:760px;margin:0 0 10px">
          {{ t('settings.templates.intro') }}
          <el-tag
            v-for="(token, i) in placeholderTokens" :key="token" size="small" class="ph-tag"
            @click="insertPlaceholder(placeholders[i])"
          >{{ token }}</el-tag>
          <span style="color:#909399;font-size:12px">{{ t('settings.templates.clickToInsert') }}</span>
        </p>
        <div v-if="events.length" style="max-width:760px">
          <el-radio-group v-model="activeEvent" size="small" style="margin-bottom:12px">
            <el-radio-button v-for="ev in events" :key="ev" :value="ev">
              {{ eventLabels[ev] || ev }}
            </el-radio-button>
          </el-radio-group>
          <div v-if="templates[activeEvent]">
            <el-form label-width="70px">
              <el-form-item :label="t('settings.templates.titleLabel')">
                <el-input
                  v-model="templates[activeEvent].title"
                  @focus="activeField = 'title'"
                  :placeholder="t('settings.templates.titlePlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('settings.templates.bodyLabel')">
                <el-input
                  v-model="templates[activeEvent].content"
                  type="textarea" :rows="5"
                  @focus="activeField = 'content'"
                  :placeholder="t('settings.templates.bodyPlaceholder')"
                />
              </el-form-item>
            </el-form>
            <div class="preview">
              <div class="preview-hd">{{ t('settings.templates.preview') }}</div>
              <div class="preview-title">
                {{ renderTpl(templates[activeEvent].title, activeEvent) || t('settings.templates.noTitle') }}
              </div>
              <pre class="preview-body">{{ renderTpl(templates[activeEvent].content, activeEvent) }}</pre>
            </div>
          </div>
          <div style="margin-top:4px">
            <el-button type="primary" :loading="savingTemplates" @click="saveTemplates">{{ t('settings.templates.save') }}</el-button>
            <el-button plain :disabled="savingTemplates" @click="resetTemplate">{{ t('settings.templates.reset') }}</el-button>
            <span class="tip-inline">{{ t('settings.templates.resetTip') }}</span>
          </div>
        </div>
      </el-tab-pane>

      <el-tab-pane :label="t('settings.tabs.database')" name="database">
        <!-- 按需加载:进页面不请求,点「查看占用」才拉;未加载前显示引导占位 -->
        <div v-if="!dbStats" v-loading="loadingDb || computingDb"
             style="padding:24px 0;color:#909399;font-size:13px;max-width:760px">
          <p style="margin:0 0 12px">
            {{ computingDb ? t('settings.db.computing') : t('settings.db.notLoaded') }}
          </p>
          <el-button v-if="!computingDb" type="primary" size="small" :loading="loadingDb"
                     @click="refreshDBStats">
            {{ t('settings.db.view') }}
          </el-button>
        </div>
        <template v-else>
        <!-- 汇总句里有加粗的库文件大小,故走 i18n-t 的具名插槽 -->
        <i18n-t
          keypath="settings.db.summary"
          tag="p"
          scope="global"
          style="color:#606266;font-size:13px;margin:0 0 10px"
        >
          <template #tables>{{ dbStats.collections }}</template>
          <template #rows>{{ dbStats.objects.toLocaleString() }}</template>
          <template #size><b>{{ fmtBytes(dbStats.totalSize) }}</b></template>
        </i18n-t>
        <el-table
          v-loading="loadingDb" :data="dbStats.items"
          size="small" :empty-text="t('settings.db.empty')" style="max-width:760px"
        >
          <el-table-column prop="name" :label="t('settings.db.columnTable')" min-width="140" />
          <el-table-column :label="t('settings.db.columnRows')" width="110" align="right">
            <template #default="{ row }">{{ row.count.toLocaleString() }}</template>
          </el-table-column>
          <el-table-column :label="t('settings.db.columnSize')" width="120" align="right">
            <template #default="{ row }">{{ fmtBytes(row.dataSize) }}</template>
          </el-table-column>
        </el-table>
        <!-- 灰字提示:沿用本文件其它标签页的行内写法,不依赖样式类 -->
        <p v-if="dbStats.freeSize > 0" style="color:#909399;font-size:12px;margin:6px 0 0;line-height:1.6;max-width:760px">
          {{ t('settings.db.freeHint', { size: fmtBytes(dbStats.freeSize) }) }}
        </p>
        <!-- 每日增长预估:按当前启用监控与检测频率由后端折算,随占用一起刷新 -->
        <p
          v-if="dbStats.dailyGrowth && dbStats.dailyGrowth.enabledMonitors > 0"
          style="color:#909399;font-size:12px;margin:6px 0 0;line-height:1.6;max-width:760px"
        >
          <i18n-t keypath="settings.db.dailyGrowth" tag="span" scope="global">
            <template #monitors>{{ dbStats.dailyGrowth.enabledMonitors }}</template>
            <template #rounds>{{ dbStats.dailyGrowth.rounds.toLocaleString() }}</template>
            <template #results>{{ dbStats.dailyGrowth.results.toLocaleString() }}</template>
            <template #size><b>{{ fmtBytes(dbStats.dailyGrowth.bytes) }}</b></template>
          </i18n-t>
        </p>
        <div style="margin-top:10px">
          <!-- 首次之后即「刷新」:POST 触发后台重算后轮询,按钮转圈直到 computing=false -->
          <el-button size="small" :loading="loadingDb || computingDb" @click="refreshDBStats">
            {{ t('settings.db.refresh') }}
          </el-button>
          <el-button type="warning" plain size="small" :loading="compacting" @click="compactDB">
            {{ t('settings.db.compact') }}
          </el-button>
        </div>
        <p style="color:#909399;font-size:12px;margin:6px 0 0;line-height:1.6;max-width:760px">
          {{ t('settings.db.compactTip') }}
        </p>
        </template>
      </el-tab-pane>

      <el-tab-pane :label="t('settings.tabs.import')" name="import">
        <p class="import-intro">
          {{ t('settings.import.intro') }}
        </p>
        <ul class="import-intro" style="margin-top:-6px">
          <li>
            <i18n-t keypath="settings.import.itemApiKey" scope="global">
              <template #apiKey><b>{{ t('settings.import.authApikey') }}</b></template>
              <template #metrics><code>/metrics</code></template>
            </i18n-t>
          </li>
          <li>
            <i18n-t keypath="settings.import.itemPassword" scope="global">
              <template #password><b>{{ t('settings.import.authPassword') }}</b></template>
              <template #versions><b>{{ t('settings.import.versionsSupported') }}</b></template>
              <template #socketIo><code>/socket.io/*</code></template>
              <template #apiAuth><code>/api/auth/*</code></template>
            </i18n-t>
          </li>
          <li>
            <i18n-t keypath="settings.import.itemTypes" scope="global">
              <template #types><b>{{ t('settings.import.typesSupported') }}</b></template>
              <template #push><b>{{ t('type.push') }}</b></template>
            </i18n-t>
          </li>
        </ul>
        <el-form label-width="160px" style="max-width: 720px">
          <el-form-item :label="t('settings.import.baseUrl')" required>
            <el-input v-model="kuma.baseUrl" placeholder="http://192.168.1.10:3001" />
          </el-form-item>
          <el-form-item :label="t('settings.import.authMode')" required>
            <el-radio-group v-model="kuma.authMode" @change="resetKumaPreview">
              <el-radio-button value="apikey">{{ t('settings.import.authApikey') }}</el-radio-button>
              <el-radio-button value="password">{{ t('settings.import.authPassword') }}</el-radio-button>
            </el-radio-group>
            <span
              v-if="kuma.authMode === 'apikey'"
              class="tip-inline" style="display:block; margin:6px 0 0"
            >
              {{ t('settings.import.apikeyTip') }}
            </span>
          </el-form-item>
          <el-form-item v-if="kuma.authMode === 'apikey'" :label="t('settings.import.apiKey')" required>
            <el-input
              v-model="kuma.apiKey" type="password" show-password
              :placeholder="t('settings.import.apiKeyPlaceholder')"
            />
          </el-form-item>
          <template v-else>
            <el-form-item :label="t('settings.import.username')" required>
              <el-input v-model="kuma.username" :placeholder="t('settings.import.usernamePlaceholder')" />
            </el-form-item>
            <el-form-item :label="t('settings.import.password')" required>
              <el-input
                v-model="kuma.password" type="password" show-password
                :placeholder="t('settings.import.passwordPlaceholder')"
              />
            </el-form-item>
            <el-form-item :label="t('settings.import.twoFA')">
              <el-input v-model="kuma.twoFACode" style="width:160px" :placeholder="t('settings.import.twoFAPlaceholder')" />
              <span class="tip-inline">{{ t('settings.import.twoFATip') }}</span>
            </el-form-item>
          </template>
          <el-form-item :label="t('settings.import.tls')">
            <el-checkbox v-model="kuma.allowInsecureTLS">{{ t('settings.import.insecureTls') }}</el-checkbox>
          </el-form-item>
          <el-form-item :label="t('settings.import.group')">
            <el-input v-model="kuma.group" style="width:260px" :placeholder="t('settings.import.groupPlaceholder')" />
            <span class="tip-inline">
              {{ t('settings.import.groupTip') }}
            </span>
          </el-form-item>
          <el-form-item :label="t('settings.import.periodTimeout')">
            <el-input-number v-model="kuma.period" :min="10" :max="3600" />
            <span class="sep">/</span>
            <el-input-number v-model="kuma.timeout" :min="1" :max="3600" />
            <span class="tip-inline">
              {{ kuma.authMode === 'password'
                ? t('settings.import.periodTipPassword')
                : t('settings.import.periodTipApiKey') }}
            </span>
          </el-form-item>
          <el-form-item :label="t('settings.import.thresholdConsecutive')">
            <el-input-number v-model="kuma.threshold" :min="0" :max="100" />
            <span class="sep">% ×</span>
            <el-input-number v-model="kuma.consecutive" :min="1" :max="20" />
            <span class="tip-inline">{{ t('settings.import.roundsUnit') }}</span>
          </el-form-item>
          <el-form-item :label="t('monitorForm.assign')" required>
            <el-radio-group v-model="kuma.assignMode">
              <el-radio value="all">{{ t('monitorForm.assignAll') }}</el-radio>
              <el-radio value="selected">{{ t('monitorForm.assignSelected') }}</el-radio>
              <el-radio value="exclude">{{ t('monitorForm.assignExclude') }}</el-radio>
            </el-radio-group>
            <template v-if="kuma.assignMode === 'exclude'">
              <el-select v-model="kuma.excludedAgentIds" multiple
                style="width:100%; margin-top:8px" :placeholder="t('monitorForm.excludePlaceholder')">
                <el-option v-for="a in agents" :key="a.id" :value="a.id"
                  :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')" :disabled="a.status !== 'approved'" />
              </el-select>
            </template>
            <template v-else-if="kuma.assignMode === 'selected'">
              <el-select v-model="kuma.assignedAgentIds" multiple
                style="width:100%; margin-top:8px" :placeholder="t('monitorForm.assignedPlaceholder')">
                <el-option v-for="a in agents" :key="a.id" :value="a.id"
                  :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')" :disabled="a.status !== 'approved'" />
              </el-select>
            </template>
            <div v-else class="tip-inline" style="display:block">
              {{ t('settings.import.assignAllTip') }}
            </div>
          </el-form-item>
          <el-form-item :label="t('settings.import.afterImport')">
            <el-checkbox v-model="kuma.enabled">{{ t('settings.import.enableNow') }}</el-checkbox>
            <span class="tip-inline">{{ t('settings.import.enableNowTip') }}</span>
          </el-form-item>
          <el-form-item :label="t('settings.import.existing')">
            <div style="display:block">
              <el-checkbox v-model="kuma.syncPaused">{{ t('settings.import.syncPaused') }}</el-checkbox>
              <div class="tip-inline" style="display:block; margin-left:0">
                {{ t('settings.import.syncPausedTip') }}
              </div>
              <el-checkbox v-model="kuma.overwrite" style="margin-top:4px">
                {{ t('settings.import.overwrite') }}
              </el-checkbox>
              <div class="tip-inline" style="display:block; margin-left:0">
                {{ t('settings.import.overwriteTip') }}
              </div>
            </div>
          </el-form-item>
        </el-form>

        <div class="import-actions">
          <el-button :loading="previewing" @click="previewKuma">{{ t('settings.import.preview') }}</el-button>
          <el-button
            type="primary" :loading="importing"
            :disabled="!kumaPreview || !kumaPreview.importable"
            @click="importKuma"
          >{{ t('settings.import.doImport') }}</el-button>
          <span v-if="kumaPreview" class="tip-inline">
            {{ t('settings.import.importableCount', { count: kumaPreview.importable }) }}
          </span>
        </div>

        <template v-if="kumaPreview">
          <el-alert
            type="info" show-icon :closable="false" class="import-summary"
            :title="t('settings.import.summary', {
              endpoint: kumaPreview.endpoint,
              total: kumaPreview.total,
              importable: kumaPreview.importable,
              duplicate: kumaPreview.duplicate,
              unsupported: kumaPreview.unsupported,
            })
              + (kumaPreview.groupContainers > 0
                ? t('settings.import.summaryGroups', { count: kumaPreview.groupContainers }) : '')
              + (kumaPreview.paused > 0
                ? t('settings.import.summaryPaused', { count: kumaPreview.paused }) : '')"
          />
          <el-table :data="kumaPreview.items" size="small" max-height="420" :empty-text="t('settings.import.previewEmpty')">
            <el-table-column prop="name" :label="t('monitors.columnName')" min-width="150" show-overflow-tooltip>
              <template #default="{ row }">
                <el-tag v-if="row.invertMode" size="small" type="warning" style="margin-right:6px">{{ t('monitors.invertTag') }}</el-tag>
                <el-tag v-if="row.paused" size="small" type="info" style="margin-right:6px">{{ t('settings.import.tagPaused') }}</el-tag>
                {{ row.name }}
              </template>
            </el-table-column>
            <el-table-column prop="kumaType" :label="t('settings.import.columnKumaType')" width="130" />
            <el-table-column :label="t('settings.import.columnMappedTo')" width="90">
              <template #default="{ row }">
                <span v-if="row.importable">{{ row.type.toUpperCase() }}</span>
                <span v-else style="color:#909399">—</span>
              </template>
            </el-table-column>
            <el-table-column :label="t('settings.import.columnMethod')" width="70">
              <template #default="{ row }">{{ row.method || '—' }}</template>
            </el-table-column>
            <el-table-column :label="t('monitors.columnPeriod')" width="80">
              <template #default="{ row }">
                {{ row.period ? row.period + 's' : t('settings.import.byParam') }}
              </template>
            </el-table-column>
            <el-table-column :label="t('settings.import.columnMappedGroup')" width="130" show-overflow-tooltip>
              <template #default="{ row }">
                <span v-if="row.group">{{ row.group }}</span>
                <span v-else style="color:#909399">{{ t('settings.import.byImportParam') }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="t('monitors.columnTarget')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span v-if="row.type === 'tcp' && row.target">{{ row.target }}:{{ row.port }}</span>
                <span v-else-if="row.target">{{ row.target }}</span>
                <span v-else-if="row.type === 'push'" style="color:#909399">{{ t('settings.import.pushNoTarget') }}</span>
                <span v-else style="color:#909399">—</span>
              </template>
            </el-table-column>
            <el-table-column prop="status" :label="t('settings.import.columnKumaStatus')" width="80" />
            <el-table-column :label="t('settings.import.columnOutcome')" width="90">
              <template #default="{ row }">
                <el-tag size="small" :type="kumaOutcome(row).type">{{ kumaOutcome(row).text }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column :label="t('settings.import.columnReason')" min-width="200">
              <template #default="{ row }">
                <span v-if="row.reason" style="color:#e6a23c">{{ row.reason }}</span>
                <span v-else-if="row.warnings.length" style="color:#909399">{{ row.warnings.join(';') }}</span>
                <span v-else style="color:#909399">—</span>
              </template>
            </el-table-column>
          </el-table>
        </template>

        <template v-if="kumaResult">
          <h4 style="margin:18px 0 8px">{{ t('settings.import.resultTitle') }}</h4>
          <el-alert
            :type="kumaResult.failed > 0 ? 'warning' : 'success'" show-icon :closable="false"
            class="import-summary"
            :title="t('settings.import.resultSummary', {
              created: kumaResult.created,
              skipped: kumaResult.skipped,
              failed: kumaResult.failed,
            })"
          />
          <el-table :data="kumaResult.items" size="small" max-height="320">
            <el-table-column prop="name" :label="t('monitors.columnName')" min-width="150" show-overflow-tooltip />
            <el-table-column :label="t('settings.import.columnResult')" width="90">
              <template #default="{ row }">
                <el-tag size="small" :type="row.ok ? 'success' : 'info'">
                  {{ row.ok ? t('settings.import.created') : t('settings.import.notCreated') }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="message" :label="t('settings.import.columnReason')" min-width="240" show-overflow-tooltip />
          </el-table>
        </template>
      </el-tab-pane>

      <el-tab-pane :label="t('settings.tabs.config')" name="config">
        <p class="import-intro">{{ t('settings.exportImport.intro') }}</p>
        <ul class="import-intro" style="margin-top:-6px">
          <li>{{ t('settings.exportImport.itemScope') }}</li>
          <li>{{ t('settings.exportImport.itemAssign') }}</li>
          <li>{{ t('settings.exportImport.itemVersion') }}</li>
        </ul>

        <h4 class="cfg-hd">{{ t('settings.exportImport.exportTitle') }}</h4>
        <div class="cfg-actions">
          <el-button type="primary" plain :loading="exportingConfig" @click="exportConfig">
            {{ t('settings.exportImport.exportButton') }}
          </el-button>
          <span class="tip-inline">{{ t('settings.exportImport.exportHint') }}</span>
        </div>

        <h4 class="cfg-hd">{{ t('settings.exportImport.importTitle') }}</h4>
        <el-form label-width="160px" style="max-width: 720px">
          <el-form-item :label="t('settings.exportImport.pickFile')" required>
            <div style="width:100%">
              <!-- 隐藏的原生 file input:Element 的上传组件会把文件 POST 到服务端,
                   而这里只需要在浏览器里读出文本再自己提交。 -->
              <input
                ref="configFileInput" type="file" accept=".json,application/json"
                style="display:none" @change="onConfigFileChange"
              >
              <el-button @click="pickConfigFile">{{ t('settings.exportImport.pickFile') }}</el-button>
              <span v-if="configFile" class="tip-inline">
                {{ t('settings.exportImport.fileChosen', { name: configFile.name }) }}
              </span>
              <div v-if="configPreview" class="tip-inline" style="display:block; margin:6px 0 0">
                {{ configFileMeta(configPreview.file) }}
              </div>
            </div>
          </el-form-item>
          <el-form-item :label="t('monitorForm.assign')" required>
            <el-radio-group v-model="configImport.assignMode" @change="resetConfigPreview">
              <el-radio value="all">{{ t('monitorForm.assignAll') }}</el-radio>
              <el-radio value="selected">{{ t('monitorForm.assignSelected') }}</el-radio>
              <el-radio value="exclude">{{ t('monitorForm.assignExclude') }}</el-radio>
            </el-radio-group>
            <template v-if="configImport.assignMode === 'exclude'">
              <el-select
                v-model="configImport.excludedAgentIds" multiple
                style="width:100%; margin-top:8px" :placeholder="t('monitorForm.excludePlaceholder')"
              >
                <el-option
                  v-for="a in agents" :key="a.id" :value="a.id"
                  :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')"
                  :disabled="a.status !== 'approved'"
                />
              </el-select>
            </template>
            <template v-else-if="configImport.assignMode === 'selected'">
              <el-select
                v-model="configImport.assignedAgentIds" multiple
                style="width:100%; margin-top:8px" :placeholder="t('monitorForm.assignedPlaceholder')"
              >
                <el-option
                  v-for="a in agents" :key="a.id" :value="a.id"
                  :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')"
                  :disabled="a.status !== 'approved'"
                />
              </el-select>
            </template>
            <div v-else class="tip-inline" style="display:block">
              {{ t('monitorForm.assignAllTip') }}
            </div>
            <div class="tip-inline" style="display:block">{{ t('settings.exportImport.assignTip') }}</div>
          </el-form-item>
          <el-form-item :label="t('settings.exportImport.importTitle')">
            <div style="display:block">
              <el-checkbox v-model="configImport.overwrite" @change="resetConfigPreview">
                {{ t('settings.exportImport.overwrite') }}
              </el-checkbox>
              <div class="tip-inline" style="display:block; margin-left:0">
                {{ t('settings.exportImport.overwriteTip') }}
              </div>
              <el-checkbox v-model="configImport.importSettings" style="margin-top:4px" @change="resetConfigPreview">
                {{ t('settings.exportImport.importSettings') }}
              </el-checkbox>
              <div class="tip-inline" style="display:block; margin-left:0">
                {{ t('settings.exportImport.importSettingsTip') }}
              </div>
            </div>
          </el-form-item>
        </el-form>

        <div class="cfg-actions">
          <el-button :loading="previewingConfig" @click="previewConfigImport">
            {{ t('settings.exportImport.preview') }}
          </el-button>
          <el-button
            type="primary" :loading="importingConfig"
            :disabled="!configPreview || (!configPreview.summary.create && !configPreview.summary.update)"
            @click="importConfig"
          >{{ t('settings.exportImport.doImport') }}</el-button>
        </div>

        <template v-if="configPreview">
          <el-alert
            type="info" show-icon :closable="false" class="import-summary"
            :title="t('settings.exportImport.previewSummary', {
              monitors: configPreview.summary.monitors,
              create: configPreview.summary.create,
              update: configPreview.summary.update,
              duplicate: configPreview.summary.duplicate,
              invalid: configPreview.summary.invalid,
            })
              + t('settings.exportImport.previewChannels', {
                create: configPreview.summary.channelsCreate,
                reuse: configPreview.summary.channelsReuse,
                invalid: configPreview.summary.channelsInvalid,
              })
              + (configPreview.settings.present
                ? t('settings.exportImport.previewSettings', {
                  keys: settingsKeysText(configPreview.settings.changes.map((c) => c.key)),
                })
                : t('settings.exportImport.previewNoSettings'))"
          />
          <el-alert
            v-if="configPreview.warnings.length"
            type="warning" show-icon :closable="false" class="import-summary"
            :title="t('settings.exportImport.warningsTitle')"
            :description="configPreview.warnings.join('\n')"
          />

          <!-- 面板设置差异:保留期/格数给「当前 → 文件」,模板给逐事件结论 -->
          <div v-if="configPreview.settings.present" class="cfg-settings">
            <div class="preview-hd">{{ t('settings.exportImport.settingsChanges') }}</div>
            <div v-for="c in configPreview.settings.changes" :key="c.key" class="cfg-setting-row">
              {{ settingChangeText(c) }}
            </div>
            <div v-if="configPreview.settings.templates.length" class="cfg-setting-row">
              <span>{{ t('settings.exportImport.settingsTemplates') }}:</span>
              <span
                v-for="a in configPreview.settings.templates" :key="a.event"
                class="tip-inline" style="margin-left:8px"
              >
                {{ a.action === 'overwrite'
                  ? t('settings.exportImport.settingsTplOverwrite', { event: a.event })
                  : t('settings.exportImport.settingsTplReset', { event: a.event }) }}
              </span>
            </div>
          </div>

          <el-table
            v-if="configPreview.monitors.length" :data="configPreview.monitors"
            size="small" max-height="420" :empty-text="t('settings.exportImport.previewEmpty')"
          >
            <el-table-column prop="name" :label="t('monitors.columnName')" min-width="150" show-overflow-tooltip />
            <el-table-column :label="t('settings.import.columnMappedTo')" width="90">
              <template #default="{ row }"><span>{{ (row.type || '').toUpperCase() }}</span></template>
            </el-table-column>
            <el-table-column :label="t('monitors.columnTarget')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span v-if="row.target">{{ row.target }}</span>
                <span v-else style="color:#909399">—</span>
              </template>
            </el-table-column>
            <el-table-column prop="group" :label="t('settings.import.columnMappedGroup')" width="120" show-overflow-tooltip />
            <el-table-column :label="t('settings.exportImport.columnStatus')" width="100">
              <template #default="{ row }">
                <el-tag size="small" :type="configStatusType(row.status)">
                  {{ configStatusLabel(row.status) }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="message" :label="t('settings.import.columnReason')" min-width="220" show-overflow-tooltip />
          </el-table>

          <el-table
            v-if="configPreview.channels.length" :data="configPreview.channels"
            size="small" max-height="260" style="margin-top:10px"
          >
            <el-table-column
              prop="name" :label="t('settings.exportImport.channelTitle')"
              min-width="150" show-overflow-tooltip
            />
            <el-table-column prop="url" label="URL" min-width="200" show-overflow-tooltip />
            <el-table-column :label="t('settings.exportImport.columnStatus')" width="120">
              <template #default="{ row }">
                <el-tag size="small" :type="configStatusType(row.status)">
                  {{ channelStatusLabel(row.status) }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="message" :label="t('settings.import.columnReason')" min-width="200" show-overflow-tooltip />
          </el-table>
        </template>

        <template v-if="configResult">
          <h4 class="cfg-hd">{{ t('settings.exportImport.resultTitle') }}</h4>
          <el-alert
            :type="configResult.failed || configResult.invalid ? 'warning' : 'success'"
            show-icon :closable="false" class="import-summary"
            :title="t('settings.exportImport.resultSummary', {
              created: configResult.created, updated: configResult.updated,
              skipped: configResult.skipped, invalid: configResult.invalid, failed: configResult.failed,
            })
              + (configResult.settingsApplied.length
                ? t('settings.exportImport.importDoneSettings', {
                  keys: settingsKeysText(configResult.settingsApplied),
                })
                : '')"
          />
          <el-alert
            v-if="configResult.warnings.length"
            type="warning" show-icon :closable="false" class="import-summary"
            :title="t('settings.exportImport.warningsTitle')"
            :description="configResult.warnings.join('\n')"
          />
          <el-table :data="configResult.monitors" size="small" max-height="320">
            <el-table-column prop="name" :label="t('monitors.columnName')" min-width="150" show-overflow-tooltip />
            <el-table-column :label="t('settings.import.columnResult')" width="100">
              <template #default="{ row }">
                <el-tag size="small" :type="row.ok ? 'success' : 'info'">
                  {{ row.ok ? t('settings.exportImport.resultOk') : t('settings.exportImport.resultNotOk') }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="message" :label="t('settings.import.columnReason')" min-width="240" show-overflow-tooltip />
          </el-table>
        </template>
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<style scoped>
.settings-tabs { max-width: 880px; }
/* 安全标签页里的说明文字:与其它标签页的 tip 同款灰字。 */
.field-help { color: #909399; font-size: 12px; margin: 6px 0 0; line-height: 1.6; }
.muted-help { color: #909399; font-size: 12px; max-width: 560px; margin-top: 8px; line-height: 1.7; }
.ph-tag {
  margin: 0 4px 4px 0;
  cursor: pointer;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.preview {
  background: #f7f8fa;
  border: 1px solid #ebeef5;
  border-radius: 4px;
  padding: 10px 12px;
  margin: 0 0 12px;
}
.preview-hd { color: #909399; font-size: 12px; margin-bottom: 6px; }
.preview-title { font-weight: 600; margin-bottom: 6px; }
.preview-body {
  margin: 0;
  font-family: inherit;
  font-size: 13px;
  color: #606266;
  white-space: pre-wrap;
  word-break: break-all;
}
.tip-inline { color: #909399; font-size: 12px; margin-left: 10px; }
.import-intro {
  color: #606266;
  font-size: 13px;
  max-width: 760px;
  margin: 0 0 12px;
  line-height: 1.7;
}
.import-intro code {
  background: #f5f7fa;
  border-radius: 3px;
  padding: 1px 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.import-actions { display: flex; align-items: center; margin-bottom: 12px; }
.import-summary { max-width: 900px; margin-bottom: 10px; }
.sep { margin: 0 6px; color: #606266; }
/* 配置导入导出:区块标题与操作行,与上面的导入标签页同款留白。 */
.cfg-hd { margin: 18px 0 8px; }
.cfg-actions { display: flex; align-items: center; margin-bottom: 12px; }
.cfg-settings {
  background: #f7f8fa;
  border: 1px solid #ebeef5;
  border-radius: 4px;
  padding: 8px 12px;
  margin: 0 0 10px;
  max-width: 900px;
}
.cfg-setting-row { color: #606266; font-size: 13px; line-height: 1.9; }
</style>
