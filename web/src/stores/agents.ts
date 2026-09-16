import { defineStore } from 'pinia'
import { ref } from 'vue'
import { http } from '../api/http'

interface AgentLite { id: string; name: string; region: string }

// 节点名称/地域映射共享 store(总览/详情/监控列表都要用)。
// 带 includeDeleted:已删除节点的历史轮次仍需回显名称而非 ID。
export const useAgentsStore = defineStore('agents', () => {
  const agents = ref<AgentLite[]>([])
  async function load() {
    agents.value = (await http.get('/agents?includeDeleted=1')) as unknown as AgentLite[]
  }
  const nameOf = (id: string) =>
    agents.value.find((a) => a.id === id)?.name || id.slice(0, 8)
  const regionOf = (id: string) =>
    agents.value.find((a) => a.id === id)?.region || ''
  return { agents, load, nameOf, regionOf }
})
