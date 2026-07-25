import React, { useState } from 'react'
import { ActivityIndicator, Image, Pressable, Text, View } from 'react-native'
import type { Message, FileRef } from '../../lib/types'
import { theme, avatarColor } from '../../lib/theme'
import { useUserStore } from '../../stores/userStore'
import { fileURL } from '../../api/client'
import type { CustomEmoji } from '../../api/emoji'
import ReactionBar from './ReactionBar'
import RichText from './RichText'
import { formatFull, formatTime } from './time'
import { MIN_TOUCH, touchSlop } from '../a11y'

/** Local delivery state of an optimistic row; server messages have none. */
export type SendState = 'sending' | 'failed'

interface Props {
  message: Message
  showHeader: boolean
  /** Non-null on the first message of a calendar day — renders a separator. */
  dayLabel?: string | null
  sendState?: SendState
  customEmoji?: CustomEmoji[]
  onLongPress?: (message: Message) => void
  onAddReaction?: (message: Message) => void
  /** Overrides ReactionBar's store-backed toggle (see its Props). */
  onToggleReaction?: (message: Message, emoji: string, mine: boolean) => void
  onOpenThread?: (message: Message) => void
  onOpenFile?: (file: FileRef) => void
  onRetrySend?: (message: Message) => void
  onDiscardSend?: (message: Message) => void
}

function kb(bytes: number): string {
  if (!bytes) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function DaySeparator({ label }: { label: string }) {
  return (
    <View
      accessibilityRole="header"
      style={{ flexDirection: 'row', alignItems: 'center', gap: 10, marginTop: 20, marginBottom: 4 }}
    >
      <View style={{ flex: 1, height: 1, backgroundColor: theme.border }} />
      <Text style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 0.5 }}>
        {label}
      </Text>
      <View style={{ flex: 1, height: 1, backgroundColor: theme.border }} />
    </View>
  )
}

function FileChip({ file, onPress }: { file: FileRef; onPress?: () => void }) {
  const size = kb(file.size_bytes)
  return (
    <Pressable
      onPress={onPress}
      disabled={!onPress}
      accessibilityRole="button"
      accessibilityLabel={`Attachment ${file.name || 'file'}${size ? `, ${size}` : ''}`}
      accessibilityHint="Downloads the file"
      style={({ pressed }) => ({
        flexDirection: 'row',
        alignItems: 'center',
        gap: 10,
        marginTop: 6,
        paddingHorizontal: 12,
        paddingVertical: 10,
        minHeight: MIN_TOUCH,
        backgroundColor: pressed ? theme.surfaceAlt : theme.surface,
        borderWidth: 1,
        borderColor: theme.border,
        borderRadius: 10,
        alignSelf: 'flex-start',
        maxWidth: 260,
      })}
    >
      <Text style={{ fontSize: 18 }}>📄</Text>
      <View style={{ flex: 1 }}>
        <Text style={{ color: theme.body, fontSize: 13, fontWeight: '600' }} numberOfLines={1}>
          {file.name || 'file'}
        </Text>
        {!!size && <Text style={{ color: theme.textMuted, fontSize: 11 }}>{size}</Text>}
      </View>
    </Pressable>
  )
}

