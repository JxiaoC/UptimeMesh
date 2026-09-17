<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { http } from '../api/http'
import { connected, onRealtime } from '../api/realtime'
import type { AgentLatencyPoint, Round, StateChange, StatsBucket, TimelineRow } from '../api/types'
import { toKbps, fromKbps } from '../utils/speed'
import TrendChart from './TrendChart.vue'
import RoundTimeTable from './RoundTimeTable.vue'

/**
 * MonitorDetailPanel 是监控详情的**内容本体**:趋势图 + 区间汇总 + 轮次时间线 + 状态变动记录。
 *
 * 为什么拆成组件:详情内容现在有两个出口 ——
 *   1. 列表页点监控名 / 总览页点变动记录(或卡片)弹出的**弹窗**(MonitorDetailDialog);
 *   2. 直接访问 /monitors/:id(旧书签、外链)时的独立页(MonitorDetailView)。
 * 两个出口共用这一份实现,就不存在"改了一处忘了另一处"。
 *
 * 面板只管内容:标题、状态标签与「返回」按钮由外层壳子负责 —— 弹窗用 #header 插槽,
 * 独立页用 el-page-header。monitor 通过 loaded 事件交给外壳去渲染标题与标签。
 *
 * 底部那两块表(轮次时间线 / 最近状态变动记录)在**弹窗与独立页里都是并排的两栏**,
 * 各占一半宽度:弹窗宽度是屏幕的 90%(见 MonitorDetailDialog),半幅 ≈ 45vw,
 * 在常见桌面宽度下够放「变动记录」表(实测这张表要 ~770px),窄屏下由表格自身横向滚动兜底。
 * layout 只用来区分「弹窗」这个更窄的容器,给正文补 gutter 那 16px(见下方样式说明)。
 */
const props = defineProps<{
  monitorId: string
  /**
   * 容器口径:弹窗('dialog')还是独立页('page')。
   *
   * 只影响正文根节点的左右内边距 —— el-row 的 gutter 会给行加 -8px 左右外边距,
   * 行因此比内容盒宽 16px;弹窗里父级不够宽,不补就会凭空多一条横向滚动条
   * (实测正文 1208 / scrollWidth 1216)。半幅宽度的取舍见上方注释。
   */
  layout?: 'page' | 'dialog'
}>()
const emit = defineEmits<{ loaded: [monitor: unknown] }>()

// 是否弹窗容器:决定正文根节点要不要补 gutter 的 16px(见 props 注释)。
const inDialog = computed(() => props.layout === 'dialog')

const { t } = useI18n()

const DAY_SEC = 86400
// 结束时间距「现在」在此范围内 ⇒ 视为实时窗口,每次刷新滚动到当前时刻。
const LIVE_SLACK_MS = 60_000
const monitor = ref<any>(null)
const rounds = ref<Round[]>([])
// 状态变动记录:只在告警状态翻转(UPDOWN)时后端才写一条,展示内容与轮次时间线同构。
const stateChanges = ref<StateChange[]>([])
const agents = ref<Record<string, { name: string; region: string }>>({})
const loading = ref(true)
// 时间段与颗粒度:默认最近 24 小时 + 5 分钟。
const range = ref<[Date, Date]>([new Date(Date.now() - DAY_SEC * 1000), new Date()])
const bucketSec = ref(300)
// 实时窗口:结束时间贴近「现在」时为 true,刷新时按原跨度滚动到当前时刻。
const liveRange = ref(true)
const stats = ref<StatsBucket[]>([])
// 分节点延时:原始结果按「节点 + 所选颗粒度桶」聚合(与 stats 同区间、同桶边界)。
const agentLatency = ref<AgentLatencyPoint[]>([])
const summary = ref<{ rate: number | null; latency: number | null; speed: number | null }>({
  rate: null, latency: null, speed: null,
})
let timer: number | undefined
let offs: (() => void)[] = []

// 下载速度监控:整个页面的判定口径从「成功率」换成「速度」——
// 摘要、区间汇总、趋势曲线与轮次时间线都按 monitor.speedUnit 展示。
const isDownload = computed(() => monitor.value?.type === 'download')
// 外部上报(push)的摘要口径(没有可用率阈值,只说连续失败轮数)在 MonitorDetailTags 里,
// 这里不再需要 isPush —— 面板正文只关心"下载速度"这一种口径差异。
const speedUnit = computed<string>(() => monitor.value?.speedUnit || 'KB/s')

