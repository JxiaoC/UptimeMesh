<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { http } from '../api/http'
import { confirmBox } from '../utils/confirm'

interface Channel {
  id: string
  name: string
  url: string
  bodyTemplate: string
  /** 启用状态:false = 禁用,不接收自动告警/恢复通知(测试发送不受限)。 */
  enabled: boolean
  createdAt: string
}

// 变量说明:与后端 notifytmpl.BodyPlaceholders 对应,点击可插入模板。
interface VarInfo { name: string; desc: string }

const { t } = useI18n()

// 变量名是接口契约,不翻译;说明按当前语言给,故放 computed(模块级常量只求值一次)。
const vars = computed<VarInfo[]>(() => [
  { name: 'title', desc: t('channels.vars.title') },
  { name: 'content', desc: t('channels.vars.content') },
  { name: 'monitorName', desc: t('channels.vars.monitorName') },
  { name: 'monitorId', desc: t('channels.vars.monitorId') },
  { name: 'monitorType', desc: t('channels.vars.monitorType') },
  { name: 'url', desc: t('channels.vars.url') },
  { name: 'event', desc: t('channels.vars.event') },
  { name: 'successRate', desc: t('channels.vars.successRate') },
  { name: 'speed', desc: t('channels.vars.speed') },
  { name: 'agents', desc: t('channels.vars.agents') },
  { name: 'errorCount', desc: t('channels.vars.errorCount') },
  { name: 'duration', desc: t('channels.vars.duration') },
  { name: 'timestamp', desc: t('channels.vars.timestamp') },
])

const channels = ref<Channel[]>([])
const loading = ref(false)
// 正在切换启用状态的行 id(开关转圈用):用集合而不是单值 —— 两个管理员可能同时
// 切不同行,单值会让先点的那行转圈提前停掉。
const toggling = ref(new Set<string>())
const dialog = reactive({
  visible: false, editing: null as Channel | null,
  name: '', url: '', bodyTemplate: '', enabled: true,
})

// 括号 token 在脚本里拼好:模板中直接写 {{ 会破坏 Vue 解析。
const varToken = (name: string) => `{{${name}}}`
// 请求体模板示例带花括号,不能写进词条(会破坏 vue-i18n 插值解析),当参数传。
const bodyTemplateExample = '{"msgtype":"markdown","markdown":{"title":"{{title}}","text":"{{content}}"}}'
// 对话框内相对固定的提示样例:展示变量替换后的取值(示例文案按语言给)。
const samples = computed<Record<string, string>>(() => ({
  title: t('channels.samples.title'),
  content: t('channels.samples.content'),
  monitorName: t('channels.samples.monitorName'),
  monitorId: '6aa3eaea721b7851dac0d93e',
  monitorType: 'HTTP',
  url: 'https://example.com/health',
  event: 'DOWN',
  successRate: '42.5',
  speed: '12.34 MB/s',
  agents: t('channels.samples.agents'),
  errorCount: '3',
  duration: t('channels.samples.duration'),
  timestamp: '2026-09-12 10:00:00',
}))

async function load() {
  loading.value = true
  try {
    channels.value = (await http.get('/channels')) as unknown as Channel[]
  } finally { loading.value = false }
}
onMounted(load)

function openCreate() {
  Object.assign(dialog, {
    visible: true, editing: null, name: '', url: '', bodyTemplate: '', enabled: true,
  })
}
function openEdit(c: Channel) {
  Object.assign(dialog, {
    visible: true, editing: c, name: c.name, url: c.url, bodyTemplate: c.bodyTemplate || '',
    enabled: c.enabled,
  })
}

// 记录最后聚焦的输入框,点击变量时插入到该处。
const focused = ref<'url' | 'bodyTemplate'>('bodyTemplate')
function insertVar(name: string) {
  if (focused.value === 'url') dialog.url += `{{${name}}}`
  else dialog.bodyTemplate += `{{${name}}}`
}

// 模板渲染预览:替换已知变量,未知的原样保留(与后端行为一致)。
const preview = computed(() => {
  const text = dialog.bodyTemplate.trim()
  if (!text) return ''
  return text.replace(/\{\{(\w+)\}\}/g, (m, name: string) =>
    name in samples.value ? JSON.stringify(samples.value[name]).slice(1, -1) : m,
  )
})
// 预览是否合法 JSON,给用户即时反馈(后端也会校验)。
const previewValid = computed(() => {
  if (!preview.value) return true
  try { JSON.parse(preview.value); return true } catch { return false }
})

async function save() {
  if (!dialog.name.trim() || !/^https?:\/\/.+/.test(dialog.url.trim())) {
    ElMessage.warning(t('channels.invalidForm'))
    return
  }
  const body = {
    name: dialog.name.trim(),
    url: dialog.url.trim(),
    bodyTemplate: dialog.bodyTemplate.trim(),
    enabled: dialog.enabled,
  }
  try {
    if (dialog.editing) await http.put(`/channels/${dialog.editing.id}`, body)
    else await http.post('/channels', body)
  } catch { return /* 拦截器已提示 */ }
  ElMessage.success(t('common.saved'))
  dialog.visible = false
  await load()
}

