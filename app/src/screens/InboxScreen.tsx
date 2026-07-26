import React, { useCallback, useEffect, useState } from 'react'
import {
  SafeAreaView, View, Text, Pressable, ScrollView, TextInput, Alert, StyleSheet,
} from 'react-native'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import { collectPages, errorMessage } from '../api/client'
import { mailboxApi } from '../api/mailboxes'
import { formatBytes } from '../api/drive'
import type { Conversation, ConversationState, Mailbox, MailMessage } from '../api/mailboxes'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader } from './internal/ui'

const STATES: { key: ConversationState | 'all'; label: string }[] = [
  { key: 'open', label: 'Open' },
  { key: 'pending', label: 'Waiting' },
  { key: 'closed', label: 'Closed' },
  { key: 'all', label: 'All' },
]

/**
 * The shared inbox.
 *
 * ONE MAILBOX AT A TIME, on purpose. A merged "everything" view reads well in a
 * screenshot and is wrong in use: conversations from support@ and billing@ have
 * different people responsible for them, and merging them hides whose queue is
 * backing up. It is also the permission boundary — an agent is granted on a
 * mailbox — so a merged list would be a list whose contents depend on grants
 * the reader cannot see.
 */
export default function InboxScreen({ navigation }: { navigation: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const [mailboxes, setMailboxes] = useState<Mailbox[]>([])
  const [selected, setSelected] = useState<Mailbox | null>(null)
  const [state, setState] = useState<ConversationState | 'all'>('open')
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [loading, setLoading] = useState(true)
  /** The CONVERSATION list, separately from the mailbox list. Without it,
   * switching mailbox or filter left the previous mailbox's threads on screen
   * as though they belonged to the newly selected one. */
  const [loadingConversations, setLoadingConversations] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!workspace) return
    let cancelled = false
    void mailboxApi
      .list(workspace.id)
      .then((res) => {
        if (cancelled) return
        const list = res.data ?? []
        setMailboxes(list)
        setSelected((cur) => cur ?? list[0] ?? null)
      })
      .catch((e) => !cancelled && setError(errorMessage(e)))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
  }, [workspace])

  const load = useCallback(async () => {
    if (!selected) return
    setError(null)
    // Said out loud. `loading` covered only the mailbox list, so switching
    // mailbox or filter left the PREVIOUS mailbox's conversations on screen —
    // reading as though they belonged to the one just selected.
    setLoadingConversations(true)
    try {
      // EVERY page: a shared inbox that stops at 50 conversations with no
      // "load more" simply loses the older half of the queue.
      const { items } = await collectPages<Conversation>((cursor) =>
        mailboxApi.conversations(selected.id, state === 'all' ? undefined : state, cursor),
      )
      setConversations(items)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoadingConversations(false)
    }
  }, [selected, state])

  useEffect(() => {
    void load()
  }, [load])

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Loading the inbox" />
      </SafeAreaView>
    )
  }

  if (mailboxes.length === 0) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title="Inbox" onBack={() => navigation.goBack()} />
        <ContentColumn>
          <Text style={styles.empty}>
            No mailbox yet. Setting one up takes three steps: verify a sending domain, add an
            address, and connect your mail provider.
          </Text>
          {/* The empty state POINTS AT THE FIX. It used to say "an admin can add
              one in Settings" about a screen that did not exist. */}
          <Pressable
            onPress={() => navigation.navigate('MailSetup')}
            style={({ pressed }) => [styles.setup, pressed && styles.pressed]}
            accessibilityRole="button"
          >
            <Text style={styles.setupText}>Set up email</Text>
          </Pressable>
        </ContentColumn>
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={selected?.display_name || selected?.address || 'Inbox'}
        subtitle={selected?.address}
        onBack={() => navigation.goBack()}
      />

      {mailboxes.length > 1 && (
        <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.chips}>
          {mailboxes.map((mb) => (
            <Pressable
              key={mb.id}
              onPress={() => setSelected(mb)}
              style={[styles.chip, selected?.id === mb.id && styles.chipOn]}
              accessibilityRole="button"
            >
              <Text style={[styles.chipText, selected?.id === mb.id && styles.chipTextOn]}>
                {mb.display_name || mb.address}
              </Text>
            </Pressable>
          ))}
        </ScrollView>
      )}

      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.chips}>
        {STATES.map((s) => (
          <Pressable
            key={s.key}
            onPress={() => setState(s.key)}
            style={[styles.chip, state === s.key && styles.chipOn]}
            accessibilityRole="button"
          >
            <Text style={[styles.chipText, state === s.key && styles.chipTextOn]}>{s.label}</Text>
          </Pressable>
        ))}
      </ScrollView>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : loadingConversations ? (
        <LoadingState label="Loading conversations" />
      ) : conversations.length === 0 ? (
        <ContentColumn>
          <Text style={styles.empty}>Nothing here.</Text>
        </ContentColumn>
      ) : (
        <ScrollView>
          {conversations.map((c) => (
            <Pressable
              key={c.id}
              onPress={() => navigation.navigate('Conversation', { conversationId: c.id })}
              style={({ pressed }) => [styles.row, pressed && styles.pressed]}
              accessibilityRole="button"
              accessibilityLabel={`${c.reference}, ${c.subject || 'no subject'}, from ${c.customer_email}`}
            >
              <View style={styles.rowHead}>
                <Text style={styles.reference}>{c.reference}</Text>
                {/* "Waiting on us" is a column, not a computation — it is what
                    an agent scans the list for. */}
                {c.awaiting_reply && <View style={styles.dot} />}
                <Text style={styles.time}>{new Date(c.last_message_at).toLocaleDateString()}</Text>
              </View>
              <Text style={styles.subject} numberOfLines={1}>
                {c.subject || '(no subject)'}
              </Text>
              <Text style={styles.from} numberOfLines={1}>
                {c.customer_name || c.customer_email}
              </Text>
            </Pressable>
          ))}
        </ScrollView>
      )}
    </SafeAreaView>
  )
}

