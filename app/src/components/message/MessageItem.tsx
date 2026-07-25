import React, { useCallback, useState } from 'react'
import { ActivityIndicator, Image, Pressable, Text, View } from 'react-native'
import type { GestureResponderEvent, PointerEvent as RNPointerEvent } from 'react-native'
import type { Message, FileRef } from '../../lib/types'
import { theme, avatarColor } from '../../lib/theme'
import { useUserStore } from '../../stores/userStore'
import { fileURL } from '../../api/client'
import type { CustomEmoji } from '../../api/emoji'
import { CONTENT_MAX_WIDTH, space, type Tier } from '../../lib/responsive'
import ReactionBar from './ReactionBar'
import RichText from './RichText'
import { anchorFrom, type Anchor } from './anchor'
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
  /**
   * Layout tier of the window, from `useResponsive()`. Resolved once by
   * `MessageList` and handed down rather than subscribed to per row: fifty
   * visible rows would otherwise register fifty dimension listeners.
   */
  tier?: Tier
  /** Minimum interactive size at this tier. 44 on a thumb, 36 under a pointer. */
  minTouch?: number
  onLongPress?: (message: Message) => void
  /** `anchor` is the pointer position, so the picker can open beside it. */
  onAddReaction?: (message: Message, anchor?: Anchor) => void
  /** Overrides ReactionBar's store-backed toggle (see its Props). */
  onToggleReaction?: (message: Message, emoji: string, mine: boolean) => void
  onOpenThread?: (message: Message) => void
  /** Pin/unpin. Only wired where the hover toolbar can act on the channel. */
  onPin?: (message: Message) => void
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

function DaySeparator({ label, dense }: { label: string; dense: boolean }) {
  return (
    <View
      accessibilityRole="header"
      style={{
        flexDirection: 'row',
        alignItems: 'center',
        gap: 10,
        marginTop: dense ? space.lg : 20,
        marginBottom: space.xs,
      }}
    >
      <View style={{ flex: 1, height: 1, backgroundColor: theme.border }} />
      <Text style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 0.5 }}>
        {label}
      </Text>
      <View style={{ flex: 1, height: 1, backgroundColor: theme.border }} />
    </View>
  )
}