// toggleEnabled 列表行上的启用开关。
//
// 刻意**先乐观改本地状态再发请求**:el-switch 的勾选态完全由 model-value 决定,
// 若只在成功后赋值,失败时 prop 从未变过、组件不会重渲染 —— 开关会停在用户点出来的
// 那个位置,与真实状态相反。先改再在失败时改回来,两种结局都有一次真实的 prop 变化。
// (请求期间开关处于 loading,Element Plus 自己会挡掉重复点击。)
// el-switch 的 change 参数在配了 active-value 时可能是 string/number,这里按布尔收口。
async function toggleEnabled(c: Channel, value: boolean | string | number) {
  const enabled = value === true
  const before = c.enabled
  c.enabled = enabled
  toggling.value.add(c.id)
  try {
    await http.post(`/channels/${c.id}/${enabled ? 'enable' : 'disable'}`)
    ElMessage.success(t(enabled ? 'channels.enabledOn' : 'channels.enabledOff', { name: c.name }))
  } catch {
    c.enabled = before // 拦截器已提示原因;把开关拨回真实状态
  } finally {
    toggling.value.delete(c.id)
  }
}

// rowClassName 禁用行整行压暗:一眼看出哪些渠道现在不投递。
function rowClassName({ row }: { row: Channel }) {
  return row.enabled ? '' : 'row-disabled'
}
async function test(c: Channel) {
  const msg = ElMessage({ message: t('channels.testing', { name: c.name }), duration: 0 })
  try {
    await http.post(`/channels/${c.id}/test`)
    msg.close()
    ElMessage.success(t('channels.testSent'))
  } catch {
    msg.close()
  }
}
async function remove(c: Channel) {
  await confirmBox(
    t('channels.deleteConfirm', { name: c.name }),
    t('channels.deleteConfirmTitle'),
    { type: 'warning' },
  )
  await http.delete(`/channels/${c.id}`)
  ElMessage.success(t('common.deleted'))
  await load()
}

// applyToAll 把该渠道加到所有监控的通知渠道里。语义是**追加**:已包含的跳过、各监控原有
// 的其它渠道不动 —— 二次确认里写明这一点,免得被当成"只留这一个渠道"。
async function applyToAll(c: Channel) {
  try {
    await confirmBox(
      t('channels.applyToAllConfirm', { name: c.name }),
      t('channels.applyToAllConfirmTitle'),
      { type: 'info', confirmButtonText: t('channels.applyToAllConfirmBtn') },
    )
  } catch { return /* 用户取消 */ }
  try {
    const res = (await http.post(`/channels/${c.id}/apply-all`)) as unknown as
      { affected: number; total: number; already: number }
    if (!res.total) ElMessage.warning(t('channels.applyToAllNoMonitors'))
    else if (!res.affected) ElMessage.info(t('channels.applyToAllAlready', { total: res.total }))
    else ElMessage.success(
      t('channels.applyToAllDone', { affected: res.affected, name: c.name })
      + (res.already ? t('channels.applyToAllAlso', { already: res.already }) : ''),
    )
  } catch { /* 拦截器已提示 */ }
}
</script>

