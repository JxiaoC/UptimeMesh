import { ref } from 'vue'

/**
 * 监控详情弹窗的共享开关(模块级单例)。
 *
 * 为什么放模块级而不是各页面自己拿一个 ref:要有**唯一一个**弹窗实例。
 * 详情内容不便宜(趋势图 + 两张表 + 自己的 30s 轮询与 WebSocket 订阅),每个页面各挂一份
 * 就等于同一时刻最多有 3 份在后台跑;更麻烦的是"打开另一个监控"要能保证旧的那份被关掉。
 * 这里由 MainLayout 挂唯一的实例,列表页与总览页只调 openMonitorDetail(id)。
 *
 * id 为 null = 关闭。切换监控时直接换 id:弹窗不关(用户是在"看另一个监控",不是重开)。
 */
export const detailMonitorId = ref<string | null>(null)

// 打开/切换监控详情弹窗。
export function openMonitorDetail(id: string) {
  detailMonitorId.value = id
}

export function closeMonitorDetail() {
  detailMonitorId.value = null
}