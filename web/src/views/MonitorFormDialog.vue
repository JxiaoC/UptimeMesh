<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { http } from '../api/http'
import { DEFAULT_SPEED_UNIT, SPEED_UNITS, formatSpeed, type SpeedUnit } from '../utils/speed'

export interface Agent {
  id: string
  name: string
  status: string
  online: boolean
  /** 节点自报的本机网络族可用性;null/缺省 = 老版本节点未上报(未知)。 */
  ipv4Available?: boolean | null
  ipv6Available?: boolean | null
}

const props = defineProps<{
  modelValue: boolean
  editing: any | null   // null=新建;传入对象=编辑
  agents: Agent[]
  channels: { id: string; name: string; enabled?: boolean }[]  // 票 07 前有缓存但为空列表
  groups: string[]  // 已有分组,供下拉选择/新建
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
  (e: 'saved'): void
}>()

const { t } = useI18n()

const defaultForm = () => ({
  type: 'http', name: '', group: '', period: 60, timeout: 10,
  threshold: 100, consecutive: 3,
  url: '', method: 'GET', headersText: '', body: '',
  expectStatusSpecs: ['200'] as string[],
  containsText: '', notContainsText: '',
  allowInsecureTLS: false, targetHost: '', port: 6379,
  invertMode: false,
  // IP 协议族:auto = 交给系统(默认),ipv4 / ipv6 = 强制只走该族。
  ipVersion: 'auto' as 'auto' | 'ipv4' | 'ipv6',
  // 下载速度监控:阈值单位(KB/s | MB/s);阈值字段与 HTTP 共用 form.threshold,
  // 只是解释方式由单位决定(见 utils/speed.ts 与 .scratch/download-speed-monitor/spec.md)。
  speedUnit: DEFAULT_SPEED_UNIT as SpeedUnit,
  // JSON 查询断言(与 UptimeKuma 的 "HTTP(s) - JSON 查询" 同语义)。
  jsonPath: '', jsonPathOperator: '==', jsonAssertExpected: '',
  // push(外部上报)监控的上报地址;新建时后端生成,这里只做回显与复制。
  pushToken: '', pushUrl: '',
  assignMode: 'all' as 'selected' | 'all' | 'exclude',
  excludedAgentIds: [] as string[],
  assignedAgentIds: [] as string[], channelIds: [] as string[],
})
const form = ref(defaultForm())
const saving = ref(false)
const testing = ref(false)
// 上一次的类型:类型切换时把"阈值"的默认值按新口径重置(100% ⇄ 10 MB/s),
// 免得一个"100%"被静默当成"100 MB/s"。只由用户点击单选触发,编辑回显不动它。
const lastType = ref('http')
// testResult 是最近一次「测试」的结果:null 表示还没测过(面板不显示)。
// 失败时自动展开请求/返回明细 —— 那正是用户点测试的原因。
const testResult = ref<TestResult | null>(null)
const testDetailOpen = ref(false)
const statusInput = ref('')

// 请求头示例的值里带花括号,不能写进词条(会破坏 vue-i18n 的插值解析),所以当参数传。
const headersExample = '{"Authorization":"Bearer x"}'

// TestResult 是 POST /monitors/test 的返回:判定 + 请求/响应明细。
// ok 是有效判定(反转模式下已取反),probeOk 是节点的原始判定。
interface TestResult {
  agentId: string
  agentName: string
  type: string
  invertMode: boolean
  ok: boolean
  probeOk: boolean
  latencyMs: number
  httpStatus?: number
  error?: string
  // 下载速度监控:测得的平均速度(KB/s,统一单位)与字节数;speedUnit 是监控配置的单位。
  speedKbps?: number
  bytes?: number
  speedUnit?: string
  request?: {
    method?: string
    url?: string
    headers?: Record<string, string>
    body?: string
    target?: string
  }
  response?: {
    status?: number
    statusText?: string
    headers?: Record<string, string>
    bodyExcerpt?: string
    bodyBytes?: number
    bodyTruncated?: boolean
    bodyNote?: string
    finalUrl?: string
    redirects?: string[]
  }
}

// JSON 断言的比较方式(与后端 checkconfig.JSONAssertOperators 一致)。
// 文案放 computed:模块级常量只求值一次,切语言后下拉不会跟着变。
const jsonOperators = computed(() => [
  { value: '==', label: t('monitorForm.operators.eq') },
  { value: '!=', label: t('monitorForm.operators.neq') },
  { value: 'contains', label: t('monitorForm.operators.contains') },
  { value: '>', label: t('monitorForm.operators.gt') },
  { value: '>=', label: t('monitorForm.operators.gte') },
  { value: '<', label: t('monitorForm.operators.lt') },
  { value: '<=', label: t('monitorForm.operators.lte') },
])

