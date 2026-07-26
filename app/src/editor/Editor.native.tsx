import React from 'react'
import { ScrollView, Text, View, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { space } from '../lib/responsive'
import type * as Y from 'yjs'
import type { Awareness } from 'y-protocols/awareness'
import type { Projection } from './projection'

/**
 * The document on a phone: READ-ONLY, rendered from the CRDT.
 *
 * ROADMAP §3 option (1). TipTap/ProseMirror is DOM-only and there is no native
 * port worth shipping, so the editor pane exists in the web bundle alone and
 * this file is what Metro resolves on iOS and Android. Nothing here imports
 * TipTap, prosemirror or the projection extractor's DOM path — that is the
 * whole point, and the bundle check in CI is what keeps it true.
 *
 * It renders the live Y.Doc rather than the server's stored projection, so a
 * phone in an open room sees other people's edits arrive. The stored projection
 * is the fallback for a document nobody is in.
 *
 * UNKNOWN BLOCK TYPES RENDER AS THEIR TEXT, never as a blank. A block shipped
 * to web before this renderer knows it must still be readable, or the first
 * schema addition makes documents look empty on phones.
 */

export interface EditorProps {
  doc: Y.Doc
  awareness: Awareness
  /** The Drive object, for resolving reference labels against THIS document.
   * Unused on native — the read-only renderer shows a reference as its
   * placeholder rather than resolving it, because resolving costs a request per
   * open on a connection that may be a phone's. */
  fileId?: string
  editable: boolean
  user: { name: string; color: string }
  onProject: (projection: Projection) => void
  seq: number
}

interface Block {
  key: string
  type: string
  level: number
  text: string
}

export default function Editor({ doc }: EditorProps) {
  const blocks = useBlocks(doc)

  if (blocks.length === 0) {
    return (
      <View style={styles.empty}>
        <Text style={styles.emptyText}>This document is empty.</Text>
      </View>
    )
  }

  return (
    <ScrollView contentContainerStyle={styles.page}>
      {blocks.map((b) => (
        <Text key={b.key} style={blockStyle(b)} selectable>
          {b.text}
        </Text>
      ))}
      <View style={styles.notice}>
        <Text style={styles.noticeText}>
          Editing documents is available on the web.
        </Text>
      </View>
    </ScrollView>
  )
}

/** Subscribes to the shared fragment and re-renders on every remote edit. */
function useBlocks(doc: Y.Doc): Block[] {
  const [blocks, setBlocks] = React.useState<Block[]>(() => read(doc))
  React.useEffect(() => {
    const onChange = () => setBlocks(read(doc))
    doc.on('update', onChange)
    return () => doc.off('update', onChange)
  }, [doc])
  return blocks
}

/**
 * Reads the ProseMirror fragment as flat blocks.
 *
 * Deliberately structural rather than schema-aware: it asks each node for its
 * text content and its type, so a node type this build has never heard of comes
 * out as a paragraph of its own words instead of vanishing.
 */
function read(doc: Y.Doc): Block[] {
  const fragment = doc.getXmlFragment('default')
  const out: Block[] = []
  fragment.forEach((node, index) => {
    const el = node as unknown as {
      nodeName?: string
      getAttribute?: (k: string) => string | null
      toString: () => string
    }
    const type = el.nodeName ?? 'paragraph'
    const level = Number(el.getAttribute?.('level') ?? 0) || 0
    const text = stripTags(el.toString())
    if (text.trim() === '' && type !== 'horizontalRule') return
    out.push({ key: `${index}-${type}`, type, level, text })
  })
  return out
}

/** Y.XmlElement.toString() renders markup; the native surface wants the words.
 * A regex is enough because the fragment is ours and shallow — this is not
 * parsing untrusted HTML, it is unwrapping our own nodes. */
function stripTags(markup: string): string {
  return markup
    .replace(/<br\s*\/?>/g, '\n')
    .replace(/<[^>]*>/g, '')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&')
}

function blockStyle(b: Block) {
  if (b.type === 'heading') {
    switch (b.level) {
      case 1:
        return styles.h1
      case 2:
        return styles.h2
      default:
        return styles.h3
    }
  }
  if (b.type === 'codeBlock') return styles.code
  if (b.type === 'blockquote') return styles.quote
  return styles.paragraph
}

const styles = StyleSheet.create({
  page: { padding: space.md, paddingBottom: space.xl },
  paragraph: { color: theme.body, fontSize: 15, lineHeight: 23, marginBottom: space.sm },
  h1: { color: theme.text, fontSize: 24, fontWeight: '700', marginTop: space.md, marginBottom: space.sm },
  h2: { color: theme.text, fontSize: 20, fontWeight: '700', marginTop: space.md, marginBottom: 6 },
  h3: { color: theme.text, fontSize: 17, fontWeight: '600', marginTop: space.sm, marginBottom: 4 },
  code: {
    color: theme.body,
    fontSize: 13,
    fontFamily: 'Menlo',
    backgroundColor: theme.surfaceAlt,
    padding: space.sm,
    borderRadius: 6,
    marginBottom: space.sm,
  },
  quote: {
    color: theme.textMuted,
    fontSize: 15,
    fontStyle: 'italic',
    borderLeftWidth: 3,
    borderLeftColor: theme.border,
    paddingLeft: space.sm,
    marginBottom: space.sm,
  },
  empty: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: space.lg },
  emptyText: { color: theme.textFaint, fontSize: 14 },
  notice: {
    marginTop: space.lg,
    padding: space.sm,
    backgroundColor: theme.surfaceAlt,
    borderRadius: 8,
  },
  noticeText: { color: theme.textMuted, fontSize: 13, textAlign: 'center' },
})
