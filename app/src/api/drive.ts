import { api, withPaging } from './client'

/**
 * Drive.
 *
 * The one thing to understand before reading the rest: the client DISPATCHES ON
 * `file_type`, never on a MIME type. The server publishes what types exist at
 * `GET /drive/registry`, and the descriptor returned by `GET /drive/files/{id}`
 * says which of `content_url` / `collab_document_id` is populated. A client that
 * branched on `content_type` would have to be taught about every editor, which
 * is exactly what the registry exists to prevent.
 */

export type StorageMode = 'blob' | 'collab'

export interface DriveFolder {
  id: string
  workspace_id: string
  parent_id: string | null
  name: string
  is_root: boolean
  created_by: string | null
  trashed_at?: string
  created_at: string
  updated_at: string
}

export interface DriveFile {
  id: string
  workspace_id: string
  folder_id: string | null
  name: string
  file_type: string
  content_type: string
  size_bytes: number
  current_version: number
  has_thumbnail: boolean
  created_by: string
  trashed_at?: string
  created_at: string
  updated_at: string
}

/**
 * The open descriptor — the whole dispatch contract in one payload.
 *
 * Exactly one of `collab_document_id` and `content_url` is non-null. `capability`
 * is what THIS caller may do, so a read-only surface can be rendered without a
 * second round trip — and so it is rendered at all, which a client that only
 * knew "you could open it" would not do.
 */
export interface DriveDescriptor extends DriveFile {
  storage_mode: StorageMode
  capability: 'read' | 'comment' | 'write' | 'share' | 'admin'
  collab_document_id: string | null
  content_url: string | null
  thumbnail_url: string | null
}

/** One row of a folder listing. Folders and files arrive in one stream, because
 * that is how a file browser renders them — paginating two lists against one
 * scrollbar is how "the last folder is on page 3" happens. */
export interface DriveEntry {
  kind: 'folder' | 'file'
  folder?: DriveFolder
  file?: DriveFile
}

export interface RegistryKind {
  type: string
  display_name: string
  storage_mode: StorageMode
  extensions: string[]
  mime_types: string[]
  /** Whether "New <display_name>" should appear. A plain file is uploaded. */
  creatable: boolean
  versioned: boolean
  previewable: boolean
  /**
   * Whether this type's searchable body comes from the CLIENT.
   *
   * The server cannot read a CRDT document, so the editor that has it in memory
   * renders it to text and posts that. The flag is on the wire — rather than
   * each editor deciding for itself — because three editors deciding
   * independently is exactly how you end up with three projection pipelines.
   */
  client_projected: boolean
}

export interface DriveVersion {
  file_id: string
  version: number
  size_bytes: number
  content_type: string
  created_by: string | null
  created_at: string
  is_current: boolean
}

export interface TrashEntry {
  kind: 'folder' | 'file'
  id: string
  name: string
  trashed_at: string
  trashed_by: string | null
  /** When it goes for good, or null when this deployment purges nothing
   * automatically. It is the promise made to whoever deleted it, so it is shown
   * rather than recomputed from a setting the client cannot see. */
  purge_after: string | null
  size_bytes: number
  item_count: number
}

/**
 * Storage usage. The two halves are SEPARATE and there is deliberately no
 * total: one is exact at every instant an upload can observe it and the other is
 * recomputed by a job, so a single number would have an accuracy nobody can
 * state. `counts_trashed_files` and `counts_old_versions` come from the server
 * rather than being assumed here — they are policy, and a client that hardcoded
 * them would be wrong the day the policy changed.
 */
export interface DriveUsage {
  workspace_id: string
  quota_bytes: number
  available_bytes: number
  blob: { bytes: number; as_of: string | null; consistency: 'exact' }
  collab: { bytes: number; as_of: string | null; consistency: 'eventual' }
  counts_trashed_files: boolean
  counts_old_versions: boolean
  breakdown?: {
    live_bytes: number
    trashed_bytes: number
    version_bytes: number
    recomputed_bytes: number
    drift_bytes: number
  }
}

export interface Share {
  subject_type: string
  subject_id: string
  capability: string
  granted_by?: string
  created_at: number
}

export interface ShareLink {
  id: string
  object_type: 'folder' | 'file'
  object_id: string
  capability: string
  has_password: boolean
  expires_at: string | null
  max_uses: number | null
  use_count: number
  created_by: string | null
  created_at: string
}

export interface PickedFile {
  uri: string
  name: string
  mimeType?: string
}

/** rnFilePart builds the { uri, name, type } object React Native's FormData
 * accepts for a file, which the DOM lib's types do not model. */
function rnFilePart(file: PickedFile) {
  return {
    uri: file.uri,
    name: file.name,
    type: file.mimeType || 'application/octet-stream',
  } as unknown as Blob
}