function open() {
  form.value = defaultForm()
  // 上一次的测试结果属于上一次打开的表单:重新打开时清掉,免得张冠李戴。
  testResult.value = null
  testDetailOpen.value = false
  if (props.editing) {
    const m = props.editing
    form.value = {
      ...form.value,
      type: m.type, name: m.name, group: m.group || '', period: m.period, timeout: m.timeout,
      threshold: m.threshold, consecutive: m.consecutive ?? 3,
      url: m.url || '', method: m.method || 'GET',
      headersText: m.headers ? JSON.stringify(m.headers, null, 2) : '',
      body: m.body || '',
      expectStatusSpecs: initialSpecs(m),
      containsText: (m.expectContains || []).join('\n'),
      notContainsText: (m.expectNotContains || []).join('\n'),
      allowInsecureTLS: !!m.allowInsecureTLS, targetHost: m.targetHost || '',
      port: m.port || 6379,
      ipVersion: (m.ipVersion || 'auto') as 'auto' | 'ipv4' | 'ipv6',
      speedUnit: (m.speedUnit || DEFAULT_SPEED_UNIT) as SpeedUnit,
      pushToken: m.pushToken || '', pushUrl: m.pushUrl || '',
      invertMode: !!m.invertMode,
      jsonPath: m.jsonPath || '',
      jsonPathOperator: m.jsonPathOperator || '==',
      jsonAssertExpected: m.jsonAssertExpected || '',
      assignMode: (m.assignMode || 'selected') as 'selected' | 'all' | 'exclude',
      excludedAgentIds: m.excludedAgentIds || [],
      assignedAgentIds: m.assignedAgentIds || [], channelIds: m.channelIds || [],
    }
  }
  lastType.value = form.value.type
}

// onTypeChange 用户切换监控类型:阈值的含义随之改变(百分比 ⇄ 速度),
// 因此按新口径给一个默认值,而不是把旧数字原样留在输入框里。
function onTypeChange(next: string) {
  const was = lastType.value
  lastType.value = next
  if (next === 'download' && was !== 'download') {
    form.value.threshold = 10
    form.value.speedUnit = 'MB/s'
  } else if (next !== 'download' && was === 'download') {
    form.value.threshold = 100
    form.value.speedUnit = DEFAULT_SPEED_UNIT
  }
}

// pushThreshold 是外部上报监控恒定的阈值(与后端 store.PushThreshold 同值):
// 这类监控没有可调的可用率阈值,载荷里固定回它。
const pushThreshold = 100

// isDownload 下载速度监控:请求侧与 HTTP 相同,但阈值是速度、没有内容类断言与反转模式。
const isDownload = computed(() => form.value.type === 'download')
// isPush 外部上报:没有探测目标,也没有可用率阈值(每轮只有一个样本,成功率非 0% 即
// 100%,填多少判定都一样;填 0 还会变成"永不告警")。表单因此不给阈值这一项,
// 连续性字段的文案也换成外部上报的口径(未上报/上报故障)。
const isPush = computed(() => form.value.type === 'push')
// 速度单位是代码常量,不需要翻译;列表只影响下拉顺序与取值。
const speedUnits = SPEED_UNITS

// ipSupport 统计已批准节点自报的网络能力:选了 IPv6 却没有任何节点支持时,这条监控
// 会在所有节点上稳定失败 —— 必须在保存前就说清楚,而不是等告警响了再排查。
// 未上报的节点(老版本 Agent)单独计数:它们不代表"不支持",而是"不知道"。
const ipSupport = computed(() => {
  let ipv4 = 0, ipv6 = 0, unknown = 0
  for (const a of props.agents) {
    if (a.status !== 'approved') continue
    if (a.ipv4Available == null && a.ipv6Available == null) {
      unknown++
      continue
    }
    if (a.ipv4Available) ipv4++
    if (a.ipv6Available) ipv6++
  }
  return { ipv4, ipv6, unknown }
})

// ipVersionHint 提示当前选择的协议族在本节点群里的可行性(含"未知"节点数量)。
const ipVersionHint = computed(() => {
  const { ipv4, ipv6, unknown } = ipSupport.value
  const base = t('monitorForm.ipVersionNodes', { ipv4, ipv6 })
    + (unknown > 0 ? ' ' + t('monitorForm.ipVersionUnknownNodes', { count: unknown }) : '')
  const chosen = form.value.ipVersion
  const none = (chosen === 'ipv4' && ipv4 === 0) || (chosen === 'ipv6' && ipv6 === 0)
  if (!none) return { text: base, warn: false }
  return {
    text: base + ' ' + t('monitorForm.ipVersionUnsupported', {
      family: chosen === 'ipv4' ? 'IPv4' : 'IPv6',
    }),
    warn: true,
  }
})

