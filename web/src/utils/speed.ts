// 下载速度监控的单位换算与展示。
//
// 约定(与后端 checkconfig 一致):速度在接口与存储里**统一以 KB/s 传递**,
// 只有"用户录入阈值"与"页面展示"按监控配置的单位(KB/s 或 MB/s)换算 ——
// 于是历史数据不会因为用户改了单位而含义漂移。
// 见 .scratch/download-speed-monitor/spec.md。

/** 速度单位白名单(与后端 checkconfig.SpeedUnits 一一对应)。 */
export const SPEED_UNITS = ['KB/s', 'MB/s'] as const
export type SpeedUnit = (typeof SPEED_UNITS)[number]

/** 默认单位:与后端 checkconfig.SpeedUnitKBps 一致。 */
export const DEFAULT_SPEED_UNIT: SpeedUnit = 'KB/s'

/** toKbps 把配置单位下的速度值换算为 KB/s(阈值比较与接口载荷都用 KB/s)。 */
export function toKbps(value: number, unit: string): number {
  return unit === 'MB/s' ? value * 1024 : value
}

/** fromKbps 把 KB/s 换算回展示/录入单位。 */
export function fromKbps(kbps: number, unit: string): number {
  return unit === 'MB/s' ? kbps / 1024 : kbps
}

/**
 * formatSpeed 把 KB/s 渲染成「12.34 MB/s」这样的展示文本(单位跟随监控配置)。
 * 没有速度数据(null/NaN)返回空串,由调用方决定显示占位符。
 */
export function formatSpeed(kbps: number | null | undefined, unit: string): string {
  if (kbps == null || !Number.isFinite(kbps)) return ''
  const u = unit || DEFAULT_SPEED_UNIT
  return `${fromKbps(kbps, u).toFixed(2)} ${u}`
}
