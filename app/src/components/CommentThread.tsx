import React, { useCallback, useEffect, useState } from 'react'
import { View, Text, Pressable, TextInput, Alert, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { collectPages, errorMessage } from '../api/client'
import { MENTION_PATTERN, commentApi, renderMentions } from '../api/comments'
import type { Comment } from '../api/comments'
import { useUserStore } from '../stores/userStore'
import { space, MIN_TOUCH } from '../lib/responsive'

/**
 * The comment thread, for ANY object.
 *
 * It takes an object type and an id and nothing else. That is the whole point of
 * the shared surface: this component renders comments on an issue, a file, a
 * folder or anything a pillar registers as an object, and it needs no change
 * when one appears.
 */
export default function CommentThread({
  objectType,
  objectId,
  canComment,
  currentUserId,
}: {
  objectType: string
  objectId: string
  /** Whether the viewer may add one. A read-only surface is RENDERED read-only
   * rather than failing on submit — the capability comes from the object's own
   * payload, so this costs no extra request. */
  canComment: boolean
  currentUserId: string
}) {
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [comments, setComments] = useState<Comment[]>([])
  const [replies, setReplies] = useState<Record<string, Comment[]>>({})
  const [draft, setDraft] = useState('')
  const [replyTo, setReplyTo] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  /** The cursor walk hit its page cap, so the count below is a floor. */
  const [truncated, setTruncated] = useState(false)

  const load = useCallback(async () => {
    try {
      // EVERY page. The header prints the list's length as the comment count,
      // so one page of 50 turned a truncation into a wrong number rather than a
      // missing "load more".
      const { items, truncated } = await collectPages<Comment>((cursor) =>
        commentApi.list(objectType, objectId, cursor),
      )
      setComments(items)
      setTruncated(truncated)
      setError(null)
    } catch (e) {
      setError(errorMessage(e))
    }
  }, [objectType, objectId])

  useEffect(() => {
    void load()
  }, [load])

  // Authors, plus everyone mentioned in a body — the store caches across
  // screens, so a busy thread resolves once.
  useEffect(() => {
    const ids = new Set<string>()
    for (const c of comments) {
      if (c.author_id) ids.add(c.author_id)
      // MENTION_PATTERN, not a second regex. This was
      // /<@([0-9a-fA-F-]{36})>/g, which accepts any 36-character run of hex and
      // dashes — so it resolved ids the notifier never treated as mentions.
      // comment.go's own note says two implementations of "what is a mention"
      // is two different sets of people; there were three.
      for (const m of c.body.matchAll(MENTION_PATTERN)) ids.add(m[1])
    }
    ensureUsers([...ids])
  }, [comments, ensureUsers])

  const expand = useCallback(
    async (parentId: string) => {
      if (replies[parentId]) return
      try {
        const res = await commentApi.replies(parentId)
        setReplies((r) => ({ ...r, [parentId]: res.data }))
      } catch (e) {
        Alert.alert('Could not load the replies', errorMessage(e))
      }
    },
    [replies],
  )

  const submit = useCallback(async () => {
    const body = draft.trim()
    if (!body || busy) return
    setBusy(true)
    try {
      await commentApi.create(objectType, objectId, body, replyTo ?? undefined)
      setDraft('')
      // A reply refetches its own list rather than the whole thread: the thread
      // only shows top-level comments and their counts.
      if (replyTo) {
        const res = await commentApi.replies(replyTo)
        setReplies((r) => ({ ...r, [replyTo]: res.data }))
      }
      setReplyTo(null)
      await load()
    } catch (e) {
      Alert.alert('Could not post the comment', errorMessage(e))
    } finally {
      setBusy(false)
    }
  }, [draft, busy, objectType, objectId, replyTo, load])

  const remove = useCallback(
    (c: Comment) => {
      Alert.alert('Delete this comment?', 'The text is removed. Replies to it stay.', [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Delete',
          style: 'destructive',
          onPress: async () => {
            try {
              await commentApi.remove(c.id)
              await load()
            } catch (e) {
              Alert.alert('Could not delete it', errorMessage(e))
            }
          },
        },
      ])
    },
    [load],
  )

  const nameFor = useCallback(
    (id: string) => users[id]?.full_name || users[id]?.username || 'someone',
    [users],
  )

  return (
    <View>
      <Text style={styles.heading}>
        {comments.length === 0
          ? 'No comments'
          : `${comments.length}${truncated ? '+' : ''} comment(s)`}
      </Text>
      {error && <Text style={styles.error}>{error}</Text>}

      {comments.map((c) => (
        <View key={c.id} style={styles.comment}>
          <Row
            comment={c}
            nameFor={nameFor}
            canDelete={!c.deleted && c.author_id === currentUserId}
            onDelete={() => remove(c)}
          />
          {c.reply_count > 0 && !replies[c.id] && (
            <Pressable onPress={() => expand(c.id)} hitSlop={8} accessibilityRole="button">
              <Text style={styles.link}>Show {c.reply_count} repl{c.reply_count === 1 ? 'y' : 'ies'}</Text>
            </Pressable>
          )}
          {(replies[c.id] ?? []).map((r) => (
            <View key={r.id} style={styles.reply}>
              <Row
                comment={r}
                nameFor={nameFor}
                canDelete={!r.deleted && r.author_id === currentUserId}
                onDelete={() => remove(r)}
              />
            </View>
          ))}
          {canComment && (
            <Pressable onPress={() => setReplyTo(c.id)} hitSlop={8} accessibilityRole="button">
              <Text style={styles.link}>Reply</Text>
            </Pressable>
          )}
        </View>
      ))}

      {canComment ? (
        <View style={styles.composer}>
          {replyTo && (
            <View style={styles.replyingTo}>
              <Text style={styles.replyingToText}>Replying</Text>
              <Pressable onPress={() => setReplyTo(null)} hitSlop={8} accessibilityRole="button">
                <Text style={styles.link}>Cancel</Text>
              </Pressable>
            </View>
          )}
          <TextInput
            value={draft}
            onChangeText={setDraft}
            placeholder="Add a comment"
            placeholderTextColor={theme.textDim}
            style={styles.input}
            multiline
            accessibilityLabel="Comment"
          />
          <Pressable
            onPress={submit}
            disabled={busy || draft.trim() === ''}
            style={({ pressed }) => [
              styles.send,
              (busy || draft.trim() === '') && styles.sendDisabled,
              pressed && styles.sendPressed,
            ]}
            accessibilityRole="button"
          >
            <Text style={styles.sendText}>{busy ? 'Posting…' : 'Comment'}</Text>
          </Pressable>
        </View>
      ) : (
        <Text style={styles.readOnly}>You can read this discussion but not add to it.</Text>
      )}
    </View>
  )
}

