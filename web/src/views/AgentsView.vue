<script setup lang="ts">
import { computed, inject, onMounted, onUnmounted, ref, watch, type Ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ElMessage } from 'element-plus'
import { http } from '../api/http'
import { connected } from '../api/realtime'
import { confirmBox } from '../utils/confirm'
import { flagClass, regionName, regionOptions } from '../utils/region'

interface Agent {
  id: string
  name: string
  version: string
  os: string
  arch: string
  sourceIp: string
  status: 'pending' | 'approved' | 'revoked'
  online: boolean
  region: string // 生效地域码(ISO alpha-2),空 = 未知或内网来源
  manual: boolean // 地域为手动指定
  netScope: 'public' | 'loopback' | 'lan' | 'linklocal' // 来源 IP 的可达范围
  firstSeen: string
  lastSeen: string
  latestVersion: string // 仪表盘可分发的版本;空 = 未内置该架构安装包
  outdated: boolean // 版本与分发版本不一致(需要升级)
  upgradable: boolean // 能一键升级:outdated + 节点声明了自升级能力
  // 节点自报的本机 IPv4 / IPv6 可用性;null = 老版本 Agent 未上报(未知,不等于不支持)
  ipv4Available: boolean | null
  ipv6Available: boolean | null
}

const { t } = useI18n()
const agents = ref<Agent[]>([])
const loading = ref(false)

// 状态与地域下拉都放在 computed 里:切语言后 t()/regionOptions() 重新求值
// (写成模块级常量只会求值一次,切换语言就看不到变化)。
const statusText = computed<Record<string, string>>(() => ({
  pending: t('agents.statusPending'),
  approved: t('agents.statusApproved'),
  revoked: t('agents.statusRevoked'),
}))
// 地域可选项按当前语言排序;regionOptions() 每次要排上百项,故缓存起来。
const regions = computed(() => regionOptions())

// 内网来源 IP 不解析国家(region 为空),改说它属于哪张网 —— 本地开发时
// 127.0.0.1 显示「未知」毫无信息量,「本机」才能一眼看出 Agent 和仪表盘同机。
const SCOPE_KEYS: Record<string, string> = {
  loopback: 'agents.scopeLoopback',
  lan: 'agents.scopeLan',
  linklocal: 'agents.scopeLinkLocal',
}

// scopeLabel 返回内网来源的说明文案;公网来源(交给地域解析)返回空串。
// 手动指定过地域的节点照手动值显示国家,不再提内网。
function scopeLabel(row: Agent): string {
  if (row.manual) return ''
  const key = SCOPE_KEYS[row.netScope]
  return key ? t(key) : ''
}

// regionText 地域格显示的文字:内网来源优先(说"在哪张网"),其次国家名,
// 都没有才是「未知」。单行省略后可能看不全,故悬停提示里也带上它。
function regionText(row: Agent): string {
  return scopeLabel(row) || (row.region ? regionName(row.region) : t('common.unknown'))
}

// regionClass 地域文字的配色:内网来源=常规色、解析不出=次要色(与「未知」区分开)。
function regionClass(row: Agent): string {
  if (scopeLabel(row)) return 'region-scope'
  return row.region ? '' : 'region-empty'
}

// regionTip 是地域格的悬浮说明,按取值来源分三种:手动 / 内网 / 自动解析。
// 前面带上当前取值 —— 地域名(英文尤甚)与「手动」标记会超出列宽被省略,提示要给得出全文。
function regionTip(row: Agent): string {
  const source = row.manual
    ? t('agents.regionManualTip')
    : scopeLabel(row) ? t('agents.regionLocalTip') : t('agents.regionAutoTip')
  return `${regionText(row)} · ${source}`
}

// nameTip 名称格的悬浮说明同理:节点名超过列宽会被省略,提示里先给完整名称。
function nameTip(row: Agent): string {
  return `${row.name} · ${t('agents.renameTip')}`
}

// netTagType 网络能力标签的配色:可用=绿、不可用=红、未上报=灰(未知)。
// 三态必须分开:把"未上报"画成红色会让所有老节点看起来像没有 IPv6 出口地址。
function netTagType(available: boolean | null): 'success' | 'danger' | 'info' {
  if (available === true) return 'success'
  if (available === false) return 'danger'
  return 'info'
}

// 拉取节点列表。silent=true 用于推送/轮询触发的对齐:不置 loading,
// 免得兜底轮询每 30 秒让表格转一次圈。
async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    agents.value = (await http.get('/agents')) as unknown as Agent[]
  } finally {
    if (!silent) loading.value = false
  }
}

