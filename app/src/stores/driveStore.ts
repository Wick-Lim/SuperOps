import { create } from 'zustand'
import type { DriveEntry, DriveFolder, RegistryKind } from '../api/drive'

/**
 * Drive's client state.
 *
 * The registry is here rather than fetched per screen because it is a property
 * of the DEPLOYMENT, not of a view: which editors exist decides what the "New"
 * menu offers, and asking again on every navigation would flicker the menu.
 * It is fetched once and never invalidated — a deployment does not gain an
 * editor while somebody is looking at a folder.
 */
interface DriveState {
  registry: RegistryKind[]
  registryLoaded: boolean

  /** The workspace's root folder id, so navigating "home" needs no request. */
  rootId: string | null

  /** The folder currently open, and the chain back to the root. */
  folder: DriveFolder | null
  breadcrumb: DriveFolder[]
  entries: DriveEntry[]
  cursor: string | null
  hasMore: boolean

  setRegistry: (kinds: RegistryKind[]) => void
  setRoot: (id: string) => void
  setFolder: (folder: DriveFolder, breadcrumb: DriveFolder[]) => void
  setEntries: (entries: DriveEntry[], cursor: string | null, hasMore: boolean) => void
  appendEntries: (entries: DriveEntry[], cursor: string | null, hasMore: boolean) => void
  /** Drops one entry without refetching, for an optimistic trash. */
  removeEntry: (kind: 'folder' | 'file', id: string) => void
  clear: () => void
}

export const useDriveStore = create<DriveState>()((set) => ({
  registry: [],
  registryLoaded: false,
  rootId: null,
  folder: null,
  breadcrumb: [],
  entries: [],
  cursor: null,
  hasMore: false,

  setRegistry: (registry) => set({ registry, registryLoaded: true }),
  setRoot: (rootId) => set({ rootId }),
  setFolder: (folder, breadcrumb) => set({ folder, breadcrumb }),
  setEntries: (entries, cursor, hasMore) => set({ entries, cursor, hasMore }),
  appendEntries: (entries, cursor, hasMore) =>
    set((s) => ({ entries: [...s.entries, ...entries], cursor, hasMore })),

  removeEntry: (kind, id) =>
    set((s) => ({
      entries: s.entries.filter((e) =>
        e.kind !== kind ? true : kind === 'folder' ? e.folder?.id !== id : e.file?.id !== id,
      ),
    })),

  // Registry survives a clear: it describes the server, not the session's
  // navigation, and refetching it on every workspace switch would blank the
  // "New" menu for a moment each time.
  clear: () =>
    set({ rootId: null, folder: null, breadcrumb: [], entries: [], cursor: null, hasMore: false }),
}))

/** creatableKinds are the types the "New" menu offers. A plain file is uploaded
 * rather than created — "New File" would produce an empty object with no way to
 * put bytes into it. */
export function creatableKinds(registry: RegistryKind[]): RegistryKind[] {
  return registry.filter((k) => k.creatable)
}
