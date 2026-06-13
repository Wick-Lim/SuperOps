import React from 'react'
import { View, Text, Image, Pressable } from 'react-native'
import type { Message, FileRef } from '../../lib/types'
import { theme, avatarColor } from '../../lib/theme'
import { useUserStore, displayName } from '../../stores/userStore'
import { fileURL } from '../../api/client'
import ReactionBar from './ReactionBar'

interface Props {
  message: Message
  showHeader: boolean
  onLongPress?: (message: Message) => void
  onAddReaction?: (message: Message) => void
}

function formatTime(dateStr: string) {
  const d = new Date(dateStr)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

// --- Minimal inline markdown-ish renderer ---------------------------------
// Supports **bold**, *italic*, `code`, and @mentions (highlighted in accent).
type Token = { text: string; bold?: boolean; italic?: boolean; code?: boolean; mention?: boolean }

function tokenize(input: string): Token[] {
  const tokens: Token[] = []
  // Split on markdown markers and @mentions while keeping the delimiters.
  const re = /(\*\*[^*]+\*\*|\*[^*]+\*|`[^`]+`|@[A-Za-z0-9_.-]+)/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(input)) !== null) {
    if (m.index > last) tokens.push({ text: input.slice(last, m.index) })
    const tok = m[0]
    if (tok.startsWith('**') && tok.endsWith('**')) {
      tokens.push({ text: tok.slice(2, -2), bold: true })
    } else if (tok.startsWith('`') && tok.endsWith('`')) {
      tokens.push({ text: tok.slice(1, -1), code: true })
    } else if (tok.startsWith('*') && tok.endsWith('*')) {
      tokens.push({ text: tok.slice(1, -1), italic: true })
    } else if (tok.startsWith('@')) {
      tokens.push({ text: tok, mention: true })
    } else {
      tokens.push({ text: tok })
    }
    last = m.index + tok.length
  }
  if (last < input.length) tokens.push({ text: input.slice(last) })
  if (tokens.length === 0) tokens.push({ text: input })
  return tokens
}

function MarkdownText({ content }: { content: string }) {
  const tokens = tokenize(content)
  return (
    <Text style={{ color: theme.body, fontSize: 15, lineHeight: 22 }}>
      {tokens.map((t, i) => {
        if (t.code) {
          return (
            <Text
              key={i}
              style={{
                fontFamily: 'monospace',
                fontSize: 13,
                color: theme.accent,
                backgroundColor: theme.surface,
              }}
            >
              {t.text}
            </Text>
          )
        }
        return (
          <Text
            key={i}
            style={{
              fontWeight: t.bold ? '700' : '400',
              fontStyle: t.italic ? 'italic' : 'normal',
              color: t.mention ? theme.accent : theme.body,
            }}
          >
            {t.text}
          </Text>
        )
      })}
    </Text>
  )
}

// --- Attachments -----------------------------------------------------------
function Attachment({ file }: { file: FileRef }) {
  const isImage = (file.content_type || '').startsWith('image/')
  if (isImage) {
    return (
      <Image
        source={{ uri: fileURL(file.id) }}
        style={{ width: 220, height: 160, borderRadius: 10, marginTop: 6, backgroundColor: theme.surface }}
        resizeMode="cover"
      />
    )
  }
  const sizeKb = file.size_bytes ? `${Math.max(1, Math.round(file.size_bytes / 1024))} KB` : ''
  return (
    <View
      style={{
        flexDirection: 'row',
        alignItems: 'center',
        gap: 10,
        marginTop: 6,
        paddingHorizontal: 12,
        paddingVertical: 10,
        backgroundColor: theme.surface,
        borderWidth: 1,
        borderColor: theme.border,
        borderRadius: 10,
        alignSelf: 'flex-start',
        maxWidth: 260,
      }}
    >
      <Text style={{ fontSize: 18 }}>📄</Text>
      <View style={{ flex: 1 }}>
        <Text style={{ color: theme.body, fontSize: 13, fontWeight: '600' }} numberOfLines={1}>
          {file.name || 'file'}
        </Text>
        {!!sizeKb && <Text style={{ color: theme.textFaint, fontSize: 11 }}>{sizeKb}</Text>}
      </View>
    </View>
  )
}

export default function MessageItem({ message, showHeader, onLongPress, onAddReaction }: Props) {
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  React.useEffect(() => {
    ensureUsers([message.user_id])
  }, [message.user_id, ensureUsers])

  const name = displayName(users, message.user_id)
  const initials = (name || message.user_id).slice(0, 2).toUpperCase()
  const color = avatarColor(message.user_id)
  const files = message.files ?? []

  return (
    <Pressable
      onLongPress={() => onLongPress?.(message)}
      delayLongPress={250}
      style={{ flexDirection: 'row', marginTop: showHeader ? 16 : 2, gap: 10 }}
    >
      {showHeader ? (
        <View style={{ width: 36, height: 36, borderRadius: 10, backgroundColor: color, alignItems: 'center', justifyContent: 'center' }}>
          <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>{initials}</Text>
        </View>
      ) : (
        <View style={{ width: 36 }} />
      )}

      <View style={{ flex: 1 }}>
        {showHeader && (
          <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8, marginBottom: 2 }}>
            <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }}>{name}</Text>
            <Text style={{ color: theme.textDim, fontSize: 11 }}>{formatTime(message.created_at)}</Text>
            {message.is_edited && <Text style={{ color: theme.textDim, fontSize: 11 }}>(edited)</Text>}
            {message.is_pinned && <Text style={{ color: theme.warning, fontSize: 11 }}>📌 pinned</Text>}
          </View>
        )}

        {!!message.content && <MarkdownText content={message.content} />}

        {files.map((f) => (
          <Attachment key={f.id} file={f} />
        ))}

        <ReactionBar message={message} onAddReaction={onAddReaction} />

        {message.reply_count > 0 && (
          <Text style={{ color: theme.accent, fontSize: 12, marginTop: 4 }}>
            {message.reply_count} {message.reply_count === 1 ? 'reply' : 'replies'}
          </Text>
        )}
      </View>
    </Pressable>
  )
}