function lines(s: string): string[] {
  return s.split('\n').map((x) => x.trim()).filter(Boolean)
}

// pushFullUrl 上报地址的完整形态:后端回的是相对路径(/api/push/<token>),
// 这里补上当前站点 origin,便于直接粘贴到路由器脚本里。
const pushFullUrl = computed(() => {
  if (!form.value.pushUrl) return ''
  return window.location.origin + form.value.pushUrl
})

// copyPushUrl 复制完整上报地址(浏览器不支持剪贴板 API 时提示用户手动复制)。
async function copyPushUrl() {
  try {
    await navigator.clipboard.writeText(pushFullUrl.value)
    ElMessage.success(t('common.pushUrlCopied'))
  } catch {
    ElMessage.warning(t('common.copyFailedManual'))
  }
}

const CODE_MIN = 100
const CODE_MAX = 599

// initialSpecs 回显期望状态码:优先编辑态 spec,其次展开码,再默认 200。
function initialSpecs(m: any): string[] {
  const specs: string[] = m.expectStatusSpecs?.length
    ? [...m.expectStatusSpecs]
    : (m.expectStatusCodes || []).map((c: number) => String(c))
  return specs.length ? specs : ['200']
}

// normalizeSpec 归一化单个输入:单码 "200" 或区间 "400~499"(也接受连字符)。
function normalizeSpec(raw: string): { spec?: string; error?: string } {
  const s = raw.trim().replace(/[～]/g, '~').replace(/[－—]/g, '-').replace(/\s+/g, '')
  if (!s) return { error: t('monitorForm.errors.statusEmpty') }
  const range = s.match(/^(\d{1,3})[~-](\d{1,3})$/)
  if (range) {
    const lo = Number(range[1]), hi = Number(range[2])
    if (lo < CODE_MIN || lo > CODE_MAX || hi < CODE_MIN || hi > CODE_MAX)
      return { error: t('monitorForm.errors.statusOutOfRange') }
    if (lo > hi) return { error: t('monitorForm.errors.statusRangeOrder') }
    return { spec: lo === hi ? String(lo) : `${lo}~${hi}` }
  }
  if (!/^\d{1,3}$/.test(s)) {
    return { error: t('monitorForm.errors.statusInvalid', { raw: raw.trim() }) }
  }
  const n = Number(s)
  if (n < CODE_MIN || n > CODE_MAX) return { error: t('monitorForm.errors.statusOutOfRange') }
  return { spec: String(n) }
}

// addStatus 支持一次输入多个(逗号/空格分隔),逐个校验去重后落为卡片。
function addStatus() {
  const tokens = statusInput.value.split(/[,，\s]+/).map((token) => token.trim()).filter(Boolean)
  if (!tokens.length) return
  for (const token of tokens) {
    const { spec, error } = normalizeSpec(token)
    if (error) { ElMessage.error(error); return }
    if (spec && !form.value.expectStatusSpecs.includes(spec)) form.value.expectStatusSpecs.push(spec)
  }
  statusInput.value = ''
}

function removeStatus(i: number) {
  form.value.expectStatusSpecs.splice(i, 1)
}

// buildBody 把表单整理成接口载荷 —— 保存与测试共用一份:测试要测的必须是保存时
// 真正会下发的那份配置,两边各拼一次迟早会漂移。返回 null 表示表单本身不合法(已提示)。
function buildBody(): any | null {
  let headers: Record<string, string> | null = null
  if (form.value.headersText.trim()) {
    try { headers = JSON.parse(form.value.headersText) }
    catch {
      ElMessage.error(t('monitorForm.errors.headersInvalid', { example: headersExample }))
      return null
    }
  }
  const body: any = { ...form.value }
  delete body.headersText
  delete body.containsText
  delete body.notContainsText
  // 外部上报没有可用率阈值(见 isPush 的说明):表单不给这一项,载荷里固定回恒定值,
  // 免得把上一次切类型留下的数字(甚至 0)带进去 —— 后端同样会归一化,这是第二道。
  if (body.type === 'push') body.threshold = pushThreshold
  if (headers) body.headers = headers
  // 清空卡片时回落到默认 200,与后端默认值一致。
  body.expectStatusSpecs = form.value.expectStatusSpecs.length
    ? [...form.value.expectStatusSpecs]
    : ['200']
  body.expectContains = lines(form.value.containsText)
  body.expectNotContains = lines(form.value.notContainsText)
  return body
}

