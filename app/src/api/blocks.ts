import { api } from './client'

/** backend/internal/block/model.go — Block. */
export interface Block {
  blocked_id: string
  created_at: string
}

export const blockApi = {
  /** Users the caller has blocked. */
  list() {
    return api.get<Block[]>('/blocks')
  },
  /**
   * Blocking also stops a DM from being opened in either direction
   * (POST /channels/dm answers 403 `BLOCKED`).
   */
  block(blockedId: string) {
    return api.post<{ message: string }>('/blocks', { blocked_id: blockedId })
  },
  unblock(blockedId: string) {
    return api.del<{ message: string }>(`/blocks/${blockedId}`)
  },
}