function Row({
  comment,
  nameFor,
  canDelete,
  onDelete,
}: {
  comment: Comment
  nameFor: (id: string) => string
  canDelete: boolean
  onDelete: () => void
}) {
  return (
    <View style={styles.row}>
      <View style={styles.rowHeader}>
        <Text style={styles.author}>
          {comment.author_id ? nameFor(comment.author_id) : 'A removed account'}
        </Text>
        <Text style={styles.when}>
          {new Date(comment.created_at).toLocaleString()}
          {comment.edited_at ? ' · edited' : ''}
        </Text>
        {canDelete && (
          <Pressable onPress={onDelete} hitSlop={8} accessibilityRole="button">
            <Text style={styles.danger}>Delete</Text>
          </Pressable>
        )}
      </View>
      {comment.deleted ? (
        // A tombstone, not a hole: the replies under it still make sense.
        <Text style={styles.deleted}>This comment was deleted.</Text>
      ) : (
        <Text style={styles.body}>
          {renderMentions(comment.body, nameFor).map((seg, i) => (
            <Text key={i} style={seg.mention ? styles.mention : undefined}>
              {seg.text}
            </Text>
          ))}
        </Text>
      )}
    </View>
  )
}

const styles = StyleSheet.create({
  heading: { color: theme.text, fontWeight: '700', fontSize: 14, marginTop: space.md },
  error: { color: theme.danger, fontSize: 13, paddingVertical: space.xs },
  comment: { paddingVertical: space.sm, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: theme.border },
  reply: { paddingLeft: space.md, borderLeftWidth: 2, borderLeftColor: theme.border, marginTop: space.xs },
  row: { gap: 2 },
  rowHeader: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  author: { color: theme.text, fontWeight: '600', fontSize: 13 },
  when: { color: theme.textFaint, fontSize: 11, flex: 1 },
  body: { color: theme.body, fontSize: 14 },
  mention: { color: theme.accent, fontWeight: '600' },
  deleted: { color: theme.textDim, fontSize: 14, fontStyle: 'italic' },
  danger: { color: theme.danger, fontSize: 12, fontWeight: '600' },
  link: { color: theme.accent, fontSize: 13, fontWeight: '600', paddingTop: space.xs },
  readOnly: { color: theme.textFaint, fontSize: 13, paddingVertical: space.md },

  composer: { paddingVertical: space.md, gap: space.sm },
  replyingTo: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  replyingToText: { color: theme.textMuted, fontSize: 12, flex: 1 },
  input: {
    minHeight: MIN_TOUCH * 2,
    borderWidth: 1,
    borderColor: theme.border,
    borderRadius: 8,
    padding: space.sm,
    color: theme.text,
    backgroundColor: theme.surface,
    textAlignVertical: 'top',
  },
  send: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  sendDisabled: { opacity: 0.4 },
  sendPressed: { opacity: 0.7 },
  sendText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
})
