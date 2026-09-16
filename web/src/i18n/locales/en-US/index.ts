// English catalog aggregator. Every module is typed against its zh-CN counterpart,
// so a missing or extra key fails `vue-tsc` (npm run build) instead of silently
// falling back to Chinese at runtime.
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
