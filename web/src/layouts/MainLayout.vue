<script setup lang="ts">
import { computed, onMounted, onUnmounted, provide, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { http, TOKEN_KEY } from '../api/http'
import { connected, connectionId, onRealtime } from '../api/realtime'
import LocaleSwitch from '../components/LocaleSwitch.vue'
import MonitorDetailDialog from '../views/MonitorDetailDialog.vue'
import { setFaviconAlertCount } from '../utils/favicon'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
// 详情页(/monitors/:id)高亮「监控」菜单项
const activeMenu = computed(() =>
  route.path.startsWith('/monitors') ? '/monitors' : route.path,
)
function logout() {
  localStorage.removeItem(TOKEN_KEY)
  router.push({ name: 'login' })
}

// 节点变更失效计数(票 10):节点页订阅后在变更时自动重拉。
const agentsVersion = ref(0)
let off: (() => void) | undefined
onMounted(() => {
  off = onRealtime('agent_changed', () => agentsVersion.value++)
})
onUnmounted(() => off?.())
provide('agentsVersion', agentsVersion)

interface MonitorStatus { enabled: boolean; displayState: string }
const alertCount = ref(0)
let alertTimer: number | undefined
let alertPollTimer: number | undefined
let alertRefreshPending = false
let alertRefreshRunning = false
let alertRefreshAgain = false
let alertSequence = 0

function updateFavicon() {
  setFaviconAlertCount(alertCount.value)
}

async function refreshAlertCount() {
  if (alertRefreshRunning) {
    alertRefreshAgain = true
    return
  }
  alertRefreshRunning = true
  const sequence = ++alertSequence
  try {
    const monitors = (await http.get('/overview')) as MonitorStatus[]
    if (sequence === alertSequence) {
      alertCount.value = monitors.filter((m) => m.enabled && m.displayState === 'DOWN').length
      updateFavicon()
    }
  } catch {
    // Keep the last known count during transient API failures.
  } finally {
    alertRefreshRunning = false
    if (alertRefreshAgain) {
      alertRefreshAgain = false
      scheduleAlertRefresh()
    }
  }
}

function scheduleAlertRefresh() {
  if (alertRefreshPending) return
  alertRefreshPending = true
  alertTimer = window.setTimeout(() => {
    alertRefreshPending = false
    void refreshAlertCount()
  }, 400)
}

function startAlertPolling(live: boolean) {
  if (alertPollTimer !== undefined) clearInterval(alertPollTimer)
  alertPollTimer = window.setInterval(() => void refreshAlertCount(), live ? 30000 : 5000)
}

const alertEvents = [
  'round_finalized',
  'monitor_flipped',
  'monitor_changed',
  'monitors_changed',
  'monitor_deleted',
  'monitors_deleted',
]
let alertOffs: (() => void)[] = []
onMounted(() => {
  void refreshAlertCount()
  alertOffs = alertEvents.map((event) => onRealtime(event, scheduleAlertRefresh))
  startAlertPolling(connected.value)
})
watch(connected, (live) => {
  startAlertPolling(live)
  if (live) scheduleAlertRefresh()
})
watch(connectionId, (next, previous) => {
  if (previous > 0 && next !== previous) scheduleAlertRefresh()
})
watch(alertCount, updateFavicon)
onUnmounted(() => {
  alertOffs.forEach((off) => off())
  if (alertTimer !== undefined) clearTimeout(alertTimer)
  if (alertPollTimer !== undefined) clearInterval(alertPollTimer)
  alertSequence++
  setFaviconAlertCount(0)
})

// 导航项放 computed:切语言后 t() 重新求值,菜单文案立即跟着变
// (写成模块级常量只会在加载时求值一次)。
const menu = computed(() => [
  { index: '/', label: t('nav.overview') },
  { index: '/monitors', label: t('nav.monitors') },
  { index: '/agents', label: t('nav.agents') },
  { index: '/channels', label: t('nav.channels') },
  { index: '/settings', label: t('nav.settings') },
])
</script>

<template>
  <el-container style="height: 100vh">
    <el-aside width="200px">
      <div class="brand">
        <img src="/uptimemesh-mark.svg" alt="" />
        <span>UptimeMesh</span>
      </div>
      <!-- index 必须用绝对路径:相对路径在 /monitors/:id 下会被解析成 /monitors/xxx 导致导航失效 -->
      <el-menu :default-active="activeMenu" router>
        <el-menu-item v-for="m in menu" :key="m.index" :index="m.index">{{ m.label }}</el-menu-item>
      </el-menu>
    </el-aside>
    <el-container>
      <el-header class="topbar">
        <span>{{ t('app.subtitle') }}</span>
        <div class="topbar-right">
          <el-tag size="small" :type="connected ? 'success' : 'warning'" effect="plain">
            {{ connected ? `● ${t('common.live')}` : `○ ${t('common.polling')}` }}
          </el-tag>
          <!-- 语言切换固定在右上角(需求);切换即时生效,无需刷新 -->
          <LocaleSwitch />
          <el-button link type="primary" @click="logout">{{ t('common.logout') }}</el-button>
        </div>
      </el-header>
      <el-main>
        <!-- 只缓存监控列表页(include 按组件名匹配,见 MonitorsView 的 defineOptions):
             从监控详情页返回、或在菜单间来回切换时,列表页不再被卸载重建,
             于是回来的一瞬间就是上次那份数据与筛选/勾选,不再空表 + 转圈;
             新数据由页面自己的 onActivated 静默拉取(见 MonitorsView)。
             详情页必须留在缓存之外:它带 :id 路由参数,缓存住会让 /monitors/a 的实例
             被 /monitors/b 复用,页面还停在旧监控上。 -->
        <router-view v-slot="{ Component }">
          <keep-alive :include="['MonitorsView']">
            <component :is="Component" />
          </keep-alive>
        </router-view>
      </el-main>
    </el-container>
    <!-- 监控详情弹窗:挂**唯一一份**在路由之外(见 utils/monitorDetailDialog)。
         列表页点监控名、总览页点变动记录/卡片都打开它,不再跳独立详情页;
         组件自己按 detailMonitorId 决定显隐,没打开时连面板都不渲染。 -->
    <MonitorDetailDialog />
  </el-container>
</template>

<style scoped>
.brand {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 700;
  font-size: 18px;
  padding: 16px;
  color: #303133;
}
.brand img {
  width: 24px;
  height: 24px;
  flex: none;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid #e4e7ed;
  color: #606266;
}
.topbar-right {
  display: flex;
  align-items: center;
  gap: 12px;
}
.el-menu {
  border-right: none;
}
</style>
