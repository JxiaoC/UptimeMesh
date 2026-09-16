// 中文词条聚合:结构即词条契约 —— `MessageSchema = typeof zhCN`(见 ../index.ts)。
// en-US/index.ts 用同一结构声明,任何一侧缺 key / 多 key 都会让 `npm run build`
// 里的 vue-tsc 直接报错,这就是「新增文案必须两种语言同步」的编译期门禁。
import agents from './agents'
import core from './core'
import detail from './detail'
import monitors from './monitors'
import overview from './overview'
import settings from './settings'

export default {
  ...core,
  ...overview,
  ...monitors,
  ...detail,
  ...agents,
  ...settings,
}