<template>
  <div>
    <div style="display:flex;justify-content:space-between;align-items:center">
      <h3>{{ t('nav.channels') }}</h3>
      <div>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" @click="openCreate">{{ t('channels.newChannel') }}</el-button>
      </div>
    </div>
    <el-table
      class="channels-table"
      :data="channels" v-loading="loading" :empty-text="t('channels.empty')"
      :row-class-name="rowClassName"
    >
      <el-table-column prop="name" :label="t('channels.columnName')" min-width="140" show-overflow-tooltip />
      <el-table-column prop="url" label="Webhook URL" min-width="260" show-overflow-tooltip />
      <el-table-column :label="t('channels.columnEnabled')" width="90">
        <template #default="{ row }">
          <el-switch
            :model-value="row.enabled"
            :loading="toggling.has(row.id)"
            @change="(v: boolean | string | number) => toggleEnabled(row, v)"
          />
        </template>
      </el-table-column>
      <!-- 请求体列:标签文字最宽的是英文 "Custom template" / "Default JSON",
           150px 是让它在标签内不被截断所需的最小宽度(按中英文里更宽的那个配足)。 -->
      <el-table-column :label="t('channels.columnBody')" width="150" show-overflow-tooltip>
        <template #default="{ row }">
          <el-tag size="small" :type="row.bodyTemplate ? 'warning' : 'info'">
            {{ row.bodyTemplate ? t('channels.customTemplate') : t('channels.defaultJson') }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="createdAt" :label="t('channels.columnCreatedAt')" width="170" show-overflow-tooltip />
      <!-- 操作列宽度按「中文与英文里更宽的那个」配足:英文按钮 "Set as channel for all
           monitors" 比中文长得多,列窄了最后一个按钮会被 .cell 的 overflow:hidden 切掉
           (整行单行显示之后不再靠按钮内折行来兜底)。 -->
      <el-table-column :label="t('channels.columnActions')" width="410" fixed="right">
        <template #default="{ row }">
          <div class="row-actions">
            <el-button size="small" @click="test(row)">{{ t('channels.testSend') }}</el-button>
            <el-button size="small" @click="openEdit(row)">{{ t('common.edit') }}</el-button>
            <el-button size="small" @click="applyToAll(row)">{{ t('channels.applyToAll') }}</el-button>
            <el-button size="small" type="danger" plain @click="remove(row)">{{ t('common.delete') }}</el-button>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dialog.visible" :title="dialog.editing ? t('channels.editTitle') : t('channels.createTitle')" width="680px">
      <el-form label-width="110px">
        <el-form-item :label="t('channels.name')" required>
          <el-input v-model="dialog.name" :placeholder="t('channels.namePlaceholder')" />
        </el-form-item>
        <el-form-item :label="t('channels.enabled')">
          <el-switch v-model="dialog.enabled" />
          <span class="enabled-hint">{{ t('channels.enabledHint') }}</span>
        </el-form-item>
        <el-form-item label="Webhook URL" required>
          <el-input
            v-model="dialog.url"
            placeholder="https://oapi.dingtalk.com/robot/send?access_token=…"
            @focus="focused = 'url'"
          />
        </el-form-item>
        <el-form-item :label="t('channels.bodyTemplate')">
          <el-input
            v-model="dialog.bodyTemplate"
            type="textarea" :rows="6"
            :placeholder="t('channels.bodyPlaceholder', { example: bodyTemplateExample })"
            @focus="focused = 'bodyTemplate'"
          />
        </el-form-item>
      </el-form>

      <el-alert type="info" :closable="false" class="vars-help">
        <template #title>
          {{ t('channels.varsHelpTitle', {
            target: focused === 'url' ? 'Webhook URL' : t('channels.bodyTemplate'),
          }) }}
        </template>
        <div class="vars-list">
          <el-tooltip
            v-for="v in vars" :key="v.name"
            :content="t('channels.varTooltip', { desc: v.desc, sample: samples[v.name] })"
            placement="top"
          >
            <el-tag size="small" class="var-tag" @click="insertVar(v.name)">
              {{ varToken(v.name) }}
            </el-tag>
          </el-tooltip>
        </div>
        <div class="vars-note">
          {{ t('channels.varsNote', { token: varToken('successRate') }) }}
        </div>
      </el-alert>

      <div v-if="dialog.bodyTemplate.trim()" class="preview">
        <div class="preview-hd">
          {{ t('channels.previewTitle') }}
          <span v-if="!previewValid" class="preview-bad">{{ t('channels.invalidJson') }}</span>
          <span v-else class="preview-ok">{{ t('channels.validJson') }}</span>
        </div>
        <pre class="preview-body">{{ preview }}</pre>
      </div>
      <p v-if="!dialog.bodyTemplate.trim()" class="tip">
        {{ t('channels.defaultPayloadTip') }}
      </p>

      <template #footer>
        <el-button @click="dialog.visible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :disabled="!previewValid" @click="save">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* 渠道列表:所有列一律单行 —— 只要有一列折行,整行行高就被撑起来(英文文案比中文长,
   尤其明显)。放不下的内容由 .cell 自带的 overflow:hidden + text-overflow:ellipsis 省略,
   名称/URL/请求体再配 show-overflow-tooltip 悬停看全(与监控、节点列表同一套做法)。 */
.channels-table :deep(.cell) {
  white-space: nowrap;
}
/* 请求体的标签放不下时,让省略号出现在标签内部 —— el-tag 是 inline-flex,
   只靠 .cell 的 text-overflow 会被硬切掉半截文字(英文 "Custom template" 就会)。 */
.channels-table :deep(.cell .el-tag) {
  max-width: 100%;
}
.channels-table :deep(.cell .el-tag .el-tag__content) {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* 操作列固定单行排列:宽度不足时按钮会换行错位。 */
.row-actions {
  display: flex;
  align-items: center;
  flex-wrap: nowrap;
}
:deep(.row-actions .el-button + .el-button) { margin-left: 8px; }
/* 禁用行整行压暗(与开关列一起表达"这个渠道现在不投递")。 */
:deep(.row-disabled) { color: #a8abb2; }
:deep(.row-disabled td) { opacity: 0.75; }
.enabled-hint { color: #909399; font-size: 12px; margin-left: 10px; line-height: 1.5; }
.vars-help { margin: 0 0 4px; }
.vars-list { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; }
.var-tag {
  cursor: pointer;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.vars-note { color: #909399; font-size: 12px; margin-top: 8px; line-height: 1.5; }
.preview {
  background: #f7f8fa;
  border: 1px solid #ebeef5;
  border-radius: 4px;
  padding: 10px 12px;
  margin-bottom: 12px;
}
.preview-hd { color: #909399; font-size: 12px; margin-bottom: 6px; }
.preview-ok { color: #67c23a; margin-left: 8px; }
.preview-bad { color: #f56c6c; margin-left: 8px; }
.preview-body {
  margin: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 180px;
  overflow: auto;
}
.tip { color: #909399; font-size: 12px; margin: 0 0 0 100px; line-height: 1.5; }
</style>