// 趋势图上的阈值虚线:取监控配置里的阈值,单位与主曲线一致 ——
// 可用率监控是 %(阈值本身就是 0~100),下载速度监控是监控配置的单位
// (阈值存储单位即 SpeedUnit,主曲线的刻度也按它换算)。所以这里原样传,不做换算。
//
// push 监控不传:它的阈值是后端恒定的 PushThreshold,表单里也没有这一项,
// 每轮成功率非 0% 即 100%,画一条 100% 的线既无信息量也贴着网格顶边。传 null 不画。
const chartThreshold = computed<number | null>(() => {
  const m = monitor.value
  if (!m || m.type === 'push') return null
  const v = Number(m.threshold)
  return Number.isFinite(v) ? v : null
})

// 颗粒度下限:跨度 ≤24 小时 ⇒ 1 分钟;24 小时~3 天 ⇒ 30 分钟;更长 ⇒ 1 小时。
function minBucketOf(span: number): number {
  if (span <= DAY_SEC) return 60
  if (span <= 3 * DAY_SEC) return 1800
  return 3600
}
const spanSec = computed(() =>
  Math.max(0, Math.floor((range.value[1].getTime() - range.value[0].getTime()) / 1000)),
)
const minBucket = computed(() => minBucketOf(spanSec.value))

// 可选颗粒度(秒)与快捷时段:都放 computed,切语言后下拉与快捷项跟着变。
const granularities = computed(() => [
  { value: 60, label: t('monitorDetail.granularity1m') },
  { value: 300, label: t('monitorDetail.granularity5m') },
  { value: 1800, label: t('monitorDetail.granularity30m') },
  { value: 3600, label: t('monitorDetail.granularity1h') },
  { value: 86400, label: t('monitorDetail.granularity1d') },
])
// 快捷时段:结束时间都是「现在」,因此查询按实时窗口滚动。
const rangeShortcuts = computed(() => [
  { text: t('monitorDetail.shortcuts.hour1'), value: () => [new Date(Date.now() - 3600 * 1000), new Date()] },
  { text: t('monitorDetail.shortcuts.day1'), value: () => [new Date(Date.now() - DAY_SEC * 1000), new Date()] },
  { text: t('monitorDetail.shortcuts.day3'), value: () => [new Date(Date.now() - 3 * DAY_SEC * 1000), new Date()] },
  { text: t('monitorDetail.shortcuts.day7'), value: () => [new Date(Date.now() - 7 * DAY_SEC * 1000), new Date()] },
  { text: t('monitorDetail.shortcuts.day30'), value: () => [new Date(Date.now() - 30 * DAY_SEC * 1000), new Date()] },
])

async function load() {
  const mid = props.monitorId
  try {
    monitor.value = await http.get(`/monitors/${mid}`)
    // 轮次时间线与状态变动记录一起拉:两者是详情页底部并排的两块,分开拉会闪。
    const [rs, scs] = await Promise.all([
      http.get(`/monitors/${mid}/rounds?limit=50`),
      http.get(`/monitors/${mid}/state-changes?limit=20`),
    ])
    rounds.value = rs as unknown as Round[]
    stateChanges.value = scs as unknown as StateChange[]
    // includeDeleted:已删除节点的历史轮次仍回显名称
    const list = (await http.get('/agents?includeDeleted=1')) as unknown as { id: string; name: string; region: string }[]
    agents.value = Object.fromEntries(list.map((a) => [a.id, { name: a.name, region: a.region || '' }]))
    await loadStats()
    // 交给外壳渲染标题与状态标签(弹窗的 #header / 独立页的 el-page-header)。
    emit('loaded', monitor.value)
  } finally {
    loading.value = false
  }
}

// queryOf 拼查询串:实时窗口(结束时间贴近「现在」)按原跨度滚动到当前时刻,
// 跨度恒定,颗粒度规则不会因刷新漂移而失效;固定区间则原样查询。
function queryOf(): string {
  const start = range.value[0].getTime()
  const end = range.value[1].getTime()
  const span = Math.max(0, end - start)
  const now = Date.now()
  const live = liveRange.value
  const toMs = live ? now : end
  const fromMs = live ? now - span : start
  return `from=${Math.floor(fromMs / 1000)}&to=${Math.floor(toMs / 1000)}&bucket=${bucketSec.value}`
}