async function save() {
  const body = buildBody()
  if (!body) return
  saving.value = true
  try {
    if (props.editing) {
      await http.put(`/monitors/${props.editing.id}`, body)
    } else {
      await http.post('/monitors', body)
    }
    ElMessage.success(t('monitorForm.saved'))
    emit('update:modelValue', false)
    emit('saved')
  } catch { /* 拦截器已提示 */ } finally { saving.value = false }
}

// ---- 测试 ----
// 「测试」把当前表单(含未保存的改动)交给一个在线节点当场跑一次,只回结果不落库:
// 轮次、可用率与告警都不受影响(见 ADR-0002/0003 与 api.monitors_check.go)。
// 外部上报监控没有可探测的目标,按钮不给。
const canTest = computed(() => form.value.type !== 'push')

async function runTest() {
  const body = buildBody()
  if (!body) return
  testing.value = true
  testResult.value = null
  try {
    testResult.value = (await http.post('/monitors/test', body)) as unknown as TestResult
    // 失败时直接把请求与返回摊开;成功默认收起(需要时自己展开)。
    testDetailOpen.value = !testResult.value.ok
  } catch { /* 没有可用节点等原因:拦截器已提示 */ } finally { testing.value = false }
}

// testSpeedText 下载速度监控的测速展示文本(单位按监控配置换算;没有速度返回空串)。
const testSpeedText = computed(() =>
  formatSpeed(testResult.value?.speedKbps, testResult.value?.speedUnit || form.value.speedUnit),
)

const testAlertTitle = computed(() => {
  const r = testResult.value
  if (!r) return ''
  if (r.ok) {
    if (r.type === 'download') {
      return t('monitorForm.test.passedDownload', { speed: testSpeedText.value || '—' })
    }
    return r.httpStatus
      ? t('monitorForm.test.passedHttp', { code: r.httpStatus, latency: r.latencyMs })
      : t('monitorForm.test.passedPlain', { latency: r.latencyMs })
  }
  return r.error
    ? t('monitorForm.test.failed', { error: r.error })
    : t('monitorForm.test.failedNoReason')
})

// testMetaText 一行元信息:执行节点 + 耗时;下载速度监控额外给出测得的字节数
// (速度是"多大文件、用了多久"的结果,单看速度数字不好判断链路)。
const testMetaText = computed(() => {
  const r = testResult.value
  if (!r) return ''
  if (r.type === 'download') {
    return t('monitorForm.test.metaDownload', {
      name: r.agentName, bytes: r.bytes ?? 0, latency: r.latencyMs,
    })
  }
  return t('monitorForm.test.meta', { name: r.agentName, latency: r.latencyMs })
})

// testRequestText 请求明细排成人能读、能直接复制的文本(ping/tcp 只有目标)。
const testRequestText = computed(() => {
  const r = testResult.value?.request
  if (!r) return ''
  if (!r.url) return r.target ? t('monitorForm.test.requestTarget', { target: r.target }) : ''
  const out = [`${r.method || 'GET'} ${r.url}`]
  if (r.target) out.push(t('monitorForm.test.requestActualConn', { target: r.target }))
  const keys = Object.keys(r.headers || {}).sort()
  if (keys.length) {
    out.push(t('monitorForm.test.requestHeaders'))
    for (const k of keys) out.push(`  ${k}: ${r.headers?.[k]}`)
  }
  if (r.body) out.push('', t('monitorForm.test.requestBody'), r.body)
  return out.join('\n')
})

// testResponseText 返回明细:状态行 + 响应头 + 重定向链 + 响应体片段。
const testResponseText = computed(() => {
  const res = testResult.value?.response
  if (!res || !res.status) return ''
  const out = [`HTTP ${res.status}${res.statusText ? ' ' + res.statusText.replace(/^\d+\s*/, '') : ''}`]
  const keys = Object.keys(res.headers || {}).sort()
  if (keys.length) {
    out.push(t('monitorForm.test.responseHeaders'))
    for (const k of keys) out.push(`  ${k}: ${res.headers?.[k]}`)
  }
  if (res.redirects?.length) {
    out.push('', t('monitorForm.test.redirects', {
      count: res.redirects.length,
      url: res.finalUrl || '',
    }))
    for (const u of res.redirects) out.push(`  → ${u}`)
  }
  if (res.bodyExcerpt) {
    out.push('', t('monitorForm.test.bodyExcerpt', {
      bytes: res.bodyBytes ?? 0,
      truncated: res.bodyTruncated ? t('monitorForm.test.bodyTruncated') : '',
    }), res.bodyExcerpt)
  } else if (res.bodyNote || res.bodyBytes) {
    out.push('', res.bodyNote || t('monitorForm.test.bodyEmpty', { bytes: res.bodyBytes ?? 0 }))
  }
  return out.join('\n')
})

const hasTestDetail = computed(() => !!(testRequestText.value || testResponseText.value))

