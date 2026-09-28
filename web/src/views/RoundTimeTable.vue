<script setup lang="ts">
// RoundTimeTable 是详情页「最近轮次时间线」与「最近状态变动记录」共用的表格:
// 两块面板的状态/成功率/节点明细口径一致,故列定义只有这一份;变动记录表在列首多一列
// 「变动」(UP→DOWN),否则看不出方向。行为空时给出各自的空文案。
//
// 节点明细列一律**单行**(见 .scratch/detail-agents-one-line/spec.md):一个节点一行,
// 行内 flex + nowrap,放不下的内容省略号截断;整行可点击,弹窗里给出该节点的完整明细
// (结果/说明/地域/回传时间/延迟/状态码/速度/完整失败原因)。失败原因等长文本
// (push 监控的静默说明、HTTP 报错串)因此不再把行撑高,也不必横向滚动就能看全。
//
// 唯一的列差异是**时间列**(见 timeField):轮次表给该轮的计划时间(调度器建轮/下发
// 那一刻),变动表给变动时间(该轮定稿、状态机真正翻转那一刻)。两者是两个时刻 ——
// 相隔"探测超时 + 收集宽限期 + 探活 3s + 一个调度 tick",push 监控还隔着静默判定的
// 一个周期 —— 所以变动表里写「计划时间」会让人以为那就是翻转发生的时刻,而它其实
// 只是"触发翻转的那一轮是什么时候开的"。
//
// 「变动」列里,恢复(DOWN→UP)那一行的标签右边还会多一张「持续 x 分 y 秒」的小卡:
// 报警持续了多久是恢复这条记录才有的信息(报错那条给不出 —— 当时还不知道要挂多久),
// 见 .scratch/alert-duration/spec.md。
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { flagClass, regionName } from '../utils/region'
import { formatSpeed } from '../utils/speed'
import { formatDuration } from '../utils/duration'
import type { StateChange, StateTag, TimelineRow } from '../api/types'

const props = defineProps<{
  rows: TimelineRow[]
  emptyText: string
  /** 本行的状态标签(与轮次时间线同一口径,由父组件提供,避免两处各写一套) */
  stateTag: (row: TimelineRow) => StateTag
  agentName: (id: string) => string
  agentRegion: (id: string) => string
  /** true 时显示列首的「变动」列(状态变动记录用) */
  showChange?: boolean
  /**
   * 本行的时间列取哪个字段、用哪个列名:
   *   'scheduledAt'(默认)⇒ 计划时间 —— 该轮是什么时候建的(轮次时间线用);
   *   'changedAt'         ⇒ 变动时间 —— 状态真正翻转的时刻(状态变动记录用,
   *                          接口在变动行上额外带出 `changedAt`)。
   * 变动表必须用 'changedAt':它的 `scheduledAt` 是**触发翻转那一轮**的计划时间,
   * 与总览页流水的「变动时间」本该是同一个时刻的两种说法,不统一就会出现
   * "同一次翻转,详情页显示 10:00:00、总览页显示 10:00:18"。
   * 列名复用 overview.columnChangedAt —— "变动时间"这个概念全局只有一份词条。
   */
  timeField?: 'scheduledAt' | 'changedAt'
  /**
   * 非空表示下载速度监控:判定列展示本轮平均速度(单位即此值)而不是成功率,
   * 节点明细里也带上各节点测得的speed。
   */
  speedUnit?: string
  /**
   * 监控是否开启反转模式:结果的 ok 已是折算后的判定(后端 EffOK),
   * error/status 仍是原始观测。反转下两种折算形态要单独说明 ——
   *   原始成功被记为失败:没有报错文本,弹窗若还说「原因见失败原因」就是空指引;
   *   原始失败被记为正常:结果「正常」却带着报错,标签改成「原始报错」才不自相矛盾。
   */
  inverted?: boolean
}>()

const { t } = useI18n()

/**
 * durationOf 本行要显示的「报警持续时长」文本;不显示时返回空串。
 *
 * 三个前提缺一不可:这是一条恢复记录(DOWN→UP)、后端算出了时长(>0)、
 * 它能被格式化成文本。恢复却没有时长(0)是真实存在的情况 —— 监控的开局就是 DOWN、
 * 或配对的那条报错记录被删掉了 —— 那时宁可什么都不显示,也不写「持续 0 秒」。
 */