async function loadStats() {
  const mid = props.monitorId
  const q = queryOf()
  stats.value = (await http.get(`/monitors/${mid}/stats?${q}`)) as unknown as StatsBucket[]
  agentLatency.value = (await http.get(`/monitors/${mid}/agent-latency?${q}`)) as unknown as AgentLatencyPoint[]
  // 区间汇总可用率/延迟(按有效样本加权);下载速度监控额外算区间平均速度
  // (按各桶的样本数加权,单位换算回监控配置的单位)。
  let valid = 0, success = 0, lsum = 0, lcnt = 0, ssum = 0, scnt = 0
  for (const s of stats.value) {
    valid += s.valid || 0
    success += s.success || 0
    if (s.avgLatencyMs != null && s.valid > 0) {
      lsum += s.avgLatencyMs * s.valid
      lcnt += s.valid
    }
    if (s.avgSpeedKbps != null && (s.speedCount || 0) > 0) {
      ssum += s.avgSpeedKbps * (s.speedCount || 0)
      scnt += s.speedCount || 0
    }
  }
  const avgKbps = scnt > 0 ? ssum / scnt : null
  summary.value = {
    rate: valid > 0 ? +((success / valid) * 100).toFixed(2) : null,
    latency: lcnt > 0 ? +(lsum / lcnt).toFixed(2) : null,
    speed: avgKbps == null ? null : +fromKbps(avgKbps, speedUnit.value).toFixed(2),
  }
}

