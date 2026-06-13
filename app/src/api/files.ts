import { api } from './client'

export interface UploadedFile {
  id: string
  name: string
  content_type: string
  size_bytes: number
  storage_key: string
}

// pickedFile mirrors the shape returned by expo-document-picker / image-picker.
export interface PickedFile {
  uri: string
  name: string
  mimeType?: string
}

export const fileApi = {
  upload(workspaceId: string, file: PickedFile) {
    const form = new FormData()
    // React Native's FormData accepts a { uri, name, type } object for files,
    // which the DOM lib types don't model — cast through unknown.
    const rnFile = { uri: file.uri, name: file.name, type: file.mimeType || 'application/octet-stream' }
    form.append('file', rnFile as unknown as Blob)
    form.append('workspace_id', workspaceId)
    return api.upload<UploadedFile>('/files/upload', form)
  },
  remove(fileId: string) {
    return api.del<{ message: string }>(`/files/${fileId}`)
  },
}
