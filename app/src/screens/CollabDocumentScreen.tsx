import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { SafeAreaView, View, Text, StyleSheet } from 'react-native'
import * as Y from 'yjs'
import { theme } from '../lib/theme'
import { space } from '../lib/responsive'
import { errorMessage } from '../api/client'
import { driveApi } from '../api/drive'
import { CollabProvider, type ProviderStatus } from '../lib/collab/provider'
import { useAuthStore } from '../stores/authStore'
import { ErrorState, LoadingState, ScreenHeader } from './internal/ui'
import Editor from '../editor/Editor'
import Backlinks from '../components/drive/Backlinks'
import Grid from '../sheet/Grid'
import Canvas from '../design/Canvas'
import { SheetModel } from '../lib/sheet/model'
import { extractSheet } from '../lib/sheet/projection'
import { DesignModel } from '../lib/design/model'
import { extractDesign } from '../lib/design/projection'
import type { Projection } from '../editor/projection'

/**
 * A collaborative document.
 *
 * The screen owns the Y.Doc and the provider; the editor is handed both and
 * owns neither. That split is what lets the spreadsheet and the design surface
 * reuse everything here except the last component — they differ in how they
 * render a Y.Doc, not in how they get one.
 *
 * There is no save button. The document is a CRDT and every keystroke is
 * already durable; a save button would be a lie about where the truth lives.
 */
