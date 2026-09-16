<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import MonitorDetailPanel from './MonitorDetailPanel.vue'
import MonitorDetailTags from './MonitorDetailTags.vue'
import { closeMonitorDetail, detailMonitorId } from '../utils/monitorDetailDialog'

// 详情接口返回的行:这里只用到名称与标签那几项,取松散类型(与 MonitorDetailTags 一致)。
type MonitorRow = Record<string, any>

/**
 * MonitorDetailDialog 监控详情弹窗:从列表页点监控名、总览页点变动记录或卡片时打开。
 *
 * 由 MainLayout 挂**唯一一份**(状态在 utils/monitorDetailDialog.ts 的模块级 ref):
 * 详情内容自带趋势图(ECharts)与 30s 轮询 + WebSocket 订阅,每个页面各挂一份会同时在
 * 后台跑好几套。这里只有"当前正在看的那一个监控"是活的。
 *
 * 数据加载、实时订阅、标签口径全在 MonitorDetailPanel / MonitorDetailTags 里,
 * 与 /monitors/:id 独立页共用同一份实现(见那两个组件的说明)。
 */
const { t } = useI18n()

const visible = computed(() => detailMonitorId.value !== null)
// 当前监控的详情行:标题与状态标签要用。切换监控时面板会重新 emit loaded 覆盖它。
const loaded = ref<MonitorRow | null>(null)

// 换监控时先把上一份的标题/标签清掉,免得新数据到位前显示的是旧监控的名字 ——
// 这一瞬间的错配比"标题空一下"更容易误导。
watch(detailMonitorId, () => { loaded.value = null })

function onLoaded(m: unknown) {
  loaded.value = m as MonitorRow
}
// 弹窗关闭时把标题/标签一并清掉,下次打开不会有上一份的残留。
function onClosed() {
  loaded.value = null
  closeMonitorDetail()
}
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="loaded?.name || t('monitorDetail.title')"
    width="90%"
    top="5vh"
    class="detail-dialog"
    append-to-body
    destroy-on-close
    @closed="onClosed"
  >
    <template #header>
      <div class="dlg-head">
        <span class="dlg-title">{{ loaded?.name || t('monitorDetail.title') }}</span>
        <MonitorDetailTags :monitor="loaded" />
      </div>
    </template>
    <!-- detailMonitorId 为 null 时不渲染面板:关掉的瞬间不该还在跑一份轮询。 -->
    <MonitorDetailPanel
      v-if="detailMonitorId"
      :key="detailMonitorId"
      :monitor-id="detailMonitorId"
      layout="dialog"
      @loaded="onLoaded"
    />
    <template #footer>
      <el-button @click="closeMonitorDetail()">{{ t('common.close') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
/* 弹窗头要放得下"名称 + 一串状态标签",默认的 title 行是横向 flex 且不换行。 */
.dlg-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding-right: 24px;
}
.dlg-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}
/* 详情内容较高:限制弹窗体高,内容自己滚,避免弹窗顶出视口。
   注意 el-dialog 开了 append-to-body:整个弹窗被 teleport 到 body,`class="detail-dialog"`
   的 scoped 属性落在 .el-overlay 上,`.detail-dialog :deep(.el-dialog__body)` 匹配不到
   弹窗内层(实测 padding 仍是 0)。所以正文的内边距写在 MonitorDetailPanel 自己的根节点上
   (见那边的 .detail-panel--dialog),这里只用 :global 兜住"限高"这一条。 */
:global(.detail-dialog .el-dialog__body) {
  max-height: 78vh;
  overflow: auto;
}
</style>