// 时间段变化:颗粒度若低于新跨度允许的下限则抬到下限,再重新查询。
function onRangeChange() {
  if (!range.value) range.value = [new Date(Date.now() - DAY_SEC * 1000), new Date()]
  // 结束时间贴近「现在」⇒ 实时窗口(快捷时段都属于这种);否则为固定区间。
  liveRange.value = Date.now() - range.value[1].getTime() <= LIVE_SLACK_MS
  if (bucketSec.value < minBucket.value) bucketSec.value = minBucket.value
  loadStats()
}
onMounted(() => {
  const mid = props.monitorId
  load()
  offs = [
    onRealtime('round_finalized', (data) => {
      const d = data as { monitorId?: string }
      if (d?.monitorId === mid) load()
    }),
    onRealtime('monitor_flipped', () => load()),
  ]
  timer = window.setInterval(load, 30000) // 兜底;WS 掉线时恢复 5s
})
watch(connected, (live) => {
  if (timer) clearInterval(timer)
  timer = window.setInterval(load, live ? 30000 : 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  offs.forEach((f) => f())
})

// agentName 解析节点名;节点已被删除、或结果来自外部上报时回退成可读文本。
const agentName = (id: string) =>
  agents.value[id]?.name || (id === 'external' ? t('type.push') : id.slice(0, 8))
const agentRegion = (id: string) => agents.value[id]?.region || ''

// pushFullUrl 外部上报地址的完整形态(后端回的是相对路径)。
const pushFullUrl = computed(() =>
  monitor.value?.pushUrl ? window.location.origin + monitor.value.pushUrl : '',
)

// copyPushUrl 复制上报地址(剪贴板 API 不可用时提示手动复制)。
async function copyPushUrl() {
  try {
    await navigator.clipboard.writeText(pushFullUrl.value)
    ElMessage.success(t('common.pushUrlCopied'))
  } catch {
    ElMessage.warning(t('common.copyFailedManual'))
  }
}

// agentSeries 把分节点延时对齐到主桶轴(按桶起点匹配,缺失桶留 null 断线)。
const agentSeries = computed(() => {
  if (!agentLatency.value.length) return []
  const byAgent = new Map<string, Map<number, number>>()
  for (const p of agentLatency.value) {
    const m = byAgent.get(p.agentId) ?? new Map<number, number>()
    m.set(p.bucketAt, p.avgLatencyMs)
    byAgent.set(p.agentId, m)
  }
  const axis = stats.value.map((s) => s.bucketAt)
  return [...byAgent.entries()].map(([id, m]) => ({
    name: agentName(id),
    region: agentRegion(id), // 图例/悬浮提示里显示国旗
    data: axis.map((at) => m.get(at) ?? null),
  }))
})

// stateTag 轮次状态标签:与列表页/状态条同一口径(UNKNOWN 单独一色,
// 其余按本轮判定值与当前阈值比)。两块表共用,保证同轮在两处的显示一致。
// 判定值随监控类型而变:成功率监控比 successRate,下载速度监控比平均速度
// (阈值换算成 KB/s 后比较,与后端 thresholdOf 同口径)。
function stateTag(r: TimelineRow) {
  if (r.state === 'UNKNOWN') return { type: 'info' as const, text: t('status.UNKNOWN') }
  const threshold = toKbps(monitor.value?.threshold ?? 80, speedUnit.value)
  const value = isDownload.value ? (r.avgSpeedKbps ?? 0) : r.successRate
  if (value >= threshold) return { type: 'success' as const, text: t('status.UP') }
  return { type: 'danger' as const, text: t('status.DOWN') }
}
</script>

<template>
  <div v-loading="loading" class="detail-panel" :class="{ 'detail-panel--dialog': inDialog }">
    <el-alert
      v-if="monitor && monitor.type === 'push'"
      type="info" show-icon :closable="false" style="margin-bottom:12px"
      :title="t('monitorDetail.pushDriven')"
    >
      <div class="push-box">
        {{ t('monitorDetail.pushUrlLabel') }}<code>{{ pushFullUrl }}</code>
        <el-button size="small" style="margin-left:8px" @click="copyPushUrl">{{ t('common.copy') }}</el-button>
        <i18n-t keypath="monitorDetail.pushTip" tag="div" scope="global" class="push-tip">
          <template #statusCode><code>status=up|down</code></template>
          <template #msg><code>{{ t('common.pushParamMsg') }}</code></template>
          <template #ping><code>{{ t('common.pushParamPing') }}</code></template>
          <template #period>{{ monitor.period }}</template>
          <template #consecutive>{{ monitor.consecutive }}</template>
          <template #lastPush>{{ monitor.lastPushAt || t('common.neverReported') }}</template>
        </i18n-t>
      </div>
    </el-alert>

    <div class="trend-bar">
      <h4 style="margin:0">{{ t('monitorDetail.trend') }}</h4>
      <div class="trend-ctl">
        <el-date-picker
          v-model="range"
          type="datetimerange"
          size="small"
          :range-separator="t('monitorDetail.rangeSeparator')"
          :start-placeholder="t('monitorDetail.startTime')"
          :end-placeholder="t('monitorDetail.endTime')"
          :shortcuts="rangeShortcuts"
          :clearable="false"
          @change="onRangeChange"
        />
        <span class="ctl-lbl">{{ t('monitorDetail.granularity') }}</span>
        <el-select v-model="bucketSec" size="small" style="width:118px" @change="loadStats">
          <el-option
            v-for="g in granularities" :key="g.value"
            :label="g.label" :value="g.value" :disabled="g.value < minBucket"
          />
        </el-select>
      </div>
    </div>
    <p class="trend-tip">
      {{ t('monitorDetail.trendTip') }}
      {{ t('monitorDetail.trendTipPoints', { count: stats.length }) }} ·
      {{ t(liveRange ? 'monitorDetail.liveWindow' : 'monitorDetail.fixedRange') }}。
    </p>
    <el-row :gutter="16" style="margin-bottom:8px">
      <el-col :span="6">
        <el-card shadow="never">
          <div class="sum-lbl">
            {{ isDownload ? t('monitorDetail.rangeAvgSpeed') : t('monitorDetail.rangeAvailability') }}
          </div>
          <div class="sum-val">
            {{ (isDownload ? summary.speed : summary.rate) ?? '—' }}
            <span v-if="(isDownload ? summary.speed : summary.rate) != null" class="sum-unit">
              {{ isDownload ? speedUnit : '%' }}
            </span>
          </div>
        </el-card>
      </el-col>
      <el-col :span="6">
        <el-card shadow="never"><div class="sum-lbl">{{ t('monitorDetail.rangeLatency') }}</div>
          <div class="sum-val">{{ summary.latency ?? '—' }}<span v-if="summary.latency != null" class="sum-unit">ms</span></div></el-card>
      </el-col>
    </el-row>
    <el-card shadow="never" style="margin-bottom:20px">
      <!-- 下载速度监控:第一条曲线画速度(独立轴、单位随监控配置)而不是可用率。
           :threshold 画一条阈值虚线,口径与第一条曲线相同(见 chartThreshold)。 -->
      <TrendChart
        :data="stats"
        :agents="agentSeries"
        :metric="isDownload ? 'speed' : 'rate'"
        :speed-unit="speedUnit"
        :threshold="chartThreshold"
      />
    </el-card>

    <!-- 两块表各占一半、同一行并排(弹窗与独立页同口径):轮次时间线在左,
         最近状态变动记录在右。窄屏(:xs)才退化成上下堆叠,免得半幅窄到无法阅读。 -->
    <el-row :gutter="16" align="top">
      <el-col :xs="24" :lg="12">
        <h4>{{ t('monitorDetail.timelineTitle') }}</h4>
        <p v-if="monitor && monitor.invertMode" class="invert-tip">
          {{ t('monitorDetail.invertPanelTip') }}
        </p>
        <p v-else class="panel-tip">{{ t('monitorDetail.roundsTip', { count: rounds.length }) }}</p>
        <RoundTimeTable
          :rows="rounds"
          :empty-text="t('monitorDetail.roundsEmpty')"
          :state-tag="stateTag"
          :agent-name="agentName"
          :agent-region="agentRegion"
          :speed-unit="isDownload ? speedUnit : ''"
        />
      </el-col>
      <el-col :xs="24" :lg="12">
        <h4>{{ t('monitorDetail.stateChangesTitle') }}</h4>
        <p class="panel-tip">{{ t('monitorDetail.stateChangesTip') }}</p>
        <RoundTimeTable
          :rows="stateChanges"
          :empty-text="t('monitorDetail.stateChangesEmpty')"
          :state-tag="stateTag"
          :agent-name="agentName"
          :agent-region="agentRegion"
          :speed-unit="isDownload ? speedUnit : ''"
          show-change
          time-field="changedAt"
        />
      </el-col>
    </el-row>
  </div>
</template>

<style scoped>
/* 弹窗里的正文左右各留 8px:el-row 的 gutter 会给行加 -8px 左右外边距(列的 8px 内边距
   靠它抵消),行本身因此比内容盒宽 16px。整页里父级够宽看不出来,弹窗里就凭空多出一条
   横向滚动条(实测正文 1208 / scrollWidth 1216)。补上这 16px 后行宽正好等于正文宽度。
   注意:这条只能挂在本面板自己的根节点上 —— el-dialog 开了 append-to-body,整个弹窗被
   teleport 到 body,`class="detail-dialog"` 的 scoped 属性落在 .el-overlay 上而不是
   .el-dialog 内层,写在弹窗那侧的 `.detail-dialog :deep(.el-dialog__body)` 根本匹配不到
   (实测 padding 仍是 0)。 */
.detail-panel--dialog {
  padding-left: 8px;
  padding-right: 8px;
}
.trend-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 6px;
}
.trend-ctl {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.trend-tip { font-size: 12px; color: #909399; margin: 0 0 12px; }
/* 提示行按两行高度占位:左右两个面板的表头始终在同一水平线上
   (有无反转模式提示、提示是否折行都不跳)。 */
.invert-tip { font-size: 12px; color: #e6a23c; margin: 0 0 8px; min-height: 36px; }
.panel-tip { font-size: 12px; color: #909399; margin: 0 0 8px; min-height: 36px; }
.push-box { line-height: 1.9; }
.push-box code {
  background: #f5f7fa;
  padding: 1px 4px;
  border-radius: 3px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.push-tip { font-size: 12px; color: #909399; line-height: 1.7; }
.ctl-lbl { font-size: 13px; color: #606266; }
.sum-lbl { font-size: 12px; color: #909399; }
.sum-val { font-size: 24px; font-weight: 600; color: #303133; }
.sum-unit { font-size: 13px; color: #909399; margin-left: 2px; }
</style>