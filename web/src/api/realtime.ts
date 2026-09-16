import { ref } from 'vue'

/**
 * 浏览器实时推送(票 10):模块级单例连接 /ws/browser。
 * 事件:hello | round_finalized | monitor_flipped | monitor_changed | monitor_deleted
 *      | monitors_changed | monitors_deleted | agent_changed | monitor_strips
 * 契约见 dashboard/internal/webhub 与 docs/protocol.md。
 * 断线指数退避自动重连;connected 供页面决定"推送为主、轮询兜底"的频率。
 *
 * 浏览器平时**只收不发**;唯一的上行帧是监控列表页要一次「最近状态」快照
 * (见 sendRealtime 与 MonitorsView)。
 */

export type RealtimeEvent = { type: string; data?: unknown }
type Handler = (data: unknown) => void

export const connected = ref(false)
/**
 * connectionId 每次连接建立自增。页面用它区分"这条连接上还没同步过":
 * 重连之后(代次变了)必须重新对齐一次,而断线窗口里丢掉的推送是补不回来的。
 */
export const connectionId = ref(0)
const listeners = new Map<string, Set<Handler>>()
let ws: WebSocket | null = null
let started = false

export function onRealtime(type: string, fn: Handler): () => void {
  let set = listeners.get(type)
  if (!set) {
    set = new Set()
    listeners.set(type, set)
  }
  set.add(fn)
  return () => set!.delete(fn)
}

/**
 * sendRealtime 向服务端发一帧请求。返回是否发出:连接没打开时返回 false,
 * 调用方据此走 HTTP 兜底(排队等连接建立只会让首屏更慢,还得处理重连后的重复)。
 */
export function sendRealtime(type: string, payload: Record<string, unknown> = {}): boolean {
  if (!ws || ws.readyState !== WebSocket.OPEN) return false
  try {
    ws.send(JSON.stringify({ type, ...payload }))
    return true
  } catch {
    return false // 发送瞬间连接断了:交给调用方的兜底路径
  }
}

function dispatch(ev: RealtimeEvent) {
  listeners.get(ev.type)?.forEach((fn) => {
    try {
      fn(ev.data)
    } catch {
      /* 单个订阅者异常不影响其它 */
    }
  })
}

/** 在 App 挂载时调用一次;重复调用安全。 */
export function startRealtime() {
  if (started) return
  started = true
  open()
}

function open() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  ws = new WebSocket(`${proto}://${location.host}/ws/browser`)
  let backoff = 1000

  ws.onopen = () => {
    // 先记代次再置 connected:订阅 connected 的页面会在回调里按代次判断要不要
    // 重新对齐(见 connectionId),顺序反了会读到上一代的号。
    connectionId.value++
    connected.value = true
    backoff = 1000
  }
  ws.onmessage = (e) => {
    try {
      dispatch(JSON.parse(e.data))
    } catch {
      /* 忽略坏帧 */
    }
  }
  ws.onclose = () => {
    connected.value = false
    setTimeout(open, backoff)
    backoff = Math.min(backoff * 2, 30000)
  }
  ws.onerror = () => ws?.close()
}
