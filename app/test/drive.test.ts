import { describe, it, expect, beforeEach } from 'vitest'
import { formatBytes } from '../src/api/drive'
import { useDriveStore, creatableKinds } from '../src/stores/driveStore'
import type { RegistryKind } from '../src/api/drive'

describe('formatBytes', () => {
  it('reads the way a person reads a size', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1024)).toBe('1.0 KB')
    expect(formatBytes(1536)).toBe('1.5 KB')
    // Past 10 the decimal is noise.
    expect(formatBytes(20 * 1024)).toBe('20 KB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB')
    expect(formatBytes(3 * 1024 * 1024 * 1024)).toBe('3.0 GB')
  })

  it('does not run past its unit table', () => {
    // A petabyte-scale number must still render rather than producing
    // "undefined" from an index nobody bounded.
    expect(formatBytes(5 * 1024 ** 5)).toMatch(/TB$/)
  })
})

describe('creatableKinds', () => {
  const kinds: RegistryKind[] = [
    {
      type: 'file',
      display_name: 'File',
      storage_mode: 'blob',
      extensions: [],
      mime_types: [],
      creatable: false,
      versioned: true,
      previewable: false,
      client_projected: false,
    },
    {
      type: 'document',
      display_name: 'Document',
      storage_mode: 'collab',
      extensions: [],
      mime_types: [],
      creatable: true,
      versioned: false,
      previewable: false,
      client_projected: true,
    },
  ]

  it('never offers "New File"', () => {
    // A plain file is UPLOADED. Creating one would produce an empty object with
    // no way to put bytes into it, and the server refuses it with a 400 — so an
    // entry in the menu would be a control that always fails.
    const offered = creatableKinds(kinds)
    expect(offered.map((k) => k.type)).toEqual(['document'])
  })

  it('offers nothing when the deployment registers nothing creatable', () => {
    expect(creatableKinds([kinds[0]])).toEqual([])
  })
})

describe('driveStore', () => {
  beforeEach(() => {
    useDriveStore.setState({
      registry: [],
      registryLoaded: false,
      rootId: null,
      folder: null,
      breadcrumb: [],
      entries: [],
      cursor: null,
      hasMore: false,
    })
  })

  it('keeps the registry across a clear', () => {
    // The registry describes the SERVER, not the session's navigation.
    // Dropping it on a workspace switch would blank the "New" menu each time.
    const kind: RegistryKind = {
      type: 'document',
      display_name: 'Document',
      storage_mode: 'collab',
      extensions: [],
      mime_types: [],
      creatable: true,
      versioned: false,
      previewable: false,
      client_projected: true,
    }
    useDriveStore.getState().setRegistry([kind])
    useDriveStore.getState().setRoot('root-1')
    useDriveStore.getState().clear()

    const s = useDriveStore.getState()
    expect(s.registry).toHaveLength(1)
    expect(s.registryLoaded).toBe(true)
    expect(s.rootId).toBeNull()
  })

  it('removes an entry by kind AND id', () => {
    // A folder and a file can share an id in no real deployment, but the store
    // is keyed by both because the listing is one stream of two types and
    // matching on id alone would be a latent way to drop the wrong row.
    useDriveStore.getState().setEntries(
      [
        { kind: 'folder', folder: { id: 'x' } as any },
        { kind: 'file', file: { id: 'x' } as any },
        { kind: 'file', file: { id: 'y' } as any },
      ],
      null,
      false,
    )
    useDriveStore.getState().removeEntry('file', 'x')

    const left = useDriveStore.getState().entries
    expect(left).toHaveLength(2)
    expect(left[0].kind).toBe('folder')
    expect(left[1].file?.id).toBe('y')
  })

  it('appends a page without losing the first', () => {
    useDriveStore.getState().setEntries([{ kind: 'file', file: { id: 'a' } as any }], 'cur', true)
    useDriveStore.getState().appendEntries([{ kind: 'file', file: { id: 'b' } as any }], null, false)

    const s = useDriveStore.getState()
    expect(s.entries.map((e) => e.file?.id)).toEqual(['a', 'b'])
    expect(s.hasMore).toBe(false)
    expect(s.cursor).toBeNull()
  })
})
