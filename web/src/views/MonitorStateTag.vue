<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

/**
 * 状态标签:列表页里唯一"会随实时推送变化"的标签类单元格。
 *
 * 为什么单独做成叶子组件(不要内联进 el-table 的插槽):el-table 的 TableBody
 * 渲染副作用会把插槽里读到的响应式字段收集成依赖,于是"某一行 displayState 变了"
 * 会触发**整表**重渲染(219 行 × 11 列,实测每次数百毫秒)。这里只把监控 id
 * 传进来,组件自己去 live 表里取状态,依赖就落在它自己身上:一次轮次定稿只重渲染
 * 这一个标签。详见 MonitorsView.vue 顶部的性能说明。
 */
interface LiveState { displayState?: string }

const props = defineProps<{
  live: Record<string, LiveState | undefined>
  monitorId: string
  monitorType: string
}>()

const { t } = useI18n()

// displayState → 标签样式与文案
// UNKNOWN 对 push(外部上报)监控的文案是"等待上报",更贴近它的实际含义。
// 文案放在 computed 里:模块级常量只求值一次,切语言时标签不会跟着变。
const stateMap = computed<
  Record<string, { type: 'success' | 'danger' | 'info' | 'warning'; text: string }>
>(() => ({
  UP: { type: 'success', text: t('status.UP') },
  DOWN: { type: 'danger', text: t('status.DOWN') },
  UNKNOWN: { type: 'info', text: t('status.UNKNOWN') },
  PAUSED: { type: 'info', text: t('status.PAUSED') },
}))

const displayState = computed(() => props.live[props.monitorId]?.displayState || 'UNKNOWN')
const tagType = computed(() => stateMap.value[displayState.value]?.type || 'info')
const tagText = computed(() => {
  if (displayState.value === 'UNKNOWN' && props.monitorType === 'push') return t('status.waitingPush')
  return stateMap.value[displayState.value]?.text || displayState.value
})
</script>

<template>
  <el-tag size="small" :type="tagType">{{ tagText }}</el-tag>
</template>