// 兜底轮询 + 重连对齐。推送只是"尽快",并不保证送达:服务端对慢客户端丢帧不背压
// (见 docs/protocol.md),浏览器 WS 也会重连。只靠推送的话,断连窗口里错过的
// agent_changed 会让本页的在线/离线**永久**停在过期值(要等下一次事件或手动刷新),
// 线上表现为"节点明明在线、节点页却显示离线,重启节点后才恢复正常"。
let timer: number | undefined
function startPolling(live: boolean) {
  if (timer) clearInterval(timer)
  timer = window.setInterval(() => void load(true), live ? 30000 : 5000)
}

onMounted(() => {
  void load()
  startPolling(connected.value)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
watch(connected, (live) => {
  startPolling(live)
  void load(true) // 断线期间可能漏了事件,恢复后先对齐一次
})

// 票 10:节点上下线/审批变化时由布局的 agentsVersion 触发重拉。
// 不要写成 watch(version, load):watch 会把新值当成 load 的第一个参数传进去。
const version = inject<Ref<number>>('agentsVersion', ref(0))
watch(version, () => { void load(true) })

async function act(agent: Agent, action: string, confirmText?: string) {
  if (confirmText) {
    await confirmBox(confirmText, t('agents.actionConfirmTitle'), { type: 'warning' })
  }
  await http.post(`/agents/${agent.id}/${action}`)
  ElMessage.success(t('common.success'))
  await load()
}

// 删除已吊销节点(后端伪删除:列表隐藏,历史轮次仍能回显节点名)。
async function remove(agent: Agent) {
  await confirmBox(
    t('agents.deleteConfirm', { name: agent.name }),
    t('agents.deleteConfirmTitle'),
    { type: 'warning' },
  )
  await http.delete(`/agents/${agent.id}`)
  ElMessage.success(t('common.deleted'))
  await load()
}

// ---- 一键升级:让节点升到仪表盘当前分发的版本 ----
// 后端已判定 upgradable(已批准 + 有对应架构安装包 + 版本与分发包不一致);
// 这里只补在线条件(离线节点收不到指令),并做 30 秒防抖——节点升级后要重启再重连。
const upgrading = ref<Set<string>>(new Set())
const eligible = computed(() => agents.value.filter((a) => a.upgradable && a.online))

function markUpgrading(id: string) {
  upgrading.value.add(id)
  setTimeout(() => upgrading.value.delete(id), 30000)
}

// 节点重启后版本变化由 agent_changed 推送触发刷新,这里再兜底拉两次,
// 覆盖推送不可用(断线轮询)或升级稍慢的情况。
function reloadAfterUpgrade() {
  setTimeout(() => void load(true), 8000)
  setTimeout(() => void load(true), 20000)
}

async function upgrade(agent: Agent) {
  markUpgrading(agent.id)
  try {
    await http.post(`/agents/${agent.id}/upgrade`)
    ElMessage.success(t('agents.upgradeSent', { name: agent.name }))
    reloadAfterUpgrade()
  } catch {
    upgrading.value.delete(agent.id) // 下发就失败:立刻恢复按钮,便于重试
  }
}

async function upgradeAll() {
  const targets = eligible.value
  if (!targets.length) return
  await confirmBox(
    t('agents.upgradeAllConfirm', { count: targets.length, version: targets[0].latestVersion })
    + t('agents.upgradeAllConfirmExtra'),
    t('agents.upgradeAllConfirmTitle'),
    { type: 'warning' },
  )
  let ok = 0
  const failed: string[] = []
  for (const a of targets) {
    markUpgrading(a.id)
    try {
      await http.post(`/agents/${a.id}/upgrade`)
      ok++
    } catch {
      upgrading.value.delete(a.id)
      failed.push(a.name)
    }
  }
  if (ok) {
    ElMessage.success(t('agents.upgradeBatchSent', { count: ok }))
    reloadAfterUpgrade()
  }
  if (failed.length) {
    ElMessage.warning(t('agents.upgradeFailed', { list: failed.join(t('common.listSeparator')) }))
  }
}

// ---- 重命名:只改展示名,不影响节点身份(接入名)----
// 身份由「接入时自报的名称 + 来源 IP」决定,接口也只改展示名;所以这里不需要
// 提醒用户"改完要重装",节点重连照样认回自己(见 .scratch/agent-rename/spec.md)。
const renameVisible = ref(false)
const renameSaving = ref(false)
const renameForm = ref<{ id: string; name: string }>({ id: '', name: '' })

function openRename(agent: Agent) {
  renameForm.value = { id: agent.id, name: agent.name }
  renameVisible.value = true
}

async function saveRename() {
  const name = renameForm.value.name.trim()
  if (!name) {
    ElMessage.warning(t('agents.renameRequired'))
    return
  }
  renameSaving.value = true
  try {
    await http.put(`/agents/${renameForm.value.id}/name`, { name })
    ElMessage.success(t('agents.renameSaved'))
    renameVisible.value = false
    await load()
  } finally {
    renameSaving.value = false
  }
}

// ---- 地域:自动按 IP 解析,也可手动指定(下拉清空 = 恢复自动解析)----
const regionVisible = ref(false)
const regionForm = ref<{ id: string; name: string; region: string }>({ id: '', name: '', region: '' })

function openRegion(agent: Agent) {
  regionForm.value = { id: agent.id, name: agent.name, region: agent.region }
  regionVisible.value = true
}

async function saveRegion() {
  await http.put(`/agents/${regionForm.value.id}/region`, { region: regionForm.value.region })
  ElMessage.success(t('agents.regionSaved'))
  regionVisible.value = false
  await load()
}

// ---- 一键安装:生成在 Linux 目标机执行的安装命令 ----
// 默认接入地址取自当前页面(生产由 dashboard 直接服务,host 即真实地址)。
const defaultServer = (() => {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/ws/agent`
})()

const installVisible = ref(false)
const installForm = ref({ server: defaultServer, key: '', name: '' })

// 旧库仅存哈希时设置接口不回传明文,此时提示去设置页轮换;否则留空由脚本交互输入。
const keyPlaceholder = computed(() =>
  installForm.value.key ? '' : t('agents.keyPlaceholder'),
)

// 打开弹窗时自动拉取接入密钥填入,省去去设置页手抄一步。
async function openInstall() {
  installVisible.value = true
  if (installForm.value.key) return
  try {
    const s = (await http.get('/settings')) as unknown as { enrollmentKey?: string }
    installForm.value.key = s.enrollmentKey || ''
  } catch {
    /* 拉取失败则留空,仍可手填或让安装脚本交互输入 */
  }
}

// httpBase 由接入地址推导安装包下载地址:ws->http、wss->https,去掉 /ws/agent 路径。
function httpBase(server: string) {
  return server.replace(/^wss:/, 'https:').replace(/^ws:/, 'http:').replace(/\/ws\/agent\/?$/, '')
}

const installCmd = computed(() => {
  const server = installForm.value.server.trim()
  if (!server) return ''
  // 单引号包裹可变值,避免名称含空格/中文时被 shell 拆词。
  const parts = [`curl -fsSL ${httpBase(server)}/api/v1/agent/install.sh | sudo sh -s -- --server ${server}`]
  const key = installForm.value.key.trim()
  if (key) parts.push(`--key '${key}'`)
  const name = installForm.value.name.trim()
  if (name) parts.push(`--name '${name}'`)
  return parts.join(' ')
})

async function copyInstall() {
  if (!installCmd.value) {
    ElMessage.warning(t('agents.needServer'))
    return
  }
  try {
    await navigator.clipboard.writeText(installCmd.value)
    ElMessage.success(t('agents.installCopied'))
  } catch {
    ElMessage.warning(t('agents.clipboardDenied'))
  }
}
</script>

<template>
  <div>
    <div style="display:flex;justify-content:space-between;align-items:center">
      <h3>{{ t('nav.agents') }}</h3>
      <div>
        <el-tooltip
          placement="top"
          :content="eligible.length
            ? t('agents.upgradeAllTooltip', { version: eligible[0].latestVersion })
            : t('agents.upgradeAllNone')"
        >
          <el-button type="warning" plain :disabled="!eligible.length" @click="upgradeAll">
            {{ t('agents.upgradeAll') }}<span v-if="eligible.length">({{ eligible.length }})</span>
          </el-button>
        </el-tooltip>
        <el-button type="primary" @click="openInstall">{{ t('agents.copyInstallCmd') }}</el-button>
        <el-button :loading="loading" @click="load()">{{ t('common.refresh') }}</el-button>
      </div>
    </div>
    <el-table class="agents-table" :data="agents" v-loading="loading" :empty-text="t('agents.empty')">
      <el-table-column :label="t('agents.columnName')" min-width="140">
        <template #default="{ row }">
          <el-tooltip :content="nameTip(row)" placement="top">
            <div class="editable-cell" @click="openRename(row)">
              <span class="editable-text">{{ row.name }}</span>
            </div>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column :label="t('agents.columnStatus')" width="110">
        <template #default="{ row }">
          <el-tag :type="row.status === 'approved' ? 'success' : row.status === 'pending' ? 'warning' : 'danger'">
            {{ statusText[row.status] }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column :label="t('agents.columnOnline')" width="90">
        <template #default="{ row }">
          <span class="online-status">
            <span class="online-dot" :class="row.online ? 'is-online' : 'is-offline'"></span>
            {{ row.online ? t('common.online') : t('common.offline') }}
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('agents.columnVersion')" width="170">
        <template #default="{ row }">
          <span>{{ row.version || '—' }}</span>
          <el-tooltip v-if="row.upgradable" placement="top"
            :content="t('agents.upgradableTip', { version: row.latestVersion })
              + (row.online ? t('agents.upgradableClickable') : t('agents.upgradableOffline'))">
            <el-tag size="small" type="warning" effect="plain" style="margin-left:6px">{{ t('agents.upgradable') }}</el-tag>
          </el-tooltip>
          <el-tooltip v-else-if="row.outdated" placement="top"
            :content="t('agents.reinstallTip')">
            <el-tag size="small" type="info" effect="plain" style="margin-left:6px">{{ t('agents.reinstall') }}</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column :label="t('agents.columnPlatform')" width="130" show-overflow-tooltip>
        <template #default="{ row }">{{ row.os }}/{{ row.arch }}</template>
      </el-table-column>
      <!-- 网络能力:节点自报的本机 IPv4 / IPv6 可用性,决定某条强制协议族的监控在它
           身上能不能测出来。三态(可用 / 不可用 / 未知)不合并,见 netTagType。 -->
      <el-table-column width="130">
        <template #header>
          <el-tooltip :content="t('agents.netTip')" placement="top">
            <span>{{ t('agents.columnNet') }}</span>
          </el-tooltip>
        </template>
        <template #default="{ row }">
          <div class="net-cell">
            <el-tag size="small" effect="plain" :type="netTagType(row.ipv4Available)">IPv4</el-tag>
            <el-tag size="small" effect="plain" :type="netTagType(row.ipv6Available)">IPv6</el-tag>
          </div>
        </template>
      </el-table-column>
      <el-table-column prop="sourceIp" :label="t('agents.columnSourceIp')" width="130" show-overflow-tooltip />
      <el-table-column :label="t('agents.columnRegion')" width="160">
        <template #default="{ row }">
          <el-tooltip :content="regionTip(row)" placement="top">
            <div class="editable-cell" @click="openRegion(row)">
              <span v-if="flagClass(row.region)" :class="flagClass(row.region)" />
              <span class="editable-text" :class="regionClass(row)">{{ regionText(row) }}</span>
              <span v-if="row.manual" class="region-manual">{{ t('agents.regionManualTag') }}</span>
            </div>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column prop="lastSeen" :label="t('agents.columnLastSeen')" width="170" show-overflow-tooltip />
      <el-table-column :label="t('agents.columnActions')" width="210" fixed="right">
        <template #default="{ row }">
          <template v-if="row.status === 'pending'">
            <el-button size="small" type="success" @click="act(row, 'approve')">{{ t('agents.approve') }}</el-button>
            <el-button size="small" type="danger" plain
              @click="act(row, 'reject', t('agents.rejectConfirm', { name: row.name }))">{{ t('agents.reject') }}</el-button>
          </template>
          <template v-else-if="row.status === 'approved'">
            <el-button v-if="row.upgradable && row.online" size="small" type="primary"
              :loading="upgrading.has(row.id)"
              @click="upgrade(row)">{{ t('agents.upgrade') }}</el-button>
            <el-button size="small" type="warning" plain
              @click="act(row, 'revoke', t('agents.revokeConfirm', { name: row.name }))">{{ t('agents.revoke') }}</el-button>
          </template>
          <template v-else>
            <el-button size="small" type="primary" plain
              @click="act(row, 're-approve')">{{ t('agents.reApprove') }}</el-button>
            <el-button size="small" type="danger" plain
              @click="remove(row)">{{ t('common.delete') }}</el-button>
          </template>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="installVisible" :title="t('agents.installTitle')" width="640px">
      <!-- 段落里有 <b>Linux</b>,故走 i18n-t 的具名插槽,避免把 HTML 拼进词条 -->
      <i18n-t keypath="agents.installIntro" tag="p" scope="global" style="margin-top:0;color:#909399;font-size:13px">
        <template #linux><b>Linux</b></template>
      </i18n-t>
      <el-form label-width="90px">
        <el-form-item :label="t('agents.installServer')">
          <el-input v-model="installForm.server" :placeholder="t('agents.installServerPlaceholder')" />
        </el-form-item>
        <el-form-item :label="t('agents.installKey')">
          <el-input v-model="installForm.key" :placeholder="keyPlaceholder" />
        </el-form-item>
        <el-form-item :label="t('agents.installName')">
          <el-input v-model="installForm.name" :placeholder="t('agents.installNamePlaceholder')" />
        </el-form-item>
      </el-form>
      <el-input
        :model-value="installCmd"
        type="textarea"
        :rows="3"
        readonly
        style="font-family:monospace"
      />
      <template #footer>
        <el-button @click="installVisible = false">{{ t('common.close') }}</el-button>
        <el-button type="primary" :disabled="!installCmd" @click="copyInstall">{{ t('agents.copyCommand') }}</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="renameVisible" :title="t('agents.renameDialogTitle')" width="440px">
      <p class="dialog-hint">{{ t('agents.renameHint') }}</p>
      <el-input
        v-model="renameForm.name"
        maxlength="64"
        show-word-limit
        :placeholder="t('agents.renamePlaceholder')"
        @keyup.enter="saveRename"
      />
      <template #footer>
        <el-button @click="renameVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :loading="renameSaving" @click="saveRename">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="regionVisible" :title="t('agents.regionDialogTitle', { name: regionForm.name })" width="440px">
      <p class="dialog-hint">{{ t('agents.regionHint') }}</p>
      <el-select v-model="regionForm.region" filterable clearable :placeholder="t('agents.regionAutoRestore')" style="width:100%">
        <el-option v-for="opt in regions" :key="opt.code" :label="opt.name" :value="opt.code">
          <span :class="flagClass(opt.code)" style="margin-right:8px" />{{ opt.name }}
        </el-option>
      </el-select>
      <template #footer>
        <el-button @click="regionVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="saveRegion">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* 节点列表:所有列一律单行 —— 只要有一列折行,整行行高就被撑起来,行高参差不齐
   (英文文案比中文长得多,尤其明显)。放不下的内容由 .cell 自带的
   overflow:hidden + text-overflow:ellipsis 省略,可省略的列(平台/来源 IP/最近心跳)
   再配 show-overflow-tooltip 悬停看全;名称与地域格自带悬停提示,提示里已带上完整取值。 */
.agents-table :deep(.cell) {
  white-space: nowrap;
}
/* 标签单元格(状态/版本/网络能力)放不下时,让省略号出现在标签内部 ——
   el-tag 是 inline-flex,只靠 .cell 的 text-overflow 会被硬切掉半截文字。 */
.agents-table :deep(.cell .el-tag) {
  max-width: 100%;
}
.agents-table :deep(.cell .el-tag .el-tag__content) {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* el-badge 的圆点绝对定位在容器顶边,无法与文字垂直对齐,改用普通圆点。 */
.online-status {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.online-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex: none;
}
.online-dot.is-online {
  background: var(--el-color-success);
}
.online-dot.is-offline {
  background: var(--el-color-info);
}
/* 可就地编辑的单元格(节点名、地域):点击弹窗修改。放不下时在自己的宽度内省略,
   国旗与「手动」标记不参与收缩(否则会被压扁),完整取值由悬停提示给出。 */
.editable-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
  max-width: 100%;
}
.editable-cell .fi,
.editable-cell .region-manual {
  flex: none;
}
.editable-text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.editable-cell:hover {
  color: var(--el-color-primary);
}
.editable-cell .fi {
  border-radius: 2px;
  font-size: 16px;
  line-height: 1;
}
.region-empty {
  color: var(--el-text-color-secondary);
}
/* 内网来源:说明的是「在哪张网」而不是国家,故用次要色与「未知」区分开 */
.region-scope {
  color: var(--el-text-color-regular);
}
.region-manual {
  font-size: 11px;
  color: var(--el-color-warning);
  border: 1px solid var(--el-color-warning-light-5);
  border-radius: 3px;
  padding: 0 3px;
  line-height: 16px;
}
.dialog-hint {
  margin: 0 0 12px;
  color: #909399;
  font-size: 13px;
}
.net-cell {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
</style>
