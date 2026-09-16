import { computed } from 'vue'
import { createI18n } from 'vue-i18n'
import elementEn from 'element-plus/es/locale/lang/en'
import elementZhCn from 'element-plus/es/locale/lang/zh-cn'
import enUS from './locales/en-US'
import zhCN from './locales/zh-CN'

/**
 * 前端国际化:默认中文(zh-CN),支持英文(en-US),入口在后台右上角(components/LocaleSwitch.vue)。
 *
 * 约定(完整版见 AGENTS.md「国际化」):
 *  1. 所有面向用户的文案都必须走 t();中文词条是唯一事实源(locales/zh-CN/*),
 *     英文词条(locales/en-US/*)以 `typeof 中文模块` 声明 —— 少一个 key、
 *     多一个 key 都会让 `npm run build` 里的 vue-tsc 直接报错。
 *  2. 组件里 `const { t } = useI18n()`;非组件模块(axios 拦截器等)用 tGlobal。
 *  3. 不要在模块顶层调用 t():顶层常量只求值一次,切换语言不会生效,请用 computed
 *     或渲染期调用的函数。
 *  4. Element Plus 自带文案由 App.vue 的 <el-config-provider :locale> 驱动;
 *     ElMessageBox 是命令式 API 拿不到该注入,必须走 utils/confirm.ts。
 */
export type LocaleCode = 'zh-CN' | 'en-US'

/** localStorage 键:与 um_token 同前缀。 */
export const LOCALE_KEY = 'um_locale'

/** 默认语言恒为中文:不跟随浏览器语言(需求明确「默认中文」)。 */
export const DEFAULT_LOCALE: LocaleCode = 'zh-CN'

/** 词条结构契约(= 中文词条的形状)。 */
export type MessageSchema = typeof zhCN

/** 语言自称:不翻译 —— 英文界面里的中文选项也该写「简体中文」。 */
export const LANGUAGES: { code: LocaleCode; label: string }[] = [
  { code: 'zh-CN', label: '简体中文' },
  { code: 'en-US', label: 'English' },
]

function isLocaleCode(v: unknown): v is LocaleCode {
  return v === 'zh-CN' || v === 'en-US'
}

/** getInitialLocale 读本地偏好:没存过或值不可识别都回落中文。 */
export function getInitialLocale(): LocaleCode {
  try {
    const saved = localStorage.getItem(LOCALE_KEY)
    if (isLocaleCode(saved)) return saved
  } catch {
    /* 隐私模式下 localStorage 可能不可用:用默认语言即可 */
  }
  return DEFAULT_LOCALE
}

// Record<LocaleCode, MessageSchema> 顺带保证英文词条与中文同构(真正的门禁在
// en-US/*.ts 里各自的 typeof 标注上,这里只是让契约类型也被用到)。
const messages: Record<LocaleCode, MessageSchema> = {
  'zh-CN': zhCN,
  'en-US': enUS,
}

export const i18n = createI18n({
  legacy: false, // Composition API 模式
  globalInjection: true, // 模板里 $t 与 <i18n-t> 可用
  locale: getInitialLocale(),
  fallbackLocale: DEFAULT_LOCALE,
  messages,
})

/** 当前语言(响应式):模板/计算属性里读它会自动跟随切换。 */
export const locale = computed<LocaleCode>(() => i18n.global.locale.value as LocaleCode)

/** tGlobal 给非组件模块用(axios 拦截器、ElMessageBox 包装等)。 */
export const tGlobal = i18n.global.t

/** Element Plus 语言包:日期选择器、下拉空态等跟着切换。 */
export const elementLocale = computed(() => (locale.value === 'en-US' ? elementEn : elementZhCn))

/** applyDocumentLocale 同步 <html lang> 与页面标题(静态 index.html 里是中文兜底)。 */
export function applyDocumentLocale() {
  document.documentElement.lang = locale.value
  document.title = tGlobal('app.title')
}

/** setLocale 切换语言:写入本地偏好并同步文档语言/标题。 */
export function setLocale(code: LocaleCode) {
  i18n.global.locale.value = code
  try {
    localStorage.setItem(LOCALE_KEY, code)
  } catch {
    /* 存不下也不影响本次会话 */
  }
  applyDocumentLocale()
}

/** useLocale 组件内的统一入口。 */
export function useLocale() {
  return { locale, setLocale }
}