export const driveApi = {
  /** What editors this DEPLOYMENT has. Fetched once and cached by the store; a
   * client that hardcoded the list would offer "New Spreadsheet" on a server
   * that has no spreadsheet. */
  registry() {
    return api.get<RegistryKind[]>('/drive/registry')
  },

  root(workspaceId: string) {
    return api.get<DriveFolder>(`/workspaces/${workspaceId}/drive/root`)
  },

  folder(folderId: string) {
    return api.get<{ folder: DriveFolder; breadcrumb: DriveFolder[] }>(`/drive/folders/${folderId}`)
  },

  children(folderId: string, cursor?: string, limit?: number) {
    return api.get<DriveEntry[]>(withPaging(`/drive/folders/${folderId}/children`, cursor, limit))
  },

  createFolder(workspaceId: string, parentId: string, name: string) {
    return api.post<DriveFolder>(`/workspaces/${workspaceId}/drive/folders`, {
      parent_id: parentId,
      name,
    })
  },

  renameFolder(folderId: string, name: string) {
    return api.patch<DriveFolder>(`/drive/folders/${folderId}`, { name })
  },

  moveFolder(folderId: string, parentId: string) {
    return api.post<DriveFolder>(`/drive/folders/${folderId}/move`, { parent_id: parentId })
  },

  trashFolder(folderId: string) {
    return api.del<void>(`/drive/folders/${folderId}`)
  },

  /** "New document": the server creates the row, the ACL and the editor's
   * initial state in one transaction and answers with the open descriptor, so
   * the client can render immediately rather than fetching again. */
  createFile(workspaceId: string, folderId: string, name: string, fileType: string) {
    return api.post<DriveDescriptor>(`/workspaces/${workspaceId}/drive/files`, {
      folder_id: folderId,
      name,
      file_type: fileType,
    })
  },

  upload(workspaceId: string, folderId: string, file: PickedFile) {
    const form = new FormData()
    form.append('file', rnFilePart(file))
    form.append('folder_id', folderId)
    return api.upload<DriveDescriptor>(`/workspaces/${workspaceId}/drive/files/upload`, form)
  },

  open(fileId: string) {
    return api.get<DriveDescriptor>(`/drive/files/${fileId}`)
  },

  renameFile(fileId: string, name: string) {
    return api.patch<DriveFile>(`/drive/files/${fileId}`, { name })
  },

  moveFile(fileId: string, folderId: string) {
    return api.post<DriveFile>(`/drive/files/${fileId}/move`, { folder_id: folderId })
  },

  trashFile(fileId: string) {
    return api.del<void>(`/drive/files/${fileId}`)
  },

  versions(fileId: string) {
    return api.get<DriveVersion[]>(`/drive/files/${fileId}/versions`)
  },

  /** Posting new bytes. Answers 409 for a collab-backed type: a PUT into a
   * CRDT-backed object would be discarded by the next merge, so the client must
   * not offer it — check `storage_mode` before showing the control. */
  replaceContent(fileId: string, file: PickedFile) {
    const form = new FormData()
    form.append('file', rnFilePart(file))
    return api.upload<DriveDescriptor>(`/drive/files/${fileId}/content`, form)
  },

  restoreVersion(fileId: string, version: number) {
    return api.post<DriveDescriptor>(`/drive/files/${fileId}/versions/${version}/restore`)
  },

  trash(workspaceId: string) {
    return api.get<TrashEntry[]>(`/workspaces/${workspaceId}/drive/trash`)
  },

  restore(objectType: 'folder' | 'file', objectId: string) {
    return api.post<{ id: string; folder_id: string; relocated: boolean; note: string }>(
      `/drive/${objectType}/${objectId}/restore`,
    )
  },

  emptyTrash(workspaceId: string) {
    return api.del<{ files_purged: number; folders_purged: number; bytes_freed: number }>(
      `/workspaces/${workspaceId}/drive/trash`,
    )
  },

  usage(workspaceId: string) {
    return api.get<DriveUsage>(`/workspaces/${workspaceId}/drive/usage`)
  },

  setQuota(workspaceId: string, quotaBytes: number) {
    return api.put<DriveUsage>(`/workspaces/${workspaceId}/drive/quota`, { quota_bytes: quotaBytes })
  },

  shares(objectType: 'folder' | 'file', objectId: string) {
    return api.get<Share[]>(`/drive/${objectType}/${objectId}/shares`)
  },

  share(objectType: 'folder' | 'file', objectId: string, subjectId: string, capability: string) {
    return api.put<Share>(`/drive/${objectType}/${objectId}/shares`, {
      subject_id: subjectId,
      capability,
    })
  },

  unshare(objectType: 'folder' | 'file', objectId: string, subjectId: string) {
    return api.del<void>(`/drive/${objectType}/${objectId}/shares/user/${subjectId}`)
  },

  links(objectType: 'folder' | 'file', objectId: string) {
    return api.get<ShareLink[]>(`/drive/${objectType}/${objectId}/links`)
  },

  /**
   * Mints a link. The token comes back ONCE and is not recoverable — the server
   * stores only its hash — so the UI has to put it in front of the user
   * immediately and must not assume it can fetch it again.
   */
  createLink(
    objectType: 'folder' | 'file',
    objectId: string,
    opts: { capability?: string; password?: string; expires_at?: string; max_uses?: number } = {},
  ) {
    return api.post<{ link: ShareLink; token: string; note: string }>(
      `/drive/${objectType}/${objectId}/links`,
      opts,
    )
  },

  revokeLink(linkId: string) {
    return api.del<void>(`/drive/links/${linkId}`)
  },
}

/** formatBytes renders a size the way a person reads one. Binary units, because
 * that is what the numbers are. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
}
