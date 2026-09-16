<script setup lang="ts">
// RoundTimeTable 是详情页「最近轮次时间线」与「最近状态变动记录」共用的表格:
// 两块面板的状态/成功率/节点明细口径一致,故列定义只有这一份;变动记录表在列首多一列
// 「变动」(UP→DOWN),否则看不出方向。行为空时给出各自的空文案。
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
import { useI18n } from 'vue-i18n'
import { flagClass, regionName } from '../utils/region'
import { formatSpeed } from '../utils/speed'
import { formatDuration } from '../utils/duration'
import type { StateChange, StateTag, TimelineRow } from '../api/types'

defineProps<{
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
        <div v-for="r in row.results" :key="r.agentId" style="line-height:20px">
          <el-tooltip v-if="agentRegion(r.agentId)" :content="regionName(agentRegion(r.agentId))" placement="top">
            <el-tag size="small" :type="r.ok ? 'success' : 'danger'" style="margin-right:6px">
              <span :class="flagClass(agentRegion(r.agentId))" class="agent-flag" />
              {{ agentName(r.agentId) }}
            </el-tag>
          </el-tooltip>
          <el-tag v-else size="small" :type="r.ok ? 'success' : 'danger'" style="margin-right:6px">
            {{ agentName(r.agentId) }}
          </el-tag>
          <span v-if="r.ok">
            {{ r.latencyMs.toFixed(1) }}ms<span v-if="r.httpStatus"> · {{ r.httpStatus }}</span><span
              v-if="speedUnit && r.speedKbps"
            > · {{ formatSpeed(r.speedKbps, speedUnit) }}</span>
          </span>
          <span v-else style="color:#f56c6c">{{ r.error || t('common.failed') }}</span>
          <el-tag v-if="r.late" size="small" type="warning" style="margin-left:6px">{{ t('detail.late') }}</el-tag>
        </div>
        <div v-for="id in row.missingAlive" :key="'ma'+id" style="line-height:20px;color:#e6a23c">
          <span v-if="flagClass(agentRegion(id))" :class="flagClass(agentRegion(id))" class="agent-flag" />{{ t('detail.missingAlive', { agent: agentName(id) }) }}
        </div>
        <div v-for="id in row.missingDead" :key="'md'+id" style="line-height:20px;color:#909399">
          <span v-if="flagClass(agentRegion(id))" :class="flagClass(agentRegion(id))" class="agent-flag" />{{ t('detail.missingDead', { agent: agentName(id) }) }}
        </div>
        <!-- 任务没交到手上(与离线同"不计分母",但成因在派发侧:重启后首轮那批) -->
        <div v-for="id in row.missingUndispatched" :key="'mu'+id" style="line-height:20px;color:#909399">
          <span v-if="flagClass(agentRegion(id))" :class="flagClass(agentRegion(id))" class="agent-flag" />{{ t('detail.missingUndispatched', { agent: agentName(id) }) }}
        </div>
      </template>
    </el-table-column>
  </el-table>
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
</style>