function durationOf(row: TimelineRow): string {
  const sc = row as StateChange
  if (sc.toState !== 'UP') return ''
  return sc.durationSec ? formatDuration(sc.durationSec) : ''
}

/**
 * DetailLine 节点明细的一行:把「已回传结果」与三种「没有样本」收敛成同一结构,
 * 单行渲染(此前四种各写一段 v-for,折行行为与省略结构要维护四份)。
 * kind 口径与后端 round 聚合一致(scheduler/round.go):results / missingAlive /
 * missingDead / missingUndispatched。
 */
interface DetailLine {
  agentId: string
  kind: 'result' | 'timeout' | 'offline' | 'undispatched'
  /** kind === 'result' 时:折算反转后的有效判定(与后端 EffOK 同口径) */
  ok?: boolean
  late?: boolean
  latencyMs?: number
  httpStatus?: number
  speedKbps?: number
  /** Agent 的原始报错文本(反转与否都不改写,详见弹窗) */
  error?: string
  createdAt?: string
}

// linesOf 一轮的节点明细行:结果在前(与后端 results 顺序一致),超时/离线/未派发在后。
function linesOf(row: TimelineRow): DetailLine[] {
  const out: DetailLine[] = (row.results || []).map((r) => ({
    agentId: r.agentId, kind: 'result' as const, ok: r.ok, late: r.late,
    latencyMs: r.latencyMs, httpStatus: r.httpStatus, speedKbps: r.speedKbps,
    error: r.error, createdAt: r.createdAt,
  }))
  for (const id of row.missingAlive || []) out.push({ agentId: id, kind: 'timeout' })
  for (const id of row.missingDead || []) out.push({ agentId: id, kind: 'offline' })
  for (const id of row.missingUndispatched || []) out.push({ agentId: id, kind: 'undispatched' })
  return out
}

// verdictOf 节点行的判定词:行内只放短词(单行省略),完整解释在弹窗(noteOf)。
// 渲染期调用,随语言切换。
function verdictOf(l: DetailLine): string {
  if (l.kind === 'result') return l.ok ? t('detail.lineOk') : t('detail.lineFail')
  if (l.kind === 'timeout') return t('detail.lineTimeout')
  if (l.kind === 'offline') return t('detail.lineOffline')
  return t('detail.lineUndispatched')
}

// noteOf 弹窗里的口径说明:三种「没有样本」的成因与是否计入分母,与后端聚合一致
// (超时=活着没回传,计失败样本;离线/未派发不计分母)。结果行的说明按反转与否分形态:
// 反转下「失败」可能是原始探测成功被折算(此时没有报错文本),要直接说破,别再
// 指向一个不存在的「失败原因」行。
function noteOf(l: DetailLine): string {
  if (l.kind === 'result') {
    if (l.ok) {
      return props.inverted ? t('detail.popupInvertedOkNote') : t('detail.popupOkNote')
    }
    return props.inverted && !l.error
      ? t('detail.popupInvertedFailNote')
      : t('detail.popupFailNote')
  }
  if (l.kind === 'timeout') return t('detail.popupTimeoutNote')
  if (l.kind === 'offline') return t('detail.popupOfflineNote')
  return t('detail.popupUndispatchedNote')
}

function lineTagType(l: DetailLine): 'success' | 'danger' | 'warning' | 'info' {
  if (l.kind === 'result') return l.ok ? 'success' : 'danger'
  return l.kind === 'timeout' ? 'warning' : 'info'
}

// lineSummary 单行里跟在节点标签后面的摘要:成功给「延迟 · 状态码 · 速度」,
// 失败给报错文本,三种「没有样本」给判定词(离线与未派发同是灰色标签,
// 行内必须留字面区分,完整解释在弹窗)。渲染期调用,随语言切换。
function lineSummary(l: DetailLine): string {
  if (l.kind !== 'result') return verdictOf(l)
  if (l.ok) {
    const parts = [`${(l.latencyMs ?? 0).toFixed(1)}ms`]
    if (l.httpStatus) parts.push(String(l.httpStatus))
    if (props.speedUnit && l.speedKbps) parts.push(formatSpeed(l.speedKbps, props.speedUnit))
    return parts.join(' · ')
  }
  return l.error || t('common.failed')
}

// 点击节点行 → 弹窗看该节点的完整内容(单行省略的出口)。
const popupLine = ref<DetailLine | null>(null)
const popupVisible = ref(false)
function openPopup(l: DetailLine) {
  popupLine.value = l
  popupVisible.value = true
}