/** One thread, with the reply box. */
export function ConversationScreen({ navigation, route }: { navigation: any; route: any }) {
  const conversationId: string = route.params?.conversationId
  const [conversation, setConversation] = useState<Conversation | null>(null)
  const [messages, setMessages] = useState<MailMessage[]>([])
  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      const res = await mailboxApi.conversation(conversationId)
      setConversation(res.data?.conversation ?? null)
      setMessages(res.data?.messages ?? [])
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
    }
  }, [conversationId])

  useEffect(() => {
    void load()
  }, [load])

  const send = useCallback(async () => {
    if (draft.trim() === '') return
    setSending(true)
    try {
      await mailboxApi.reply(conversationId, draft.trim())
      setDraft('')
      await load()
    } catch (e) {
      Alert.alert('Could not send that reply', errorMessage(e))
    } finally {
      setSending(false)
    }
  }, [draft, conversationId, load])

  const setState = useCallback(
    async (state: ConversationState) => {
      try {
        const res = await mailboxApi.patch(conversationId, { state })
        setConversation(res.data ?? null)
      } catch (e) {
        Alert.alert('Could not change that conversation', errorMessage(e))
      }
    },
    [conversationId],
  )

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Opening" />
      </SafeAreaView>
    )
  }
  if (error || !conversation) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title="Conversation" onBack={() => navigation.goBack()} />
        <ErrorState message={error ?? 'Not found'} onRetry={load} />
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={conversation.subject || conversation.reference}
        subtitle={`${conversation.reference} · ${conversation.customer_email}`}
        onBack={() => navigation.goBack()}
      />

      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.chips}>
        {(['open', 'pending', 'closed', 'spam'] as ConversationState[]).map((s) => (
          <Pressable
            key={s}
            onPress={() => setState(s)}
            style={[styles.chip, conversation.state === s && styles.chipOn]}
            accessibilityRole="button"
          >
            <Text style={[styles.chipText, conversation.state === s && styles.chipTextOn]}>{s}</Text>
          </Pressable>
        ))}
      </ScrollView>

      <ScrollView style={{ flex: 1 }}>
        <ContentColumn>
          {messages.map((m) => (
            <View
              key={m.id}
              style={[styles.bubble, m.direction === 'outbound' ? styles.ours : styles.theirs]}
            >
              <Text style={styles.bubbleFrom}>
                {m.direction === 'outbound' ? 'You' : m.from_name || m.from_address}
              </Text>
              <Text style={styles.bubbleBody}>{m.body_text || '(no text part)'}</Text>
              {/* AN UNDELIVERED REPLY IS SAID SO. A NULL sent_at on an outbound
                  message means written and not yet sent — most often because the
                  sending domain is not verified — and it looks identical to a
                  delivered one unless the thread says otherwise. */}
              {/* Attachments are Drive objects, so opening one goes through
                  the Drive descriptor and its normal permission check rather
                  than a mail-specific download path. */}
              {(m.attachments ?? []).map((a) => (
                <Pressable
                  key={a.file_id}
                  onPress={() => navigation.navigate('DriveFile', { fileId: a.file_id })}
                  style={({ pressed }) => [styles.attachment, pressed && styles.pressed]}
                  accessibilityRole="button"
                  accessibilityLabel={`Open ${a.name}`}
                >
                  <Text style={styles.attachmentName} numberOfLines={1}>
                    {a.name}
                  </Text>
                  <Text style={styles.attachmentSize}>{formatBytes(a.size_bytes)}</Text>
                </Pressable>
              ))}
              {m.direction === 'outbound' && m.sent_at === null && (
                <Text style={styles.undelivered}>
                  Not delivered yet — the sending domain may not be verified.
                </Text>
              )}
            </View>
          ))}
        </ContentColumn>
      </ScrollView>

      <View style={styles.composer}>
        <TextInput
          style={styles.composerInput}
          value={draft}
          onChangeText={setDraft}
          placeholder="Reply to the customer"
          placeholderTextColor={theme.textFaint}
          multiline
          accessibilityLabel="Reply"
        />
        <Pressable
          onPress={send}
          disabled={sending || draft.trim() === ''}
          style={({ pressed }) => [
            styles.send,
            (pressed || sending || draft.trim() === '') && styles.pressed,
          ]}
          accessibilityRole="button"
        >
          <Text style={styles.sendText}>{sending ? 'Sending' : 'Send'}</Text>
        </Pressable>
      </View>
    </SafeAreaView>
  )
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  empty: { color: theme.textFaint, fontSize: 14, paddingVertical: space.md, lineHeight: 20 },
  setup: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  setupText: { color: theme.primaryText, fontWeight: '600', fontSize: 15 },
  pressed: { opacity: 0.7 },

  chips: { gap: 6, paddingHorizontal: space.md, paddingVertical: space.sm },
  chip: { paddingHorizontal: space.sm, paddingVertical: 8, borderRadius: 16, backgroundColor: theme.surfaceAlt },
  chipOn: { backgroundColor: theme.primary },
  chipText: { color: theme.body, fontSize: 13 },
  chipTextOn: { color: theme.primaryText, fontWeight: '600' },

  row: {
    paddingHorizontal: space.md,
    paddingVertical: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
  },
  rowHead: { flexDirection: 'row', alignItems: 'center', gap: 6 },
  reference: { color: theme.textMuted, fontSize: 12, fontWeight: '700' },
  dot: { width: 8, height: 8, borderRadius: 4, backgroundColor: theme.accent },
  time: { color: theme.textFaint, fontSize: 12, marginLeft: 'auto' },
  subject: { color: theme.text, fontSize: 15, marginTop: 2 },
  from: { color: theme.textMuted, fontSize: 13, marginTop: 2 },

  bubble: { borderRadius: 10, padding: space.sm, marginBottom: space.sm },
  theirs: { backgroundColor: theme.surface },
  ours: { backgroundColor: theme.surfaceAlt },
  bubbleFrom: { color: theme.textMuted, fontSize: 12, marginBottom: 4 },
  bubbleBody: { color: theme.body, fontSize: 14, lineHeight: 20 },
  undelivered: { color: theme.danger, fontSize: 12, marginTop: 6 },
  attachment: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    marginTop: 6,
    paddingHorizontal: space.sm,
    paddingVertical: 6,
    borderRadius: 6,
    backgroundColor: theme.bg,
  },
  attachmentName: { color: theme.accent, fontSize: 13, flex: 1 },
  attachmentSize: { color: theme.textFaint, fontSize: 12 },

  composer: {
    flexDirection: 'row',
    alignItems: 'flex-end',
    gap: space.sm,
    padding: space.sm,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: theme.border,
    backgroundColor: theme.surface,
  },
  composerInput: {
    flex: 1,
    color: theme.text,
    fontSize: 14,
    maxHeight: 140,
    backgroundColor: theme.bg,
    borderRadius: 8,
    paddingHorizontal: space.sm,
    paddingVertical: 10,
  },
  send: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    paddingHorizontal: space.md,
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  sendText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
})
