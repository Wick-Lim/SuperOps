import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { SafeAreaView, View, Text, StyleSheet } from 'react-native'
import * as Y from 'yjs'
import { theme } from '../lib/theme'
import { space } from '../lib/responsive'
import { driveApi } from '../api/drive'
import {
  CollabProvider,
  isProviderEditable,
  type ProviderStatus,
} from '../lib/collab/provider'
import { useAuthStore } from '../stores/authStore'
import { ErrorState, LoadingState, ScreenHeader } from './internal/ui'
import Editor from '../editor/Editor'
import Backlinks from '../components/drive/Backlinks'
import Grid from '../sheet/Grid'
import Canvas from '../design/Canvas'
import { SheetModel } from '../lib/sheet/model'
import { worthPublishing } from '../editor/catchup'
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
 * There is no save button. Local updates remain pending until the provider
 * receives an own echo or a valid HTTP append acknowledgement; the header
 * reports that lifecycle directly.
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
  const [projectionSeq, setProjectionSeq] = useState(0)
  const projectionSeqRef = useRef(0)

  const acceptProjectionSeq = useCallback((next: number) => {
    if (next <= projectionSeqRef.current) return
    projectionSeqRef.current = next
    setProjectionSeq(next)
  }, [])

  useEffect(() => {
    projectionSeqRef.current = 0
    setProjectionSeq(0)
  }, [documentId])

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
      onProjectionSeq: acceptProjectionSeq,
      // The server asking for a projection it cannot produce itself. Raises the
      // same counter as the gap on open, so both arrive at whichever surface
      // owns this file's content — and both wait for `synced` rather than
      // projecting a document that has not received its state yet.
      onProjectRequest: () => setProjectNonce((n) => n + 1),
    })
    providerRef.current = provider
    return () => {
      provider.destroy()
      providerRef.current = null
    }
  }, [documentId, doc, identity, acceptProjectionSeq])

  // The provider's contiguous watermark owns the projection sequence. The
  // descriptor gap is only a repair hint; its head can be ahead of content
  // this client has applied and must never be assigned to projection state.
  //
  // THE GAP IS ALSO THE BACKSTOP, and reading it without acting on it was the
  // whole of what the design called one. A document edited and closed while its
  // browser was killed — no unmount, so no flush — is unsearchable until
  // somebody opens it, and the server cannot produce content on its own. So
  // opening it re-projects when it is behind. `behind` is state rather than a
  // ref because the projection cannot run until the CRDT has actually loaded.
  //
  // A COUNTER, not a flag. The worker re-asks every ten minutes, and a boolean
  // that had already been consumed would swallow every request after the first
  // — so a document that failed to repair once would never be asked again.
  const [projectNonce, setProjectNonce] = useState(0)
  useEffect(() => {
    if (!fileId) return
    let cancelled = false
    void driveApi
      .open(fileId)
      .then((res) => {
        if (cancelled) return
        const p = res.data?.projection
        // Strictly behind. Equal means somebody already projected this exact
        // state, and re-sending it would be a write the server discards.
        if (p && p.head_seq > p.projection_seq) setProjectNonce((n) => n + 1)
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
      const seq = projectionSeqRef.current
      publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
    }, 2000)
  }, [publish, fileType, sheet, design])

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
    const seq = projectionSeqRef.current
    publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
  }
  // Catch up once the document is loaded and writable.
  //
  // Gated on `synced` rather than firing on open: projecting an empty Y.Doc
  // that has not received its state yet would write an EMPTY body over a good
  // one, and the server's monotonic guard would not stop it — the seq is the
  // log head either way. Read-only callers do not project at all; they have no
  // capability for it and the server would refuse.
  //
  // THE BLOCK EDITOR IS NOT HANDLED HERE. It owns its own ProseMirror state, so
  // it answers through the `catchUp` prop below; this effect must not consume
  // the nonce on its behalf. It used to — the assignment sat above the type
  // switch, so a `document` request was marked answered and nothing was
  // published, which is precisely the swallow the counter exists to prevent.
  const caughtUp = useRef(0)
  useEffect(() => {
    if (fileType !== 'spreadsheet' && fileType !== 'design') return
    if (projectNonce === 0 || caughtUp.current === projectNonce) return
    if (status !== 'synced' || !fileId) return
    const timer = setTimeout(() => {
      // CLAIMED HERE, not above. The assignment used to sit before the timer,
      // and the effect's cleanup clears that timer — so any dependency change
      // inside the 1500 ms window (a reconnect flips `status` to 'connecting'
      // and back) re-ran the effect, found the nonce already claimed, and
      // returned. Request answered, nothing published: the exact swallow the
      // counter exists to prevent, which I had fixed for `document` and left
      // here. Editor.web.tsx claims inside its timer for the same reason.
      if (caughtUp.current === projectNonce) return
      const seq = projectionSeqRef.current
      const projection =
        fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq)
      // EMPTY IS NOT A REPAIR — see worthPublishing. Claiming after this check
      // also means an empty result leaves the request live for the server's
      // next re-ask rather than burning it.
      if (!worthPublishing(projection)) return
      caughtUp.current = projectNonce
      publish(projection)
    }, 1500)
    return () => clearTimeout(timer)
  }, [projectNonce, status, fileId, fileType, sheet, design, publish])

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

  const editable = isProviderEditable(status)

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
        <ErrorState
          message={detail ?? 'Changes have not been saved.'}
          onRetry={() => providerRef.current?.retry()}
          retryLabel="Retry saving"
        />
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
              editable={editable}
              revision={revision}
              onEdit={scheduleProjection}
            />
          ) : fileType === 'design' ? (
            <Canvas
              model={design}
              editable={editable}
              revision={revision}
              onEdit={scheduleProjection}
            />
          ) : (
            <>
              <Editor
                doc={doc}
                awareness={providerRef.current!.awareness}
                editable={editable}
                user={{ name: identity.name, color: identity.color }}
                onProject={publish}
                seq={projectionSeq}
                fileId={fileId}
                catchUp={projectNonce}
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
    case 'saving':
      return 'Saving changes…'
    case 'read-only':
      return 'Read only'
    case 'error':
      return 'Changes not saved'
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
