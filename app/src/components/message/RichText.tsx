import React, { useMemo } from 'react'
import { Alert, Linking, Text, View } from 'react-native'
import { theme } from '../../lib/theme'

/**
 * Message body renderer.
 *
 * The backend stamps every message `content_type: "markdown"`
 * (backend/internal/message/handler.go). The previous renderer was a single
 * regex over `**bold**`, `*italic*`, `` `code` `` and `@word`; URLs were plain
 * text, so a shared link could not be opened at all. This handles fenced code,
 * blockquotes, bullet/ordered lists, and inline bold/italic/strikethrough/code
 * plus tappable links.
 *
 * It is intentionally line-oriented and allocation-light: it runs for every
 * visible row on every content change.
 */

type Block =
  | { t: 'code'; v: string }
  | { t: 'quote'; v: string }
  | { t: 'list'; ordered: boolean; items: string[] }
  | { t: 'para'; v: string }

type Inline =
  | { t: 'text'; v: string }
  | { t: 'bold'; v: string }
  | { t: 'italic'; v: string }
  | { t: 'strike'; v: string }
  | { t: 'code'; v: string }
  | { t: 'link'; v: string; href: string }
  | { t: 'mention'; v: string }

const FENCE = /^\s*```/
const QUOTE = /^\s*>\s?(.*)$/
const BULLET = /^\s*[-*+]\s+(.+)$/
const ORDERED = /^\s*\d+[.)]\s+(.+)$/

// Underscore emphasis is deliberately absent: `snake_case_names` and URLs with
// underscores are far more common in a chat log than `_italics_`.
const INLINE =
  /(\*\*[^*\n]+\*\*|~~[^~\n]+~~|`[^`\n]+`|\*[^*\s][^*\n]*\*|(?:https?:\/\/|www\.)[^\s<>]+|@[A-Za-z0-9._-]+)/g

function parseBlocks(input: string): Block[] {
  const lines = input.split('\n')
  const blocks: Block[] = []
  let para: string[] = []

  const flushPara = () => {
    if (para.length) {
      blocks.push({ t: 'para', v: para.join('\n') })
      para = []
    }
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]

    if (FENCE.test(line)) {
      flushPara()
      const body: string[] = []
      i++
      while (i < lines.length && !FENCE.test(lines[i])) {
        body.push(lines[i])
        i++
      }
      blocks.push({ t: 'code', v: body.join('\n') })
      continue
    }

    const quote = QUOTE.exec(line)
    if (quote) {
      flushPara()
      const body = [quote[1]]
      while (i + 1 < lines.length) {
        const next = QUOTE.exec(lines[i + 1])
        if (!next) break
        body.push(next[1])
        i++
      }
      blocks.push({ t: 'quote', v: body.join('\n') })
      continue
    }

    const bullet = BULLET.exec(line)
    const ordered = bullet ? null : ORDERED.exec(line)
    if (bullet || ordered) {
      flushPara()
      const isOrdered = !bullet
      const items = [(bullet ?? ordered)![1]]
      while (i + 1 < lines.length) {
        const next = isOrdered ? ORDERED.exec(lines[i + 1]) : BULLET.exec(lines[i + 1])
        if (!next) break
        items.push(next[1])
        i++
      }
      blocks.push({ t: 'list', ordered: isOrdered, items })
      continue
    }

    if (line.trim() === '') {
      flushPara()
      continue
    }
    para.push(line)
  }

  flushPara()
  if (blocks.length === 0) blocks.push({ t: 'para', v: input })
  return blocks
}

/** Trailing punctuation is sentence punctuation, not part of the URL. */
function splitTrailing(url: string): [string, string] {
  let end = url.length
  while (end > 0) {
    const c = url[end - 1]
    if ('.,;:!?"\''.includes(c)) {
      end--
      continue
    }
    // A closing paren only belongs to the URL if it opened inside it.
    if (c === ')' && !url.slice(0, end).includes('(')) {
      end--
      continue
    }
    break
  }
  return [url.slice(0, end), url.slice(end)]
}