/** Images get a placeholder while loading and fall back to the file chip. */
function ImageAttachment({ file, onPress }: { file: FileRef; onPress?: () => void }) {
  const [status, setStatus] = useState<'loading' | 'ok' | 'error'>('loading')

  if (status === 'error') return <FileChip file={file} onPress={onPress} />

  return (
    <Pressable
      onPress={onPress}
      disabled={!onPress}
      accessibilityRole="imagebutton"
      accessibilityLabel={`Image, ${file.name || 'attachment'}`}
      accessibilityHint="Opens full screen"
      style={{ marginTop: 6, alignSelf: 'flex-start' }}
    >
      <Image
        source={{ uri: fileURL(file.id) }}
        onLoad={() => setStatus('ok')}
        onError={() => setStatus('error')}
        style={{ width: 220, height: 160, borderRadius: 10, backgroundColor: theme.surface }}
        resizeMode="cover"
      />
      {status === 'loading' && (
        <View
          style={{
            position: 'absolute',
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          <ActivityIndicator size="small" color={theme.textMuted} />
        </View>
      )}
    </Pressable>
  )
}

function MessageItemBase({
  message,
  showHeader,
  dayLabel,
  sendState,
  customEmoji,
  onLongPress,
  onAddReaction,
  onToggleReaction,
  onOpenThread,
  onOpenFile,
  onRetrySend,
  onDiscardSend,
}: Props) {
  // Selecting one entry instead of the whole `users` record: subscribing to the
  // map re-rendered every visible row each time any single profile resolved.
  const user = useUserStore((s) => s.users[message.user_id])

  const name = user?.full_name || user?.username || message.user_id.slice(0, 8)
  const initials = name.slice(0, 2).toUpperCase()
  const files = message.files ?? []
  const pending = sendState === 'sending'
  const failed = sendState === 'failed'

  const label = `${name}, ${formatFull(message.created_at)}. ${message.content || 'Attachment'}${
    message.is_edited ? '. Edited' : ''
  }${failed ? '. Not delivered' : pending ? '. Sending' : ''}`

  return (
    <View style={{ opacity: pending ? 0.55 : 1 }}>
      {!!dayLabel && <DaySeparator label={dayLabel} />}

      <View style={{ flexDirection: 'row', marginTop: showHeader ? 16 : 2, gap: 10 }}>
        {showHeader ? (
          <View
            style={{
              width: 36,
              height: 36,
              borderRadius: 10,
              backgroundColor: avatarColor(message.user_id),
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>{initials}</Text>
          </View>
        ) : (
          <View style={{ width: 36 }} />
        )}

        <View style={{ flex: 1 }}>
          {/* Only the text body is the long-press target, so attachments,
              reactions and the thread affordance stay individually reachable
              by a screen reader instead of being collapsed into one node. */}
          <Pressable
            onLongPress={onLongPress ? () => onLongPress(message) : undefined}
            delayLongPress={250}
            accessible
            accessibilityLabel={label}
            accessibilityHint={onLongPress ? 'Double tap and hold for message actions' : undefined}
            accessibilityActions={onLongPress ? [{ name: 'longpress', label: 'Message actions' }] : undefined}
            onAccessibilityAction={(e) => {
              if (e.nativeEvent.actionName === 'longpress') onLongPress?.(message)
            }}
          >
            {showHeader && (
              <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8, marginBottom: 2 }}>
                <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }}>{name}</Text>
                <Text style={{ color: theme.textMuted, fontSize: 11 }}>
                  {formatTime(message.created_at)}
                </Text>
                {message.is_edited && (
                  <Text style={{ color: theme.textMuted, fontSize: 11 }}>(edited)</Text>
                )}
                {message.is_pinned && (
                  <Text style={{ color: theme.warning, fontSize: 11 }}>📌 pinned</Text>
                )}
              </View>
            )}

            {!!message.content && <RichText content={message.content} />}
          </Pressable>

          {files.map((f) =>
            (f.content_type || '').startsWith('image/') ? (
              <ImageAttachment key={f.id} file={f} onPress={onOpenFile ? () => onOpenFile(f) : undefined} />
            ) : (
              <FileChip key={f.id} file={f} onPress={onOpenFile ? () => onOpenFile(f) : undefined} />
            ),
          )}

          {!pending && !failed && (
            <ReactionBar
              message={message}
              onAddReaction={onAddReaction}
              onToggle={onToggleReaction}
              customEmoji={customEmoji}
            />
          )}

          {message.reply_count > 0 && (
            <Pressable
              onPress={onOpenThread ? () => onOpenThread(message) : undefined}
              disabled={!onOpenThread}
              hitSlop={touchSlop(24)}
              accessibilityRole="button"
              accessibilityLabel={`${message.reply_count} ${
                message.reply_count === 1 ? 'reply' : 'replies'
              }. Open thread`}
              style={{ marginTop: 4, alignSelf: 'flex-start', paddingVertical: 2 }}
            >
              <Text style={{ color: theme.accent, fontSize: 12, fontWeight: '600' }}>
                💬 {message.reply_count} {message.reply_count === 1 ? 'reply' : 'replies'}
              </Text>
            </Pressable>
          )}

          {failed && (
            <View style={{ flexDirection: 'row', alignItems: 'center', gap: 12, marginTop: 4 }}>
              <Text style={{ color: theme.danger, fontSize: 12 }} accessibilityLiveRegion="polite">
                Not delivered
              </Text>
              <Pressable
                onPress={onRetrySend ? () => onRetrySend(message) : undefined}
                hitSlop={touchSlop(24)}
                accessibilityRole="button"
                accessibilityLabel="Retry sending this message"
              >
                <Text style={{ color: theme.accent, fontSize: 12, fontWeight: '700' }}>Retry</Text>
              </Pressable>
              <Pressable
                onPress={onDiscardSend ? () => onDiscardSend(message) : undefined}
                hitSlop={touchSlop(24)}
                accessibilityRole="button"
                accessibilityLabel="Discard this message"
              >
                <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '600' }}>Discard</Text>
              </Pressable>
            </View>
          )}
        </View>
      </View>
    </View>
  )
}

/**
 * Memoized: the list re-renders on every store write (a reaction, a presence
 * ping, one resolved profile) and previously re-rendered every visible row with
 * it. All callbacks handed in are `useCallback`-stable, so the default shallow
 * compare is enough.
 */
export default React.memo(MessageItemBase)
