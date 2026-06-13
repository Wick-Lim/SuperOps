import { api } from './client'

export const blockApi = {
  list() {
    return api.get<Array<{ blocked_id: string; created_at: string }>>('/blocks')
  },
  block(blockedId: string) {
    return api.post<{ message: string }>('/blocks', { blocked_id: blockedId })
  },
  unblock(blockedId: string) {
    return api.del<{ message: string }>(`/blocks/${blockedId}`)
  },
}