// popupItems 弹窗的描述项:结果与说明恒有,其余按数据有无给(不凑空行)。
// 反转下「正常」行带的报错是原始观测(探测其实失败了),label 用「原始报错」。
// computed 依赖 t() 与 props/父组件回调里的响应式数据,语言与数据变化都会跟着重算。
const popupItems = computed(() => {
  const l = popupLine.value
  if (!l) return []
  const items: { label: string; value: string }[] = [
    { label: t('detail.popupResult'), value: verdictOf(l) },
    { label: t('detail.popupNote'), value: noteOf(l) },
  ]
  const region = props.agentRegion(l.agentId)
  if (region) items.push({ label: t('detail.popupRegion'), value: regionName(region) })
  if (l.kind === 'result') {
    if (l.createdAt) items.push({ label: t('detail.popupReportedAt'), value: l.createdAt })
    if ((l.latencyMs ?? 0) > 0) {
      items.push({ label: t('detail.popupLatency'), value: `${(l.latencyMs ?? 0).toFixed(1)} ms` })
    }
    if (l.httpStatus) items.push({ label: t('detail.popupHttpStatus'), value: String(l.httpStatus) })
    if (props.speedUnit && l.speedKbps) {
      items.push({ label: t('detail.popupSpeed'), value: formatSpeed(l.speedKbps, props.speedUnit) })
    }
    if (l.error) {
      items.push({
        label: t(props.inverted ? 'detail.popupRawError' : 'detail.popupError'),
        value: l.error,
      })
    }
  }
  return items
})
</script>

