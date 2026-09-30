<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { http } from '../api/http'

/**
 * 通知记录页:历史所有通知投递的留痕(成功/失败都记),数据永久保存,
 * 不受设置的「原始结果保留(天)」限制。
 */

interface NotificationLog {
  id: string
  channelId: string
  channelName: string
  monitorId: string
  monitorName: string
  /** 事件类型:DOWN | UP | TEST | AGENT_DOWN | AGENT_UP(接口契约值,不翻译)。 */
  event: string
  /** 发送状态:success | failed(接口契约值)。 */
  status: string
  /** 实际尝试次数(重试耗尽时为总次数)。 */
  attempts: number
  title: string
  content: string
  error: string
  createdAt: string
}

interface MonitorOption {
  id: string
  name: string
}

const { t } = useI18n()

const loading = ref(false)
const logs = ref<NotificationLog[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

// 筛选:监控/渠道下拉的数据源在页面加载时拉一次(量级都是几十上百)。
const monitors = ref<MonitorOption[]>([])
const channels = ref<{ id: string; name: string }[]>([])
const filterMonitor = ref('')
const filterChannel = ref('')
const filterEvent = ref('')
const filterStatus = ref('')

// 事件与状态是接口契约值,选项文案按当前语言给 → 放 computed(切语言即时生效)。
const eventOptions = computed(() => [
  { value: 'DOWN', label: t('notifications.event.DOWN') },
  { value: 'UP', label: t('notifications.event.UP') },
  { value: 'TEST', label: t('notifications.event.TEST') },
  { value: 'AGENT_DOWN', label: t('notifications.event.AGENT_DOWN') },
  { value: 'AGENT_UP', label: t('notifications.event.AGENT_UP') },
])
const statusOptions = computed(() => [
  { value: 'success', label: t('notifications.status.success') },
  { value: 'failed', label: t('notifications.status.failed') },
])

async function load() {
  loading.value = true
  try {
    const res = (await http.get('/notifications', {
      params: {
        page: page.value,
        pageSize: pageSize.value,
        monitorId: filterMonitor.value || undefined,
        channelId: filterChannel.value || undefined,
        event: filterEvent.value || undefined,
        status: filterStatus.value || undefined,
      },
    })) as unknown as { list: NotificationLog[]; total: number }
    logs.value = res.list
    total.value = res.total
  } finally {
    loading.value = false
  }
}

// 筛选变化回第一页再查;翻页直接查。
watch([filterMonitor, filterChannel, filterEvent, filterStatus], () => {
  page.value = 1
  void load()
})
watch(page, () => void load())

onMounted(async () => {
  void load()
  // 下拉数据源失败不阻塞列表本身(拦截器已提示)。/monitors 比总览轻:不带统计。
  try {
    const list = (await http.get('/monitors')) as unknown as MonitorOption[]
    monitors.value = list.map((m) => ({ id: m.id, name: m.name }))
  } catch { /* 下拉保持为空 */ }
  try {
    channels.value = (await http.get('/channels')) as unknown as { id: string; name: string }[]
  } catch { /* 下拉保持为空 */ }
})

// 事件标签颜色:DOWN/AGENT_DOWN 红、UP/AGENT_UP 绿、TEST 灰。
function eventTagType(event: string): 'danger' | 'success' | 'info' {
  if (event === 'DOWN' || event === 'AGENT_DOWN') return 'danger'
  if (event === 'UP' || event === 'AGENT_UP') return 'success'
  return 'info'
}

// 发送状态标签:success 绿、failed 红。
function statusTagType(status: string): 'success' | 'danger' {
  return status === 'success' ? 'success' : 'danger'
}

// 明细弹窗:点行展开标题与正文(正文多行,pre-wrap 保留排版)。
const detail = ref<NotificationLog | null>(null)

// 尝试次数说明:1 次成功/失败不提示,重试过的给出「共 N 次」。
function attemptsText(row: NotificationLog): string {
  if (row.attempts <= 1) return ''
  return t('notifications.attemptsN', { n: row.attempts })
}

const clearFilters = () => {
  filterMonitor.value = ''
  filterChannel.value = ''
  filterEvent.value = ''
  filterStatus.value = ''
}
const hasFilter = computed(
  () => !!(filterMonitor.value || filterChannel.value || filterEvent.value || filterStatus.value),
)
</script>

<template>
  <div>
    <div class="page-head">
      <h3>{{ t('notifications.title') }}</h3>
      <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
    </div>

    <el-alert type="info" :closable="false" class="retention-hint">
      {{ t('notifications.retentionHint') }}
    </el-alert>

    <div class="filters">
      <el-select
        v-model="filterMonitor"
        :placeholder="t('notifications.filterMonitor')"
        clearable filterable
        style="width: 200px"
      >
        <el-option v-for="m in monitors" :key="m.id" :value="m.id" :label="m.name" />
      </el-select>
      <el-select
        v-model="filterChannel"
        :placeholder="t('notifications.filterChannel')"
        clearable filterable
        style="width: 200px"
      >
        <el-option v-for="c in channels" :key="c.id" :value="c.id" :label="c.name" />
      </el-select>
      <el-select
        v-model="filterEvent"
        :placeholder="t('notifications.filterEvent')"
        clearable
        style="width: 140px"
      >
        <el-option v-for="o in eventOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
      <el-select
        v-model="filterStatus"
        :placeholder="t('notifications.filterStatus')"
        clearable
        style="width: 140px"
      >
        <el-option v-for="o in statusOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
      <el-button v-if="hasFilter" link type="primary" @click="clearFilters">
        {{ t('notifications.clearFilters') }}
      </el-button>
    </div>

    <el-table :data="logs" v-loading="loading" :empty-text="t('notifications.empty')">
      <el-table-column prop="createdAt" :label="t('notifications.columnTime')" width="170" show-overflow-tooltip />
      <el-table-column :label="t('notifications.columnEvent')" width="110">
        <template #default="{ row }">
          <el-tag size="small" :type="eventTagType(row.event)">{{ row.event }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="monitorName" :label="t('notifications.columnMonitor')" min-width="140" show-overflow-tooltip />
      <el-table-column prop="channelName" :label="t('notifications.columnChannel')" min-width="120" show-overflow-tooltip />
      <el-table-column prop="title" :label="t('notifications.columnTitle')" min-width="200" show-overflow-tooltip />
      <el-table-column :label="t('notifications.columnStatus')" width="170">
        <template #default="{ row }">
          <el-tag size="small" :type="statusTagType(row.status)">
            {{ t(row.status === 'success' ? 'notifications.status.success' : 'notifications.status.failed') }}
          </el-tag>
          <span v-if="attemptsText(row)" class="attempts">{{ attemptsText(row) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('notifications.columnActions')" width="80" fixed="right">
        <template #default="{ row }">
          <el-button size="small" link type="primary" @click="detail = row">
            {{ t('notifications.viewDetail') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <div class="pager">
      <el-pagination
        v-model:current-page="page"
        :page-size="pageSize"
        :total="total"
        layout="total, prev, pager, next"
        :background="true"
      />
    </div>

    <el-dialog
      :model-value="detail !== null"
      :title="t('notifications.detailTitle')"
      width="640px"
      @update:model-value="detail = null"
    >
      <template v-if="detail">
        <div class="detail-meta">
          <el-tag size="small" :type="eventTagType(detail.event)">{{ detail.event }}</el-tag>
          <el-tag size="small" :type="statusTagType(detail.status)">
            {{ t(detail.status === 'success' ? 'notifications.status.success' : 'notifications.status.failed') }}
          </el-tag>
          <span v-if="attemptsText(detail)" class="attempts">{{ attemptsText(detail) }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('notifications.columnTime') }}</span>
          <span>{{ detail.createdAt }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('notifications.columnMonitor') }}</span>
          <span>{{ detail.monitorName || t('common.none') }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('notifications.columnChannel') }}</span>
          <span>{{ detail.channelName || t('common.none') }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('notifications.detailContentTitle') }}</span>
          <span>{{ detail.title || t('common.none') }}</span>
        </div>
        <div v-if="detail.error" class="detail-row">
          <span class="detail-label">{{ t('notifications.detailError') }}</span>
          <span class="detail-error">{{ detail.error }}</span>
        </div>
        <pre class="detail-content">{{ detail.content || t('common.none') }}</pre>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* 列表单行省略(与渠道页同一套做法),宽度不足时靠 tooltip 看全。 */
.el-table :deep(.cell) {
  white-space: nowrap;
}
.page-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.retention-hint {
  margin-bottom: 12px;
}
.filters {
  display: flex;
  gap: 10px;
  align-items: center;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.attempts {
  color: #909399;
  font-size: 12px;
  margin-left: 6px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 14px;
}
.detail-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.detail-row {
  display: flex;
  gap: 12px;
  margin-bottom: 8px;
  font-size: 13px;
  line-height: 1.6;
}
.detail-label {
  flex: none;
  width: 80px;
  color: #909399;
}
.detail-error {
  color: #f56c6c;
  word-break: break-all;
}
.detail-content {
  margin: 8px 0 0;
  padding: 10px 12px;
  background: #f7f8fa;
  border: 1px solid #ebeef5;
  border-radius: 4px;
  font-size: 12px;
  font-family: inherit;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 300px;
  overflow: auto;
}
</style>