function parseInline(input: string): Inline[] {
  const out: Inline[] = []
  let last = 0
  let m: RegExpExecArray | null
  INLINE.lastIndex = 0
  while ((m = INLINE.exec(input)) !== null) {
    if (m.index > last) out.push({ t: 'text', v: input.slice(last, m.index) })
    const tok = m[0]
    last = m.index + tok.length

    if (tok.startsWith('**')) out.push({ t: 'bold', v: tok.slice(2, -2) })
    else if (tok.startsWith('~~')) out.push({ t: 'strike', v: tok.slice(2, -2) })
    else if (tok.startsWith('`')) out.push({ t: 'code', v: tok.slice(1, -1) })
    else if (tok.startsWith('*')) out.push({ t: 'italic', v: tok.slice(1, -1) })
    else if (tok.startsWith('@')) out.push({ t: 'mention', v: tok })
    else {
      const [url, trailing] = splitTrailing(tok)
      if (url) out.push({ t: 'link', v: url, href: url.startsWith('www.') ? `https://${url}` : url })
      if (trailing) out.push({ t: 'text', v: trailing })
    }
  }
  if (last < input.length) out.push({ t: 'text', v: input.slice(last) })
  return out
}

function openLink(href: string) {
  Linking.openURL(href).catch(() => Alert.alert('Error', 'Could not open that link.'))
}

const codeStyle = {
  fontFamily: 'monospace' as const,
  fontSize: 13,
  color: theme.accent,
  backgroundColor: theme.surface,
}

function InlineRun({ tokens }: { tokens: Inline[] }) {
  return (
    <>
      {tokens.map((t, i) => {
        switch (t.t) {
          case 'code':
            return (
              <Text key={i} style={codeStyle}>
                {t.v}
              </Text>
            )
          case 'link':
            return (
              <Text
                key={i}
                onPress={() => openLink(t.href)}
                accessibilityRole="link"
                accessibilityLabel={`Link, ${t.v}`}
                style={{ color: theme.accent, textDecorationLine: 'underline' }}
              >
                {t.v}
              </Text>
            )
          case 'mention':
            return (
              <Text key={i} style={{ color: theme.accent, fontWeight: '600' }}>
                {t.v}
              </Text>
            )
          case 'bold':
            return (
              <Text key={i} style={{ fontWeight: '700' }}>
                {t.v}
              </Text>
            )
          case 'italic':
            return (
              <Text key={i} style={{ fontStyle: 'italic' }}>
                {t.v}
              </Text>
            )
          case 'strike':
            return (
              <Text key={i} style={{ textDecorationLine: 'line-through' }}>
                {t.v}
              </Text>
            )
          default:
            return <Text key={i}>{t.v}</Text>
        }
      })}
    </>
  )
}

const bodyStyle = { color: theme.body, fontSize: 15, lineHeight: 22 }

function BlockView({ block }: { block: Block }) {
  switch (block.t) {
    case 'code':
      return (
        <View
          style={{
            marginTop: 4,
            padding: 10,
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.border,
            borderRadius: 8,
          }}
        >
          <Text style={{ fontFamily: 'monospace', fontSize: 13, color: theme.body }}>{block.v}</Text>
        </View>
      )
    case 'quote':
      return (
        <View
          style={{
            marginTop: 4,
            paddingLeft: 10,
            borderLeftWidth: 3,
            borderLeftColor: theme.borderStrong,
          }}
        >
          <Text style={{ ...bodyStyle, color: theme.textMuted }}>
            <InlineRun tokens={parseInline(block.v)} />
          </Text>
        </View>
      )
    case 'list':
      return (
        <View style={{ marginTop: 2 }}>
          {block.items.map((item, i) => (
            <View key={i} style={{ flexDirection: 'row', gap: 6 }}>
              <Text style={{ ...bodyStyle, color: theme.textMuted }}>
                {block.ordered ? `${i + 1}.` : '•'}
              </Text>
              <Text style={{ ...bodyStyle, flex: 1 }}>
                <InlineRun tokens={parseInline(item)} />
              </Text>
            </View>
          ))}
        </View>
      )
    default:
      return (
        <Text style={bodyStyle}>
          <InlineRun tokens={parseInline(block.v)} />
        </Text>
      )
  }
}

function RichTextBase({ content }: { content: string }) {
  const blocks = useMemo(() => parseBlocks(content), [content])
  // The overwhelmingly common case is one paragraph — skip the wrapper View so
  // a plain message costs exactly one Text, as it did before.
  if (blocks.length === 1) return <BlockView block={blocks[0]} />
  return (
    <View style={{ gap: 4 }}>
      {blocks.map((b, i) => (
        <BlockView key={i} block={b} />
      ))}
    </View>
  )
}

export default React.memo(RichTextBase)
