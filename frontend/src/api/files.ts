import { useAuthStore } from '@/stores/authStore'

const BASE_URL = '/api/v1'

export const fileApi = {
  async upload(workspaceId: string, file: File): Promise<{ id: string; name: string; content_type: string; size_bytes: number }> {
    const token = useAuthStore.getState().accessToken
    const form = new FormData()
    form.append('file', file)
    form.append('workspace_id', workspaceId)

    const res = await fetch(`${BASE_URL}/files/upload`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${token}` },
      body: form,
    })
    const data = await res.json()
    if (data.error) throw new Error(data.error.message)
    return data.data
  },

  downloadUrl(fileId: string): string {
    return `${BASE_URL}/files/${fileId}`
  },
}
