<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

/**
 * MonitorDetailTags 是监控详情里"标题旁那一组标签"(分组 / 反转 / IP 协议族 / JSON 断言 /
 * 启用状态 + 摘要)。弹窗的头部与独立详情页的 el-page-header 共用它,两处的标签口径
 * 与顺序不会漂移。
 *
 * monitor 由外层在详情数据到位后传进来(null = 还在加载,什么都不渲染)。
 */
const props = defineProps<{ monitor: Record<string, any> | null }>()

const { t } = useI18n()

const isDownload = computed(() => props.monitor?.type === 'download')
const isPush = computed(() => props.monitor?.type === 'push')
const speedUnit = computed<string>(() => props.monitor?.speedUnit || 'KB/s')

// operatorLabel 把 JSON 断言的比较方式转成当前语言的展示(与后端 checkconfig 取值一致)。
function operatorLabel(op: string): string {
  const labels: Record<string, string> = {
    '==': t('monitorForm.operators.eq'),
    '!=': t('monitorForm.operators.neq'),
    contains: t('monitorForm.operators.contains'),
    '>': t('monitorForm.operators.gt'),
    '>=': t('monitorForm.operators.gte'),
    '<': t('monitorForm.operators.lt'),
    '<=': t('monitorForm.operators.lte'),
  }
  return labels[op] || op || t('monitorForm.operators.eq')
}
</script>

<template>
  <template v-if="monitor">
    <el-tag v-if="monitor.group" type="info" size="small" style="margin-right:8px">
      {{ t('monitorDetail.groupTag', { group: monitor.group }) }}
    </el-tag>
    <el-tooltip v-if="monitor.invertMode"
      :content="t('monitorDetail.invertTip')" placement="top">
      <el-tag type="warning" size="small" style="margin-right:8px">{{ t('monitorDetail.invertTag') }}</el-tag>
    </el-tooltip>
    <!-- 非 auto 才显示:auto 是默认值,标出来只是噪声。 -->
    <el-tooltip v-if="monitor.ipVersion && monitor.ipVersion !== 'auto'"
      :content="t('monitorDetail.ipVersionTip')" placement="top">
      <el-tag type="info" size="small" effect="plain" style="margin-right:8px">
        {{ monitor.ipVersion.toUpperCase() }}
      </el-tag>
    </el-tooltip>
    <el-tooltip v-if="monitor.jsonPath"
      :content="t('monitorDetail.jsonAssertTip', {
        path: monitor.jsonPath,
        operator: operatorLabel(monitor.jsonPathOperator),
      })"
      placement="top">
      <el-tag type="warning" size="small" style="margin-right:8px">
        {{ t('monitorDetail.jsonAssertTag', {
          path: monitor.jsonPath,
          operator: operatorLabel(monitor.jsonPathOperator),
          expected: monitor.jsonAssertExpected,
        }) }}
      </el-tag>
    </el-tooltip>
    <el-tag :type="monitor.enabled ? 'success' : 'info'" size="small">
      {{ t(monitor.enabled ? 'monitors.stateEnabled' : 'monitors.statePaused') }} · {{ monitor.type.toUpperCase() }} ·
      <!-- 下载速度监控的阈值是速度:带单位而不是 %;外部上报没有阈值可比,只报连续轮数。 -->
      {{ isPush
        ? t('monitorDetail.summaryPush', {
          period: monitor.period, consecutive: monitor.consecutive,
        })
        : isDownload
        ? t('monitorDetail.summarySpeed', {
          period: monitor.period, threshold: monitor.threshold,
          unit: speedUnit, consecutive: monitor.consecutive,
        })
        : t('monitorDetail.summary', {
          period: monitor.period, threshold: monitor.threshold, consecutive: monitor.consecutive,
        }) }}
    </el-tag>
  </template>
</template>