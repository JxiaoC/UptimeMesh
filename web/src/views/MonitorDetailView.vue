<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import MonitorDetailPanel from './MonitorDetailPanel.vue'
import MonitorDetailTags from './MonitorDetailTags.vue'

/**
 * MonitorDetailView 是 /monitors/:id 的**独立页外壳**。
 *
 * 页面的正文(趋势图、区间汇总、两张表)在 MonitorDetailPanel 里,与列表页/总览页弹出的
 * MonitorDetailDialog 共用同一份实现 —— 这里是"换个壳":用 el-page-header 做标题与返回,
 * 而不是弹窗的 #header。
 *
 * 为什么保留这个路由:直接粘 URL、旧书签、从别处链接过来仍然要看得到详情。
 * 页面内部的点击入口(列表页监控名、总览页变动记录与卡片)已经一律改成弹窗,
 * 不再跳到这里;这里只在"直接访问这个地址"时生效。
 */
const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const mid = route.params.id as string

// 详情行由面板加载完成后回传:标题与状态标签要用。
const monitor = ref<Record<string, any> | null>(null)

// goBack 返回上一级:从总览页进来的回总览,其余情况回监控列表。
// 来源由进入时带的 ?from=overview 标明 —— 早先一律回监控列表,于是从总览点进来
// 再点返回会落到「监控」页,与用户的上一步不符。
function goBack() {
  router.push({ name: route.query.from === 'overview' ? 'overview' : 'monitors' })
}
</script>

<template>
  <div>
    <el-page-header
      @back="goBack"
      :content="monitor ? monitor.name : t('monitorDetail.title')"
      style="margin-bottom:16px"
    >
      <template #extra>
        <MonitorDetailTags :monitor="monitor" />
      </template>
    </el-page-header>
    <MonitorDetailPanel
      :monitor-id="mid"
      @loaded="(m) => (monitor = m as Record<string, any>)"
    />
  </div>
</template>