export default function CollabDocumentScreen({ navigation, route }: { navigation: any; route: any }) {
  const documentId: string | undefined = route.params?.documentId
  const fileId: string | undefined = route.params?.fileId
  const name: string = route.params?.name ?? 'Document'
  /**
   * Which surface to render.
   *
   * THE CLIENT DISPATCHES ON file_type, exactly as the server registry does.
   * Adding a fourth editor is one entry below and one extractor — the room, the
   * provider, the projection, the revocation handling and this screen are all
   * shared. A screen per editor would have been three copies of everything
   * above the surface component.
   */
  const fileType: string = route.params?.fileType ?? 'document'

  const user = useAuthStore((s) => s.user)
  const [status, setStatus] = useState<ProviderStatus>('connecting')
  const [detail, setDetail] = useState<string | null>(null)
  const [seq, setSeq] = useState(0)

  // One Y.Doc for the life of the screen. Recreating it would discard the
  // in-memory state and re-download the whole document on every render.
  const doc = useMemo(() => new Y.Doc(), [])
  const providerRef = useRef<CollabProvider | null>(null)

  const identity = useMemo(
    () => ({
      id: user?.id ?? 'anonymous',
      name: user?.full_name || user?.username || 'Someone',
      color: colorFor(user?.id ?? ''),
    }),
    [user],
  )

  useEffect(() => {
    if (!documentId) return
    const provider = new CollabProvider({
      documentId,
      doc,
      user: identity,
      onStatus: (s, d) => {
        setStatus(s)
        setDetail(d ?? null)
      },
    })
    providerRef.current = provider
    return () => {
      provider.destroy()
      providerRef.current = null
    }
  }, [documentId, doc, identity])

  // The projection's seq must not exceed the log head, so it is read from the
  // descriptor rather than guessed. A projection above the head is refused —
  // correctly, since it would be a client describing content the log does not
  // have.
  useEffect(() => {
    if (!fileId) return
    let cancelled = false
    void driveApi
      .open(fileId)
      .then((res) => {
        if (!cancelled) setSeq(res.data?.projection?.head_seq ?? 0)
      })
      .catch(() => {
        /* the gap is an optimisation; the document still opens */
      })
    return () => {
      cancelled = true
    }
  }, [fileId])

  const publish = useCallback(
    (projection: Projection) => {
      if (!fileId) return
      // Best effort and deliberately silent. The projection is derived state:
      // losing one costs search until the next edit and costs zero writing, so
      // interrupting somebody's typing with an error about it would be the
      // wrong trade.
      void driveApi.project(fileId, projection).catch(() => undefined)
    },
    [fileId],
  )

  // The grid and the canvas read plain snapshots of the CRDT, so they need a
  // signal that it changed. The block editor does not — y-prosemirror binds to
  // the document directly — which is why this subscription is here rather than
  // in the provider.
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    if (fileType === 'document') return
    const bump = () => setRevision((r) => r + 1)
    doc.on('update', bump)
    return () => doc.off('update', bump)
  }, [doc, fileType])

  const sheet = useMemo(() => new SheetModel(doc), [doc])
  const design = useMemo(() => new DesignModel(doc), [doc])

  // Publishing is debounced by the same reasoning as the editor's: short enough
  // that closing a tab leaves the object searchable, long enough that typing is
  // one projection rather than forty. The server's monotonic write makes a
  // burst harmless regardless.
  const projectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const scheduleProjection = useCallback(() => {
    if (projectTimer.current) clearTimeout(projectTimer.current)
    projectTimer.current = setTimeout(() => {
      publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
    }, 2000)
  }, [publish, fileType, sheet, design, seq])

  // FLUSH ON UNMOUNT, do not merely cancel.
  //
  // Cancelling the pending timer was silently dropping the projection for any
  // sheet or canvas edited and closed inside the 2 s debounce — permanently, in
  // practice, because nothing on the server produces content and the next
  // projection only happens if somebody opens the object again. The block
  // editor already flushes on unmount (Editor.web.tsx); this is the same rule
  // for the two surfaces that project from here.
  //
  // The refs are read rather than the state values so this effect can have an
  // empty dependency list and actually run on unmount rather than on every
  // change of seq.
  const flushRef = useRef<() => void>(() => {})
  flushRef.current = () => {
    if (!fileId || fileType === 'document') return
    publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
  }
  useEffect(() => () => {
    if (projectTimer.current) {
      clearTimeout(projectTimer.current)
      projectTimer.current = null
      flushRef.current()
    }
  }, [])

  if (!documentId) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title={name} onBack={() => navigation.goBack()} />
        <ErrorState message="This file has no collaborative document." />
      </SafeAreaView>
    )
  }

  if (status === 'revoked') {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title={name} onBack={() => navigation.goBack()} />
        <ErrorState message={detail ?? 'Your access to this document was removed.'} />
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={name}
        subtitle={subtitleFor(status)}
        onBack={() => navigation.goBack()}
      />
      {status === 'connecting' ? (
        <LoadingState label="Opening" />
      ) : status === 'error' ? (
        <ErrorState message={detail ?? errorMessage(new Error('Could not load the document'))} />
      ) : (
        <View style={styles.body}>
          {status === 'read-only' && (
            <View style={styles.notice}>
              <Text style={styles.noticeText}>You have read access to this document.</Text>
            </View>
          )}
          {fileType === 'spreadsheet' ? (
            <Grid
              model={sheet}
              editable={status === 'synced'}
              revision={revision}
              onEdit={scheduleProjection}
            />
          ) : fileType === 'design' ? (
            <Canvas
              model={design}
              editable={status === 'synced'}
              revision={revision}
              onEdit={scheduleProjection}
            />
          ) : (
            <>
              <Editor
                doc={doc}
                awareness={providerRef.current!.awareness}
                editable={status === 'synced'}
                user={{ name: identity.name, color: identity.color }}
                onProject={publish}
                seq={seq}
                fileId={fileId}
              />
              {/* "Where is this used". Filtered by what the READER may see, so
                  a document that mentions this one but is not shared with them
                  does not appear — the panel must not become a way to discover
                  documents. */}
              {fileId ? <Backlinks refType="file" refId={fileId} navigation={navigation} /> : null}
            </>
          )}
        </View>
      )}
    </SafeAreaView>
  )
}

function subtitleFor(status: ProviderStatus): string {
  switch (status) {
    case 'connecting':
      return 'Connecting'
    case 'synced':
      return 'All changes saved'
    case 'read-only':
      return 'Read only'
    case 'error':
      return 'Disconnected'
    default:
      return ''
  }
}

/** A stable colour per user, so a cursor keeps its identity across sessions.
 * Derived rather than stored: it is decoration, and a table for it would be a
 * migration for something nobody will ever query. */
function colorFor(id: string): string {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0
  const palette = ['#e5534b', '#c9873a', '#3fa66a', '#3b82c4', '#8b5cd6', '#c2418f']
  return palette[hash % palette.length]
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  body: { flex: 1 },
  notice: {
    backgroundColor: theme.surfaceAlt,
    padding: space.sm,
    marginHorizontal: space.md,
    marginTop: space.sm,
    borderRadius: 8,
  },
  noticeText: { color: theme.textMuted, fontSize: 13 },
})