<template>
  <el-table :data="rows" :empty-text="emptyText" size="small">
    <!-- 「变动」列:方向标签(UP → DOWN),恢复那一行右边紧跟一张「报警持续时长」小卡。
         时长不是"恢复"之外的第二个概念,而是恢复这条记录才有的信息,故贴着标签放
         (另起一列会让它离标签很远,报错行还得空一格)。224 是足迹实测值:方向标签
         「UP → DOWN」86px + 间距 4px + 最宽的时长文本 113px(「持续 30 天 23 小时」)= 203px,
         加上 Element Plus 单元格左右内边距 16px ⇒ 219px;取 224 留一点字体回旋余量。
         详情页两张表并排、五列都是硬编码宽度(合计 700+),横向本来就会滚动,所以这
         一列宽一点只影响"再滚一点",却换来每张恢复卡都能一行读完 —— 值。 -->
    <el-table-column v-if="showChange" :label="t('detail.columnChange')" width="224">
      <template #default="{ row }">
        <div class="change-cell">
          <el-tag size="small" :type="row.toState === 'DOWN' ? 'danger' : 'success'">
            {{ row.fromState }} → {{ row.toState }}
          </el-tag>
          <!-- 只有恢复行有值:durationSec 由后端在恢复落记录时算好(上一条报错到此刻的
               间隔)。报错行、以及没有配对报错记录的历史恢复行都不显示 ——
               "要挂多久"在报错那一刻谁也不知道,填个 0 秒反而是假信息。
               悬停提示说清这个数的起止点(判 DOWN → 恢复),否则"持续 5 分钟"到底
               从哪算起只能猜。 -->
          <el-tooltip v-if="durationOf(row)" :content="t('common.alertDurationTip')" placement="top">
            <el-tag size="small" type="info" effect="plain" class="duration-tag">
              {{ t('common.durationTag', { duration: durationOf(row) }) }}
            </el-tag>
          </el-tooltip>
        </div>
      </template>
    </el-table-column>
    <!-- 时间列:列名跟着字段走(见 timeField 的说明)。宽度不变(两种列名等宽,
         150 是原口径),两张表并排时的横向滚动量因此不受影响。 -->
    <el-table-column
      :prop="timeField || 'scheduledAt'"
      :label="t(timeField === 'changedAt' ? 'overview.columnChangedAt' : 'detail.columnScheduledAt')"
      width="150"
    />
    <el-table-column :label="t('detail.columnState')" width="78">
      <template #default="{ row }">
        <el-tag size="small" :type="stateTag(row).type">{{ stateTag(row).text }}</el-tag>
      </template>
    </el-table-column>
    <el-table-column
      :label="speedUnit ? t('detail.columnSpeed') : t('detail.columnSuccessRate')"
      width="118"
    >
      <template #default="{ row }">
        <template v-if="row.state === 'UNKNOWN'">
          <span style="color:#909399">{{ t('detail.noValidSamples') }}</span>
        </template>
        <!-- 下载速度监控:判定值是本轮平均速度(单位随监控配置)。
             可下载率(成功/有效)照旧一并给出,便于区分"下不下来"与"下得够不够快"。 -->
        <template v-else-if="speedUnit">
          <div>{{ formatSpeed(row.avgSpeedKbps, speedUnit) || '—' }}</div>
          <div style="font-size:12px;color:#909399">{{ row.success }}/{{ row.valid }}</div>
        </template>
        <template v-else>
          <span>{{ row.success }}/{{ row.valid }} = {{ row.successRate.toFixed(1) }}%</span>
        </template>
      </template>
    </el-table-column>
    <el-table-column :label="t('detail.columnAgents')" min-width="200">
      <template #default="{ row }">
        <!-- 一个节点一行、一律单行:节点标签 + 摘要 + 迟到标记横向排布,放不下省略;
             整行可点击,完整内容(含完整失败原因)在表格后面的弹窗里。 -->
        <div
          v-for="l in linesOf(row)"
          :key="l.agentId"
          class="agent-line"
          :class="{ 'agent-line--fail': l.kind === 'result' && !l.ok }"
          @click="openPopup(l)"
        >
          <el-tooltip
            v-if="agentRegion(l.agentId)"
            :content="regionName(agentRegion(l.agentId))"
            placement="top"
          >
            <el-tag size="small" :type="lineTagType(l)" class="agent-tag">
              <span
                v-if="flagClass(agentRegion(l.agentId))"
                :class="flagClass(agentRegion(l.agentId))"
                class="agent-flag"
              />{{ agentName(l.agentId) }}
            </el-tag>
          </el-tooltip>
          <el-tag v-else size="small" :type="lineTagType(l)" class="agent-tag">{{ agentName(l.agentId) }}</el-tag>
          <span class="agent-sum">{{ lineSummary(l) }}</span>
          <el-tag v-if="l.late" size="small" type="warning" class="late-tag">{{ t('detail.late') }}</el-tag>
        </div>
      </template>
    </el-table-column>
  </el-table>
  <!-- 节点行详情弹窗:单行省略后「点击看全」的出口。append-to-body:这张表常嵌在
       详情弹窗(MonitorDetailDialog)里,不 teleport 会被父层定位/层叠裁剪。 -->
  <el-dialog
    v-model="popupVisible"
    :title="t('detail.popupTitle', { agent: popupLine ? agentName(popupLine.agentId) : '' })"
    width="560px"
    append-to-body
  >
    <div class="line-popup">
      <el-descriptions v-if="popupLine" :column="1" border size="small">
        <el-descriptions-item v-for="it in popupItems" :key="it.label" :label="it.label">
          {{ it.value }}
        </el-descriptions-item>
      </el-descriptions>
    </div>
  </el-dialog>
</template>

<style scoped>
.agent-flag {
  border-radius: 2px;
  font-size: 14px;
  line-height: 1;
  margin-right: 4px;
}
/* 方向标签 + 持续时长卡:一行摆不下就换行(row-gap 让换行后两张卡不贴在一起)。 */
.change-cell {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px;
}
/* 时长卡与方向标签同色系但更淡:它是补充信息,不该抢「恢复」这个结论的注意力。 */
.duration-tag { font-variant-numeric: tabular-nums; }

/* 节点明细:一个节点一行、一律单行。标签不参与收缩(flex:none),超长节点名的
   省略号打在标签内部 —— el-tag 是 inline-flex,只靠外层 text-overflow 会切掉半截字
   (渠道页同款处理);摘要占满剩余宽度并省略。整行可点,悬停摘要变主色示意。 */
.agent-line {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  line-height: 20px;
  cursor: pointer;
}
.agent-tag { flex: none; max-width: 100%; }
.agent-line .agent-tag :deep(.el-tag__content) {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.agent-sum {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  font-variant-numeric: tabular-nums;
}
.agent-line--fail .agent-sum { color: #f56c6c; }
.agent-line:hover .agent-sum { color: var(--el-color-primary); }
.late-tag { flex: none; }
/* 弹窗正文:完整内容允许折行,无空格的长报错串也能断行。 */
.line-popup :deep(.el-descriptions__cell) { overflow-wrap: anywhere; }
</style>