async function copyTestReport() {
  const r = testResult.value
  if (!r) return
  const out = [testAlertTitle.value, t('monitorForm.test.agentLine', { name: r.agentName })]
  if (r.invertMode) {
    out.push(t('monitorForm.test.invertProbe', {
      result: t(r.probeOk ? 'monitorForm.test.probeOk' : 'monitorForm.test.probeFail'),
    }))
  }
  if (testRequestText.value) out.push('', t('monitorForm.test.reportRequest'), testRequestText.value)
  if (testResponseText.value) out.push('', t('monitorForm.test.reportResponse'), testResponseText.value)
  try {
    await navigator.clipboard.writeText(out.join('\n'))
    ElMessage.success(t('monitorForm.test.copied'))
  } catch {
    ElMessage.warning(t('monitorForm.test.copyFailedManual'))
  }
}

defineExpose({ open })
</script>

<template>
  <el-dialog :model-value="modelValue"
    :title="editing ? t('monitorForm.editTitle') : t('monitorForm.createTitle')" width="640px"
    @update:model-value="emit('update:modelValue', $event)"
    @open="open">
    <!-- label 宽度交给 el-form 自适应:术语改叫「连续低于阈值轮数」后,固定 110px 装不下,
         之前「可用率阈值(%)」也是差几个像素就折行。自适应后所有 label 一次排齐、都不折行;
         代价是 label 列比原来的 110px 略宽,push 上报地址那一行的输入框相应收窄 40px。 -->
    <el-form :model="form" label-width="auto">
      <el-form-item :label="t('monitorForm.type')">
        <el-radio-group v-model="form.type" @change="onTypeChange">
          <el-radio-button value="http">HTTP(S)</el-radio-button>
          <el-radio-button value="ping">PING</el-radio-button>
          <el-radio-button value="tcp">{{ t('type.tcp') }}</el-radio-button>
          <el-radio-button value="push">{{ t('type.push') }}</el-radio-button>
          <el-radio-button value="download">{{ t('type.download') }}</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item v-if="form.type === 'push'" :label="t('monitorForm.pushUrlLabel')">
        <template v-if="editing">
          <el-input :model-value="pushFullUrl" readonly style="width:380px" />
          <el-button style="margin-left:8px" @click="copyPushUrl">{{ t('common.copy') }}</el-button>
          <!-- 段落里混着 <code> 示例与动态值,故走 i18n-t 的具名插槽(词条里只留 {slot}) -->
          <i18n-t keypath="monitorForm.pushTip" tag="div" scope="global" class="tip" style="display:block">
            <template #statusCode><code>status=up|down</code></template>
            <template #msg><code>{{ t('common.pushParamMsg') }}</code></template>
            <template #ping><code>{{ t('common.pushParamPing') }}</code></template>
            <template #consecutive>{{ form.consecutive }}</template>
            <template #lastPush>{{ editing.lastPushAt || t('common.neverReported') }}</template>
          </i18n-t>
        </template>
        <div v-else class="tip" style="display:block">
          {{ t('monitorForm.pushNewTip') }}
        </div>
      </el-form-item>
      <el-form-item :label="t('monitorForm.name')" required>
        <el-input v-model="form.name" :placeholder="t('monitorForm.namePlaceholder')" />
      </el-form-item>
      <el-form-item :label="t('monitorForm.group')">
        <el-select v-model="form.group" filterable allow-create default-first-option clearable
          style="width: 260px" :placeholder="t('monitorForm.groupPlaceholder')">
          <el-option v-for="g in props.groups" :key="g" :value="g" :label="g" />
        </el-select>
        <span class="tip">{{ t('monitorForm.groupTip') }}</span>
      </el-form-item>

      <template v-if="form.type === 'http'">
        <el-form-item label="URL" required>
          <el-input v-model="form.url" placeholder="https://example.com/health" />
        </el-form-item>
        <el-form-item :label="t('monitorForm.method')">
          <el-select v-model="form.method" style="width:120px">
            <el-option v-for="m in ['GET','POST','PUT','PATCH','DELETE','HEAD']" :key="m" :value="m" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('monitorForm.headers')">
          <el-input
            v-model="form.headersText"
            type="textarea"
            :rows="2"
            :placeholder="t('monitorForm.headersPlaceholder', { example: headersExample })"
          />
        </el-form-item>
        <el-form-item v-if="['POST','PUT','PATCH'].includes(form.method)" :label="t('monitorForm.body')">
          <el-input v-model="form.body" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item :label="t('monitorForm.expectStatus')">
          <div class="code-editor">
            <div class="code-cards">
              <el-tag
                v-for="(spec, i) in form.expectStatusSpecs"
                :key="spec"
                class="code-card"
                closable
                disable-transitions
                @close="removeStatus(i)"
              >
                {{ spec }}
              </el-tag>
              <span v-if="!form.expectStatusSpecs.length" class="code-empty">
                {{ t('monitorForm.expectStatusEmpty') }}
              </span>
            </div>
            <div class="code-add">
              <el-input
                v-model="statusInput"
                style="width: 220px"
                :placeholder="t('monitorForm.statusInputPlaceholder')"
                @keyup.enter="addStatus"
              />
              <el-button @click="addStatus">{{ t('common.add') }}</el-button>
              <span class="tip">{{ t('monitorForm.statusRangeTip') }}</span>
            </div>
          </div>
        </el-form-item>
        <el-form-item :label="t('monitorForm.containsText')">
          <el-input v-model="form.containsText" type="textarea" :rows="2" :placeholder="t('monitorForm.containsTextPlaceholder')" />
        </el-form-item>
        <el-form-item :label="t('monitorForm.notContainsText')">
          <el-input v-model="form.notContainsText" type="textarea" :rows="2" :placeholder="t('monitorForm.notContainsTextPlaceholder')" />
        </el-form-item>
        <el-form-item :label="t('monitorForm.jsonAssert')">
          <div class="json-assert">
            <el-input
              v-model="form.jsonPath"
              :placeholder="t('monitorForm.jsonPathPlaceholder')"
            />
            <div class="json-assert-row">
              <el-select v-model="form.jsonPathOperator" style="width: 120px">
                <el-option v-for="op in jsonOperators" :key="op.value" :value="op.value" :label="op.label" />
              </el-select>
              <el-input v-model="form.jsonAssertExpected" :placeholder="t('monitorForm.jsonExpectedPlaceholder')" />
            </div>
            <i18n-t keypath="monitorForm.jsonAssertHelp" tag="p" scope="global" class="tip json-assert-help">
              <template #jsonataOrg>
                <a href="https://jsonata.org/" target="_blank" rel="noopener noreferrer">jsonata.org</a>
              </template>
              <template #tryIt>
                <a href="https://try.jsonata.org/" target="_blank" rel="noopener noreferrer">{{ t('monitorForm.jsonataTryIt') }}</a>
              </template>
            </i18n-t>
          </div>
        </el-form-item>
        <el-form-item label="HTTPS">
          <el-checkbox v-model="form.allowInsecureTLS">{{ t('monitorForm.ignoreTls') }}</el-checkbox>
        </el-form-item>
      </template>
      <template v-else-if="form.type === 'download'">
        <!-- 下载速度监控:请求侧与 HTTP 一致(URL/方法/请求头/请求体/证书),
             但不给内容类断言(期望状态码/关键字/JSON 断言)——判定对象是速度。 -->
        <el-form-item :label="t('monitorForm.downloadUrl')" required>
          <el-input v-model="form.url" placeholder="https://example.com/file.bin" />
          <div class="tip download-url-tip">{{ t('monitorForm.downloadTip') }}</div>
        </el-form-item>
        <el-form-item :label="t('monitorForm.method')">
          <el-select v-model="form.method" style="width:120px">
            <el-option v-for="m in ['GET','POST','PUT','PATCH','DELETE','HEAD']" :key="m" :value="m" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('monitorForm.headers')">
          <el-input
            v-model="form.headersText"
            type="textarea"
            :rows="2"
            :placeholder="t('monitorForm.headersPlaceholder', { example: headersExample })"
          />
        </el-form-item>
        <el-form-item v-if="['POST','PUT','PATCH'].includes(form.method)" :label="t('monitorForm.body')">
          <el-input v-model="form.body" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="HTTPS">
          <el-checkbox v-model="form.allowInsecureTLS">{{ t('monitorForm.ignoreTls') }}</el-checkbox>
        </el-form-item>
      </template>
      <template v-else-if="form.type === 'tcp'">
        <el-form-item :label="t('monitorForm.targetHost')" required>
          <el-input v-model="form.targetHost" :placeholder="t('monitorForm.targetHostPlaceholderTcp')" />
        </el-form-item>
        <el-form-item :label="t('monitorForm.port')" required>
          <el-input-number v-model="form.port" :min="1" :max="65535" />
          <span class="tip">{{ t('monitorForm.portTip') }}</span>
        </el-form-item>
      </template>
      <template v-else-if="form.type === 'ping'">
        <el-form-item :label="t('monitorForm.targetHost')" required>
          <el-input v-model="form.targetHost" :placeholder="t('monitorForm.targetHostPlaceholderPing')" />
        </el-form-item>
      </template>

      <!-- IP 协议族:auto 交给系统、ipv4/ipv6 强制该族。外部上报没有节点探测,不给这项。 -->
      <el-form-item v-if="form.type !== 'push'" :label="t('monitorForm.ipVersion')">
        <el-radio-group v-model="form.ipVersion">
          <el-radio-button value="auto">{{ t('monitorForm.ipVersionAuto') }}</el-radio-button>
          <el-radio-button value="ipv4">IPv4</el-radio-button>
          <el-radio-button value="ipv6">IPv6</el-radio-button>
        </el-radio-group>
        <div class="tip ip-version-tip" :class="{ warn: ipVersionHint.warn }" style="display:block; margin-left:0">
          {{ t('monitorForm.ipVersionTip') }}
        </div>
        <div class="tip ip-version-tip" :class="{ warn: ipVersionHint.warn }" style="display:block; margin-left:0">
          {{ ipVersionHint.text }}
        </div>
      </el-form-item>

      <el-form-item v-if="!isDownload" :label="t('monitorForm.invertMode')">
        <el-switch v-model="form.invertMode" />
        <span class="tip">{{ t('monitorForm.invertModeTip') }}</span>
      </el-form-item>
      <el-form-item v-else :label="t('monitorForm.invertMode')">
        <span class="tip" style="margin-left:0">{{ t('monitorForm.downloadNoInvert') }}</span>
      </el-form-item>

      <el-form-item :label="t('monitorForm.period')" required>
        <el-input-number v-model="form.period" :min="10" :max="3600" />
        <span class="tip">
          {{ form.type === 'push'
            ? t('monitorForm.periodTipPush')
            : isDownload ? t('monitorForm.periodTipDownload') : t('monitorForm.periodTip') }}
        </span>
      </el-form-item>
      <el-form-item v-if="form.type !== 'push'" :label="t('monitorForm.timeout')" required>
        <el-input-number v-model="form.timeout" :min="1" :max="3600" />
      </el-form-item>
      <!-- 外部上报不给可用率阈值:每轮只有一个样本,成功率非 0% 即 100%,阈值填多少
           判定结果都一样,而填 0 会让这条监控永不告警(判定式 成功率 < 阈值 恒为假)。 -->
      <el-form-item v-if="!isPush" :label="isDownload ? t('monitorForm.speedThreshold') : t('monitorForm.threshold')" required>
        <el-input-number
          v-model="form.threshold"
          :min="isDownload ? 0.01 : 0"
          :max="isDownload ? 1048576 : 100"
          :precision="isDownload ? 2 : 0"
          :step="isDownload ? 1 : 1"
        />
        <el-select v-if="isDownload" v-model="form.speedUnit" style="width:110px; margin-left:8px">
          <el-option v-for="u in speedUnits" :key="u" :value="u" :label="u" />
        </el-select>
        <span class="tip">{{ isDownload ? t('monitorForm.speedThresholdTip') : t('monitorForm.thresholdTip') }}</span>
      </el-form-item>
      <el-form-item :label="isPush ? t('monitorForm.consecutivePush') : t('monitorForm.consecutive')" required>
        <el-input-number v-model="form.consecutive" :min="1" :max="20" />
        <span class="tip">{{ isPush ? t('monitorForm.consecutivePushTip') : t('monitorForm.consecutiveTip') }}</span>
      </el-form-item>
      <el-form-item v-if="form.type !== 'push'" :label="t('monitorForm.assign')" required>
        <el-radio-group v-model="form.assignMode">
          <el-radio value="all">{{ t('monitorForm.assignAll') }}</el-radio>
          <el-radio value="selected">{{ t('monitorForm.assignSelected') }}</el-radio>
          <el-radio value="exclude">{{ t('monitorForm.assignExclude') }}</el-radio>
        </el-radio-group>
        <template v-if="form.assignMode === 'exclude'">
          <el-select v-model="form.excludedAgentIds" multiple
            style="width:100%; margin-top:8px" :placeholder="t('monitorForm.excludePlaceholder')">
            <el-option v-for="a in agents" :key="a.id" :value="a.id"
              :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')" :disabled="a.status !== 'approved'" />
          </el-select>
          <div class="tip" style="display:block">
            {{ t('monitorForm.excludeTip') }}
          </div>
        </template>
        <template v-else-if="form.assignMode === 'selected'">
          <el-select v-model="form.assignedAgentIds" multiple
            style="width:100%; margin-top:8px" :placeholder="t('monitorForm.assignedPlaceholder')">
            <el-option v-for="a in agents" :key="a.id" :value="a.id"
              :label="a.name + (a.online ? t('monitorForm.agentOnlineSuffix') : '')" :disabled="a.status !== 'approved'" />
          </el-select>
          <div class="tip" style="display:block">{{ t('monitorForm.selectedTip') }}</div>
        </template>
        <div v-else class="tip" style="display:block">
          {{ t('monitorForm.allTip') }}
        </div>
      </el-form-item>
      <el-form-item :label="t('monitorForm.channels')">
        <el-select v-model="form.channelIds" multiple style="width:100%"
          :placeholder="t('monitorForm.channelsPlaceholder')">
          <!-- 已禁用的渠道照样能勾,但标出来:勾上它这一轮不会真发出去。 -->
          <el-option v-for="c in props.channels" :key="c.id" :value="c.id"
            :label="c.name + (c.enabled === false ? t('monitorForm.channelDisabledSuffix') : '')" />
        </el-select>
      </el-form-item>
    </el-form>
    <!-- 测试结果:判定 + 请求/返回明细。失败时默认展开(点测试就是为了看这个),
         成功默认收起,但详情一直都在,可随时展开或整段复制。 -->
    <div v-if="testResult" class="test-panel">
      <el-alert
        :type="testResult.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="testAlertTitle"
      >
        <div class="test-meta">
          {{ testMetaText }}
          <template v-if="testResult.invertMode">
            · {{ t('monitorForm.test.invertProbe', {
              result: t(testResult.probeOk ? 'monitorForm.test.probeOk' : 'monitorForm.test.probeFail'),
            }) }},
            {{ t('monitorForm.test.invertVerdict', {
              verdict: t(testResult.ok ? 'monitorForm.test.verdictHealthy' : 'monitorForm.test.verdictFailed'),
            }) }}
          </template>
        </div>
      </el-alert>
      <div v-if="hasTestDetail" class="test-detail-head">
        <el-button link type="primary" @click="testDetailOpen = !testDetailOpen">
          {{ testDetailOpen ? t('monitorForm.test.hideDetail') : t('monitorForm.test.showDetail') }}
        </el-button>
        <el-button link @click="copyTestReport">{{ t('monitorForm.test.copyDetail') }}</el-button>
      </div>
      <div v-show="testDetailOpen && hasTestDetail" class="test-detail">
        <div v-if="testRequestText">
          <div class="test-section-title">{{ t('monitorForm.test.sectionRequest') }}</div>
          <pre class="test-pre">{{ testRequestText }}</pre>
        </div>
        <div v-if="testResponseText">
          <div class="test-section-title">{{ t('monitorForm.test.sectionResponse') }}</div>
          <pre class="test-pre">{{ testResponseText }}</pre>
        </div>
      </div>
    </div>
    <template #footer>
      <div class="dialog-footer">
        <!-- 测试由在线节点执行(Dashboard 自己不探测):结果只在弹窗里看,不入库。 -->
        <el-button v-if="canTest" :loading="testing" @click="runTest">
          {{ testing ? t('monitorForm.test.running') : t('monitorForm.test.button') }}
        </el-button>
        <span v-else class="tip">{{ t('monitorForm.test.pushNoTarget') }}</span>
        <div>
          <el-button @click="emit('update:modelValue', false)">{{ t('common.cancel') }}</el-button>
          <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
        </div>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.tip { margin-left: 10px; color: #909399; font-size: 12px; }
/* IP 协议提示:没选可行协议族时标黄,这条监控会在所有节点上稳定失败。 */
.ip-version-tip { margin-top: 4px; line-height: 1.6; }
.download-url-tip { display: block; margin: 4px 0 0; line-height: 1.6; }
.ip-version-tip.warn { color: #e6a23c; }
.json-assert { width: 100%; }
.json-assert-row { display: flex; gap: 8px; margin-top: 8px; }
.json-assert-help {
  display: block;
  margin: 8px 0 0;
  line-height: 1.7;
}
.json-assert-help a { color: #409eff; text-decoration: none; }
.json-assert-help a:hover { text-decoration: underline; }
.code-editor { width: 100%; }
.code-cards { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 8px; }
.code-card { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.code-empty { color: #909399; font-size: 12px; line-height: 24px; }
.code-add { display: flex; align-items: center; }
/* ---- 测试结果面板 ---- */
.test-panel {
  margin-top: 4px;
  padding-top: 12px;
  border-top: 1px dashed var(--el-border-color);
}
.test-meta {
  margin-top: 4px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  line-height: 1.7;
}
.test-detail-head { display: flex; align-items: center; gap: 8px; margin-top: 8px; }
.test-section-title {
  margin: 8px 0 4px;
  font-size: 12px;
  font-weight: 600;
  color: var(--el-text-color-regular);
}
.test-pre {
  margin: 0;
  padding: 8px 10px;
  max-height: 180px;
  overflow: auto;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.dialog-footer { display: flex; align-items: center; justify-content: space-between; }
</style>
