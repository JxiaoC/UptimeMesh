import axios from 'axios'
import { ElMessage } from 'element-plus'
import { tGlobal } from '../i18n'
import { router } from '../router'

export const TOKEN_KEY = 'um_token'

/** 统一 API 客户端:自动携带 JWT,401 清凭据跳登录。 */
export const http = axios.create({ baseURL: '/api/v1', timeout: 15000 })

http.interceptors.request.use((config) => {
  const token = localStorage.getItem(TOKEN_KEY)
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

// 这里不是组件、没有 i18n 上下文,所以用 tGlobal(读当前语言)。
// 注意:后端返回的 message 仍是中文(见 AGENTS.md「国际化」的范围说明),
// 这里只兜底本地文案。
http.interceptors.response.use(
  (resp) => {
    const body = resp.data
    if (body && typeof body === 'object' && 'code' in body && body.code !== 0) {
      ElMessage.error(body.message || tGlobal('errors.requestFailed'))
      return Promise.reject(new Error(body.message))
    }
    return body?.data !== undefined ? body.data : body
  },
  (err) => {
    if (err.response?.status === 401) {
      localStorage.removeItem(TOKEN_KEY)
      const cur = router.currentRoute.value
      if (cur.name !== 'login') router.push({ name: 'login' })
    } else if (err.code === 'ECONNABORTED' || err.message === 'canceled') {
      // axios 超时(ECONNABORTED)与主动中止会走这里:请求根本没拿到响应,
      // 但不是「网络不通」——多半是后端慢(如 db-stats 赶上写排队),分开提示避免误导。
      ElMessage.error(tGlobal('errors.timeout'))
    } else {
      ElMessage.error(err.response?.data?.message || tGlobal('errors.network'))
    }
    return Promise.reject(err)
  },
)
