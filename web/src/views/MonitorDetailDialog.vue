<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import MonitorDetailPanel from './MonitorDetailPanel.vue'
import MonitorDetailTags from './MonitorDetailTags.vue'
import MonitorFormDialog from './MonitorFormDialog.vue'
import type { Agent } from './MonitorFormDialog.vue'
import { closeMonitorDetail, detailMonitorId } from '../utils/monitorDetailDialog'
import { http } from '../api/http'

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
 *
 * 头部有「编辑」按钮:在 MonitorFormDialog(嵌套弹窗)里直接改这份监控的配置,
 * 保存成功后重挂详情面板(popupKey),趋势图/两张表立即按新配置口径重查;
 * 底层的列表/总览页由后端的 monitor_changed 广播各自更新,这里不用管。
 */
const { t } = useI18n()

const visible = computed(() => detailMonitorId.value !== null)
// 当前监控的详情行:标题与状态标签要用。切换监控时面板会重新 emit loaded 覆盖它。
const loaded = ref<MonitorRow | null>(null)
const detailLoading = ref(true)

// 换监控时先把上一份的标题/标签清掉,免得新数据到位前显示的是旧监控的名字 ——
// 这一瞬间的错配比"标题空一下"更容易误导。
watch(detailMonitorId, () => {
  loaded.value = null
  detailLoading.value = true
})

function onLoaded(m: unknown) {
  loaded.value = m as MonitorRow
}
// 弹窗关闭时把标题/标签一并清掉,下次打开不会有上一份的残留。
function onClosed() {
  loaded.value = null
  detailLoading.value = false
  closeMonitorDetail()
}

function onLoading(value: unknown) {
  detailLoading.value = Boolean(value)
}

// ---- 直接编辑 ----
// 表单需要的节点/渠道/分组:打开表单弹窗时拉一次(与列表页打开表单的时机一致,
// 详情弹窗本身不常驻这些数据,提前拉只会白跑)。分组从详情行自身给一个,
// 供"换到新分组"下拉之外的原值回显(el-select allow-create 本就能输入新值)。
const editVisible = ref(false)
const editMonitor = ref<MonitorRow | null>(null)
const formAgents = ref<Agent[]>([])
const formChannels = ref<{ id: string; name: string; enabled?: boolean }[]>([])
const formGroups = ref<string[]>([])

async function openEdit() {
  if (!loaded.value) return
  editMonitor.value = loaded.value
  editVisible.value = true
  try {
    const [as, cs] = await Promise.all([
      http.get('/agents?status=approved'),
      http.get('/channels'),
    ])
    formAgents.value = as as unknown as Agent[]
    formChannels.value = cs as unknown as { id: string; name: string; enabled?: boolean }[]
    if (loaded.value?.group) formGroups.value = [loaded.value.group]
  } catch { /* 拉取失败不拦编辑:表单里节点/渠道下拉为空,保存仍可用 */ }
}

// 保存成功:重挂详情面板(key 变了,趋势图与两张表重新按当前配置查询),
// 标题/标签用保存后的详情行覆盖;后端同时广播 monitor_changed,底层页面各自更新。
const panelKey = ref(0)
function onEditSaved() {
  panelKey.value++
  void refreshDetailRow()
}

async function refreshDetailRow() {
  const id = detailMonitorId.value
  if (!id) return
  try {
    loaded.value = (await http.get(`/monitors/${id}`)) as unknown as MonitorRow
  } catch { /* 详情面板自己的轮询会再兜底 */ }
}
</script>

<template>
  <el-dialog
    v-model="visible"
    v-loading="detailLoading"
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
        <!-- 直接编辑当前监控:表单弹窗嵌在详情弹窗之上(el-dialog 默认遮罩互不冲突),
             保存后详情面板按新配置重挂。 -->
        <el-button size="small" style="margin-left:auto" @click="openEdit">
          {{ t('common.edit') }}
        </el-button>
      </div>
    </template>
    <!-- detailMonitorId 为 null 时不渲染面板:关掉的瞬间不该还在跑一份轮询。
         key 的两个维度:detailMonitorId —— 切换监控必须重挂(面板的 load 只在 mounted
         跑一次,不换 key 会把上一个监控的数据留在屏上);panelKey —— 编辑保存后 +1,
         强制按新配置重查(面板自己的轮询/订阅对配置变化无感知)。 -->
    <MonitorDetailPanel
      v-if="detailMonitorId"
      :key="`${detailMonitorId}:${panelKey}`"
      :monitor-id="detailMonitorId"
      layout="dialog"
      @loaded="onLoaded"
      @loading="onLoading"
    />
    <template #footer>
      <el-button @click="closeMonitorDetail()">{{ t('common.close') }}</el-button>
    </template>
  </el-dialog>
  <!-- 编辑表单(嵌套弹窗):modelValue 自管,不与详情弹窗的 visible 联动 ——
       表单里点取消/关闭只收表单,详情弹窗保持原样。 -->
  <MonitorFormDialog
    v-model="editVisible"
    :editing="editMonitor"
    :agents="formAgents"
    :channels="formChannels"
    :groups="formGroups"
    @saved="onEditSaved"
  />
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