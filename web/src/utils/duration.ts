// 时长的展示口径:秒 → 人类可读文本(「5 分 30 秒」)。
//
// 目前只有一处使用场景:状态变动记录里**恢复**那一行旁的「报警持续时长」
// (起点是上一条「报错」记录,见 .scratch/alert-duration/spec.md)。
//
// 词条走 tGlobal:vue-i18n 的 t 在读 locale 时建立依赖,所以模板里调用本函数
// 就能随语言切换重新渲染(与 utils/region.ts 的 regionName 同一套路)。
import { tGlobal } from '../i18n'

/**
 * formatDuration 把秒数渲染成当前语言的时长文本。
 * 负数/非有限值返回空串 —— 调用方据此不显示卡片(宁可少一张,也不显示「-3 秒」)。
 *
 * 只保留两个量级(与后端 humanSeconds 的口径一致):秒 / 分秒 / 时分 / 天时。
 * 告警时长是给人扫一眼的,「1 天 3 小时」比「1 天 3 小时 12 分 9 秒」更有用 ——
 * 精确到秒的原始时间在旁边两张变动记录里本来就有。
 */
export function formatDuration(sec: number): string {
  if (!Number.isFinite(sec) || sec < 0) return ''
  const s = Math.floor(sec)
  if (s < 60) return tGlobal('common.durationSeconds', { seconds: s })
  if (s < 3600) {
    return tGlobal('common.durationMinutesSeconds', { minutes: Math.floor(s / 60), seconds: s % 60 })
  }
  if (s < 86400) {
    return tGlobal('common.durationHoursMinutes', {
      hours: Math.floor(s / 3600),
      minutes: Math.floor((s % 3600) / 60),
    })
  }
  return tGlobal('common.durationDaysHours', {
    days: Math.floor(s / 86400),
    hours: Math.floor((s % 86400) / 3600),
  })
}