function FileChip({
  file,
  onPress,
  minTouch,
}: {
  file: FileRef
  onPress?: () => void
  minTouch: number
}) {
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
        paddingVertical: 8,
        minHeight: minTouch,
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
function ImageAttachment({
  file,
  onPress,
  minTouch,
}: {
  file: FileRef
  onPress?: () => void
  minTouch: number
}) {
  const [status, setStatus] = useState<'loading' | 'ok' | 'error'>('loading')

  if (status === 'error') return <FileChip file={file} onPress={onPress} minTouch={minTouch} />

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

/**
 * One button in the hover toolbar.
 *
 * It reports its own focus so the toolbar stays visible while the keyboard walks
 * across it: the toolbar is revealed by hover OR focus, and focus moving from
 * the message body to the first button would otherwise hide the very control
 * that is about to be activated.
 */
function ToolbarButton({
  glyph,
  label,
  size,
  onPress,
  onFocus,
  onBlur,
}: {
  glyph: string
  label: string
  size: number
  onPress: (e: GestureResponderEvent) => void
  onFocus: () => void
  onBlur: () => void
}) {
  return (
    <Pressable
      onPress={onPress}
      onFocus={onFocus}
      onBlur={onBlur}
      accessibilityRole="button"
      accessibilityLabel={label}
      style={({ pressed }) => ({
        width: size,
        height: size,
        alignItems: 'center',
        justifyContent: 'center',
        backgroundColor: pressed ? theme.surfaceAlt : 'transparent',
      })}
    >
      <Text style={{ fontSize: 15 }}>{glyph}</Text>
    </Pressable>
  )
}

function MessageItemBase({
  message,
  showHeader,
  dayLabel,
  sendState,
  customEmoji,
  tier = 'compact',
  minTouch = MIN_TOUCH,
  onLongPress,
  onAddReaction,
  onToggleReaction,
  onOpenThread,
  onPin,
  onOpenFile,
  onRetrySend,
  onDiscardSend,
}: Props) {
  // Selecting one entry instead of the whole `users` record: subscribing to the
  // map re-rendered every visible row each time any single profile resolved.
  const user = useUserStore((s) => s.users[message.user_id])

  const [hovered, setHovered] = useState(false)
  // A count, not a boolean: moving focus from the body to a toolbar button fires
  // blur before focus, and a boolean would flicker the toolbar out from under
  // the keyboard between the two events.
  const [focusDepth, setFocusDepth] = useState(0)
  // `onPointerEnter`/`Leave` on the row rather than Pressable's `onHoverIn`:
  // Pressable contains its hover, so a nested Pressable (the message body, or a
  // toolbar button) ENDS the ancestor's hover — the toolbar would vanish the
  // moment the mouse moved toward it. Pointer enter/leave are subtree events and
  // treat the whole row as one target.
  const hoverIn = useCallback((e: RNPointerEvent) => {
    // A tap is not a hover; the long-press path stays the one for touch.
    if (e.nativeEvent.pointerType === 'touch') return
    setHovered(true)
  }, [])
  const hoverOut = useCallback(() => setHovered(false), [])
  const focusIn = useCallback(() => setFocusDepth((n) => n + 1), [])
  const focusOut = useCallback(() => setFocusDepth((n) => (n > 0 ? n - 1 : 0)), [])

  const name = user?.full_name || user?.username || message.user_id.slice(0, 8)
  const initials = name.slice(0, 2).toUpperCase()
  const files = message.files ?? []
  const pending = sendState === 'sending'
  const failed = sendState === 'failed'

  /**
   * Everything below the phone breakpoint keeps the thumb layout untouched:
   * 44px targets, no hover, actions behind the long press.
   */
  const pointer = tier !== 'compact'
  const avatar = pointer ? 32 : 36
  // Wide enough for the "09:14" that replaces the avatar on a hovered
  // continuation row, which is space a phone does not have to spend.
  const gutter = pointer ? 44 : 36
  const hasToolbar = pointer && !pending && !failed && !!(onAddReaction || onOpenThread || onPin || onLongPress)
  const revealed = hovered || focusDepth > 0

  const label = `${name}, ${formatFull(message.created_at)}. ${message.content || 'Attachment'}${
    message.is_edited ? '. Edited' : ''
  }${failed ? '. Not delivered' : pending ? '. Sending' : ''}`

  const body = (
    <>
      {showHeader ? (
        <View style={{ width: gutter }}>
          <View
            style={{
              width: avatar,
              height: avatar,
              borderRadius: 10,
              backgroundColor: avatarColor(message.user_id),
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>{initials}</Text>
          </View>
        </View>
      ) : (
        <View
          // Decorative: the same timestamp is already in the row's label, so
          // announcing it again would read every grouped message twice.
          accessibilityElementsHidden
          importantForAccessibility="no-hide-descendants"
          style={{ width: gutter, alignItems: 'flex-end', paddingRight: pointer ? 6 : 0 }}
        >
          {pointer && revealed ? (
            <Text style={{ color: theme.textDim, fontSize: 10, lineHeight: 22 }}>
              {formatTime(message.created_at)}
            </Text>
          ) : null}
        </View>
      )}

      <View
        style={{
          flex: 1,
          // A line of text that runs the full width of a 1200px pane is genuinely
          // hard to read. Cap the measure and leave the slack on the right — the
          // conversation stays left-aligned against the sidebar.
          maxWidth: pointer ? CONTENT_MAX_WIDTH : undefined,
        }}
      >
        {/* The body comes first in the tree so that Tab reaches the message
            before the actions that operate on it, and so the absolutely
            positioned toolbar below paints over it rather than under.

            Only the text body is the long-press target, so attachments,
            reactions and the thread affordance stay individually reachable
            by a screen reader instead of being collapsed into one node.
            It is also the row's focus stop: focusing it reveals the toolbar,
            which is what keeps the hover actions reachable from a keyboard. */}
        <Pressable
          onLongPress={onLongPress ? () => onLongPress(message) : undefined}
          delayLongPress={250}
          focusable={hasToolbar ? true : undefined}
          onFocus={focusIn}
          onBlur={focusOut}
          accessible
          accessibilityLabel={label}
          accessibilityHint={onLongPress ? 'Double tap and hold for message actions' : undefined}
          accessibilityActions={onLongPress ? [{ name: 'longpress', label: 'Message actions' }] : undefined}
          onAccessibilityAction={(e) => {
            if (e.nativeEvent.actionName === 'longpress') onLongPress?.(message)
          }}
        >
          {showHeader && (
            <View
              style={{
                flexDirection: 'row',
                alignItems: 'baseline',
                // The name and the time can sit apart when there is room; on a
                // phone they are competing for the same 300px.
                gap: pointer ? space.md : space.sm,
                marginBottom: 2,
              }}
            >
              <Text style={{ color: theme.text, fontSize: pointer ? 15 : 14, fontWeight: '600' }}>
                {name}
              </Text>
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

        {hasToolbar && (
          <View
            // Invisible until revealed, but still in the tree so a Tab can reach
            // it; `none` keeps it from swallowing clicks while it is invisible.
            // Not hidden from assistive tech when invisible, deliberately: a
            // focusable control inside an aria-hidden container is worse than a
            // verbose one, and screen-reader users also have the message's
            // "Message actions" accessibility action.
            pointerEvents={revealed ? 'auto' : 'none'}
            accessibilityRole="toolbar"
            accessibilityLabel="Message actions"
            style={{
              position: 'absolute',
              top: -Math.round(minTouch / 2),
              right: 0,
              zIndex: 2,
              opacity: revealed ? 1 : 0,
              flexDirection: 'row',
              borderRadius: 10,
              borderWidth: 1,
              borderColor: theme.borderStrong,
              backgroundColor: theme.surface,
              overflow: 'hidden',
            }}
          >
            {onAddReaction && (
              <ToolbarButton
                glyph="😀"
                label="Add reaction"
                size={minTouch}
                onFocus={focusIn}
                onBlur={focusOut}
                onPress={(e) => onAddReaction(message, anchorFrom(e))}
              />
            )}
            {onOpenThread && (
              <ToolbarButton
                glyph="💬"
                label="Reply in thread"
                size={minTouch}
                onFocus={focusIn}
                onBlur={focusOut}
                onPress={() => onOpenThread(message)}
              />
            )}
            {onPin && (
              <ToolbarButton
                glyph="📌"
                label={message.is_pinned ? 'Unpin from channel' : 'Pin to channel'}
                size={minTouch}
                onFocus={focusIn}
                onBlur={focusOut}
                onPress={() => onPin(message)}
              />
            )}
            {onLongPress && (
              <ToolbarButton
                glyph="⋯"
                label="More message actions"
                size={minTouch}
                onFocus={focusIn}
                onBlur={focusOut}
                onPress={() => onLongPress(message)}
              />
            )}
          </View>
        )}

        {files.map((f) =>
          (f.content_type || '').startsWith('image/') ? (
            <ImageAttachment
              key={f.id}
              file={f}
              minTouch={minTouch}
              onPress={onOpenFile ? () => onOpenFile(f) : undefined}
            />
          ) : (
            <FileChip
              key={f.id}
              file={f}
              minTouch={minTouch}
              onPress={onOpenFile ? () => onOpenFile(f) : undefined}
            />
          ),
        )}

        {!pending && !failed && (
          <ReactionBar
            message={message}
            // With a pointer, "add reaction" already sits in the hover toolbar,
            // so the standing ＋ chip is only worth its row next to reactions
            // that actually exist.
            onAddReaction={
              hasToolbar && (message.reactions?.length ?? 0) === 0 ? undefined : onAddReaction
            }
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
    </>
  )

  const rowStyle = {
    flexDirection: 'row' as const,
    // Groups need less air when the rows themselves are tighter, and a screen
    // with more room should show more of the conversation, not the same amount
    // spread further apart.
    marginTop: showHeader ? (pointer ? 10 : 16) : 2,
    gap: pointer ? space.md : 10,
  }

  return (
    <View style={{ opacity: pending ? 0.55 : 1 }}>
      {!!dayLabel && <DaySeparator label={dayLabel} dense={pointer} />}

      {/* The hover target is the whole row, so it still counts as hovered while
          the mouse travels from the message onto the toolbar floating above it. */}
      <View
        style={rowStyle}
        onPointerEnter={pointer ? hoverIn : undefined}
        onPointerLeave={pointer ? hoverOut : undefined}
      >
        {body}
      </View>
    </View>
  )
}

/**
 * Memoized: the list re-renders on every store write (a reaction, a presence
 * ping, one resolved profile) and previously re-rendered every visible row with
 * it. All callbacks handed in are `useCallback`-stable, and `tier`/`minTouch`
 * are scalars, so the default shallow compare is enough.
 */
export default React.memo(MessageItemBase)
