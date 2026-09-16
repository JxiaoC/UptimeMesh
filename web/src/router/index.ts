import { createRouter, createWebHistory } from 'vue-router'
import { TOKEN_KEY } from '../api/http'
import LoginView from '../views/LoginView.vue'
import MainLayout from '../layouts/MainLayout.vue'
import OverviewView from '../views/OverviewView.vue'
import SettingsView from '../views/SettingsView.vue'
import AgentsView from '../views/AgentsView.vue'
import MonitorsView from '../views/MonitorsView.vue'
import MonitorDetailView from '../views/MonitorDetailView.vue'
import ChannelsView from '../views/ChannelsView.vue'

const routes = [
  { path: '/login', name: 'login', component: LoginView },
  {
    path: '/',
    component: MainLayout,
    children: [
      { path: '', name: 'overview', component: OverviewView },
      // 票 11:设置
      { path: 'monitors', name: 'monitors', component: MonitorsView },
      { path: 'monitors/:id', name: 'monitorDetail', component: MonitorDetailView },
      { path: 'agents', name: 'agents', component: AgentsView },
      { path: 'channels', name: 'channels', component: ChannelsView },
      { path: 'settings', name: 'settings', component: SettingsView },
    ],
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach((to) => {
  const authed = !!localStorage.getItem(TOKEN_KEY)
  if (to.name !== 'login' && !authed) return { name: 'login' }
  if (to.name === 'login' && authed) return { name: 'overview' }
})
