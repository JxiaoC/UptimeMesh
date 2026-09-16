import { ElMessageBox, type ElMessageBoxOptions } from 'element-plus'
import { tGlobal } from '../i18n'

/**
 * ElMessageBox 的国际化包装 —— 所有确认框/提示框都必须走这里。
 *
 * 为什么不能直接用 ElMessageBox:它是命令式 API,用 `app.use(ElementPlus)` 安装时
 * 捕获的 appContext 渲染,拿不到 App.vue 里 <el-config-provider :locale> 提供的语言,
 * 默认的「确定 / 取消」按钮不会随语言切换而变化。这里统一补上按钮文案(调用方显式
 * 传入的优先),既保证两种语言下都对,也避免各处重复写。
 */

/** boxOptions 合并默认按钮文案;显式传入的 confirmButtonText 等仍然优先。 */
function boxOptions(opts?: ElMessageBoxOptions): ElMessageBoxOptions {
  return {
    confirmButtonText: tGlobal('common.confirm'),
    cancelButtonText: tGlobal('common.cancel'),
    ...opts,
  }
}

/** confirmBox 确认框(取消时以 reject 结束,调用方按需 try/catch)。 */
export function confirmBox(
  message: string,
  title: string,
  opts?: ElMessageBoxOptions,
): Promise<unknown> {
  return ElMessageBox.confirm(message, title, boxOptions(opts))
}

/** alertBox 提示框(只有一个确认按钮)。 */
export function alertBox(
  message: string,
  title: string,
  opts?: ElMessageBoxOptions,
): Promise<unknown> {
  return ElMessageBox.alert(message, title, boxOptions(opts))
}
