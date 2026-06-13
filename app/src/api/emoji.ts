import { api } from './client'

export interface CustomEmoji {
  id: string
  workspace_id: string
  name: string
  image_url: string
  created_by: string
  created_at: string
}

export const emojiApi = {
  list(workspaceId: string) {
    return api.get<CustomEmoji[]>(`/workspaces/${workspaceId}/emojis`)
  },
  create(workspaceId: string, data: { name: string; image_url: string }) {
    return api.post<CustomEmoji>(`/workspaces/${workspaceId}/emojis`, data)
  },
  remove(workspaceId: string, emojiId: string) {
    return api.del<{ message: string }>(`/workspaces/${workspaceId}/emojis/${emojiId}`)
  },
}
