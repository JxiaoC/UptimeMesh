import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import 'flag-icons/css/flag-icons.min.css'
import App from './App.vue'
import { router } from './router'
import { applyDocumentLocale, i18n } from './i18n'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.use(i18n)
// Element Plus 自带文案的语言不在这里写死:由 App.vue 的 <el-config-provider :locale>
// 按当前语言动态提供(否则切语言时日期选择器、下拉空态不会变)。
app.use(ElementPlus)
// 静态 index.html 里是中文兜底,进入应用后按当前语言覆盖 <html lang> 与标题。
applyDocumentLocale()
app.mount('#app')
