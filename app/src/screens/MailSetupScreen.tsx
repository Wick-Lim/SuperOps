import React, { useCallback, useEffect, useState } from 'react'
import {
  SafeAreaView, View, Text, Pressable, ScrollView, TextInput, Alert, StyleSheet,
} from 'react-native'
import * as Clipboard from 'expo-clipboard'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import { errorMessage } from '../api/client'
import { mailboxApi } from '../api/mailboxes'
import type { IngestToken, Mailbox, MailDomain } from '../api/mailboxes'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

/**
 * Setting up the shared inbox.
 *
 * WITHOUT THIS SCREEN THE WHOLE PILLAR IS INERT, and not in a way anybody could
 * diagnose. No mailbox means the inbox renders its empty state forever; no
 * verified domain means every reply is written and then refused by the delivery
 * consumer; no ingest token means the inbound webhook answers 401 to the mail
 * provider on every message. Each of those is silent from the app.
 *
 * The three steps are shown IN ORDER and each says what it unblocks, because
 * the failure of any one of them presents as "email does not work" rather than
 * as itself.
 */
export default function MailSetupScreen({ navigation }: { navigation: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const [mailboxes, setMailboxes] = useState<Mailbox[]>([])
  const [domains, setDomains] = useState<MailDomain[]>([])
  const [tokens, setTokens] = useState<IngestToken[]>([])
  const [freshToken, setFreshToken] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!workspace) return
    setError(null)
    try {
      const [mb, dm, tk] = await Promise.all([
        mailboxApi.list(workspace.id),
        mailboxApi.domains(workspace.id),
        mailboxApi.ingestTokens(workspace.id),
      ])
      setMailboxes(mb.data ?? [])
      setDomains(dm.data ?? [])
      setTokens(tk.data ?? [])
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
    }
  }, [workspace])

  useEffect(() => {
    void load()
  }, [load])

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Loading mail setup" />
      </SafeAreaView>
    )
  }
  if (!workspace) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title="Email setup" onBack={() => navigation.goBack()} />
        <ErrorState message="No workspace selected." />
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader title="Email setup" subtitle={workspace.name} onBack={() => navigation.goBack()} />
      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : (
        <ScrollView>
          <ContentColumn>
            <DomainStep workspaceId={workspace.id} domains={domains} onChanged={load} />
            <MailboxStep
              workspaceId={workspace.id}
              mailboxes={mailboxes}
              domains={domains}
              onChanged={load}
            />
            <IngestStep
              workspaceId={workspace.id}
              tokens={tokens}
              fresh={freshToken}
              onFresh={setFreshToken}
              onChanged={load}
            />
          </ContentColumn>
        </ScrollView>
      )}
    </SafeAreaView>
  )
}

/** Step 1. Nothing can be SENT until a domain is verified. */
function DomainStep({
  workspaceId,
  domains,
  onChanged,
}: {
  workspaceId: string
  domains: MailDomain[]
  onChanged: () => void
}) {
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)

  const add = useCallback(async () => {
    if (value.trim() === '') return
    setBusy(true)
    try {
      await mailboxApi.createDomain(workspaceId, value.trim())
      setValue('')
      onChanged()
    } catch (e) {
      Alert.alert('Could not add that domain', errorMessage(e))
    } finally {
      setBusy(false)
    }
  }, [value, workspaceId, onChanged])

  const verify = useCallback(
    async (d: MailDomain) => {
      try {
        const res = await mailboxApi.verifyDomain(d.id)
        if (res.data?.verified) {
          Alert.alert('Verified', `Replies can now be sent from ${d.domain}.`)
          onChanged()
        } else {
          // Not an error. An unpublished record is the normal first attempt,
          // and the reason says which half is missing.
          Alert.alert('Not verified yet', res.data?.reason ?? 'The DNS record was not found.')
        }
      } catch (e) {
        Alert.alert('Could not check that domain', errorMessage(e))
      }
    },
    [onChanged],
  )

  return (
    <Section title="1. Verify a sending domain">
      <Text style={styles.explain}>
        Replies go out from your own address, so we check that you control the domain first. Until
        this is done, replies are saved in the conversation and not delivered.
      </Text>

      <View style={styles.inline}>
        <TextInput
          style={[styles.input, { flex: 1 }]}
          value={value}
          onChangeText={setValue}
          placeholder="yourcompany.com"
          placeholderTextColor={theme.textFaint}
          autoCapitalize="none"
          accessibilityLabel="Domain"
        />
        <Pressable
          onPress={add}
          disabled={busy}
          style={({ pressed }) => [styles.action, (pressed || busy) && styles.pressed]}
          accessibilityRole="button"
        >
          <Text style={styles.actionText}>Add</Text>
        </Pressable>
      </View>

      {domains.map((d) => (
        <View key={d.id} style={styles.item}>
          <View style={styles.itemHead}>
            <Text style={styles.itemTitle}>{d.domain}</Text>
            <Text style={[styles.badge, d.verified_at ? styles.badgeOk : styles.badgeWarn]}>
              {d.verified_at ? 'verified' : 'not verified'}
            </Text>
          </View>
          {!d.verified_at && (
            <>
              <Text style={styles.explain}>
                Publish this TXT record, then check again. It can take a few minutes to appear.
              </Text>
              <CopyRow label="Host" value={d.verify_host} />
              <CopyRow label="Value" value={d.verify_value} />
              <Pressable
                onPress={() => verify(d)}
                style={({ pressed }) => [styles.action, pressed && styles.pressed]}
                accessibilityRole="button"
              >
                <Text style={styles.actionText}>Check now</Text>
              </Pressable>
            </>
          )}
        </View>
      ))}
    </Section>
  )
}

/** Step 2. Without a mailbox the inbox is empty forever. */
function MailboxStep({
  workspaceId,
  mailboxes,
  domains,
  onChanged,
}: {
  workspaceId: string
  mailboxes: Mailbox[]
  domains: MailDomain[]
  onChanged: () => void
}) {
  const [address, setAddress] = useState('')
  const [name, setName] = useState('')
  const [prefix, setPrefix] = useState('')
  const [busy, setBusy] = useState(false)

  const create = useCallback(async () => {
    if (address.trim() === '' || prefix.trim() === '') {
      Alert.alert('A mailbox needs an address and a short prefix', 'The prefix appears in the reference a customer sees, like SUP-14.')
      return
    }
    setBusy(true)
    try {
      await mailboxApi.create(workspaceId, address.trim(), name.trim(), prefix.trim().toUpperCase())
      setAddress('')
      setName('')
      setPrefix('')
      onChanged()
    } catch (e) {
      Alert.alert('Could not create that mailbox', errorMessage(e))
    } finally {
      setBusy(false)
    }
  }, [address, name, prefix, workspaceId, onChanged])

  const verified = domains.filter((d) => d.verified_at)

  return (
    <Section title="2. Add a mailbox">
      <Text style={styles.explain}>
        A mailbox is an address customers write to. Each one has its own queue and its own
        reference numbers.
      </Text>

      {mailboxes.map((mb) => (
        <View key={mb.id} style={styles.item}>
          <Text style={styles.itemTitle}>{mb.address}</Text>
          <Text style={styles.itemMeta}>
            {mb.display_name || 'no display name'} · references {mb.prefix}-1, {mb.prefix}-2, …
          </Text>
        </View>
      ))}

      <TextInput
        style={styles.input}
        value={address}
        onChangeText={setAddress}
        placeholder={verified.length ? `support@${verified[0].domain}` : 'support@yourcompany.com'}
        placeholderTextColor={theme.textFaint}
        autoCapitalize="none"
        accessibilityLabel="Mailbox address"
      />
      <View style={styles.inline}>
        <TextInput
          style={[styles.input, { flex: 1 }]}
          value={name}
          onChangeText={setName}
          placeholder="Support"
          placeholderTextColor={theme.textFaint}
          accessibilityLabel="Display name"
        />
        <TextInput
          style={[styles.input, { width: 96 }]}
          value={prefix}
          onChangeText={setPrefix}
          placeholder="SUP"
          placeholderTextColor={theme.textFaint}
          autoCapitalize="characters"
          accessibilityLabel="Reference prefix"
        />
      </View>
      <Pressable
        onPress={create}
        disabled={busy}
        style={({ pressed }) => [styles.action, (pressed || busy) && styles.pressed]}
        accessibilityRole="button"
      >
        <Text style={styles.actionText}>{busy ? 'Creating' : 'Create mailbox'}</Text>
      </Pressable>
    </Section>
  )
}

/** Step 3. Without a token the provider's webhook is refused on every message. */
function IngestStep({
  workspaceId,
  tokens,
  fresh,
  onFresh,
  onChanged,
}: {
  workspaceId: string
  tokens: IngestToken[]
  fresh: string | null
  onFresh: (t: string | null) => void
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)

  const mint = useCallback(async () => {
    setBusy(true)
    try {
      const res = await mailboxApi.createIngestToken(workspaceId, 'Mail provider')
      onFresh(res.data?.token.token ?? null)
      onChanged()
    } catch (e) {
      Alert.alert('Could not create a token', errorMessage(e))
    } finally {
      setBusy(false)
    }
  }, [workspaceId, onFresh, onChanged])

  const live = tokens.filter((t) => !t.revoked_at)

  return (
    <Section title="3. Connect your mail provider">
      <Text style={styles.explain}>
        Your provider posts incoming mail to this deployment. It authenticates with a token, and
        until one exists every incoming message is rejected.
      </Text>

      {fresh && (
        <View style={styles.secret}>
          {/* SHOWN EXACTLY ONCE. The server stores only a hash, so this is the
              one moment the plaintext exists anywhere it can be copied. */}
          <Text style={styles.secretLabel}>Copy this now — it is not shown again.</Text>
          <CopyRow label="Token" value={fresh} />
          <Pressable onPress={() => onFresh(null)} hitSlop={8} accessibilityRole="button">
            <Text style={styles.link}>Done</Text>
          </Pressable>
        </View>
      )}

      {live.map((t) => (
        <View key={t.id} style={styles.item}>
          <View style={styles.itemHead}>
            <Text style={styles.itemTitle}>{t.name || 'Unnamed token'}</Text>
            <Pressable
              onPress={async () => {
                try {
                  await mailboxApi.revokeIngestToken(t.id)
                  onChanged()
                } catch (e) {
                  Alert.alert('Could not revoke that token', errorMessage(e))
                }
              }}
              hitSlop={8}
              accessibilityRole="button"
            >
              <Text style={styles.remove}>Revoke</Text>
            </Pressable>
          </View>
          <Text style={styles.itemMeta}>Created {new Date(t.created_at).toLocaleDateString()}</Text>
        </View>
      ))}

      <Pressable
        onPress={mint}
        disabled={busy}
        style={({ pressed }) => [styles.action, (pressed || busy) && styles.pressed]}
        accessibilityRole="button"
      >
        <Text style={styles.actionText}>{busy ? 'Creating' : 'Create a token'}</Text>
      </Pressable>
    </Section>
  )
}

function CopyRow({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.copyRow}>
      <Text style={styles.copyLabel}>{label}</Text>
      <Text style={styles.copyValue} numberOfLines={1} selectable>
        {value}
      </Text>
      <Pressable
        onPress={() => void Clipboard.setStringAsync(value)}
        hitSlop={8}
        accessibilityRole="button"
        accessibilityLabel={`Copy ${label}`}
      >
        <Text style={styles.link}>Copy</Text>
      </Pressable>
    </View>
  )
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  explain: { color: theme.textMuted, fontSize: 13, lineHeight: 19, marginBottom: space.sm },
  inline: { flexDirection: 'row', gap: space.xs, alignItems: 'center' },
  input: {
    color: theme.text,
    fontSize: 14,
    backgroundColor: theme.surface,
    borderRadius: 8,
    paddingHorizontal: space.sm,
    paddingVertical: 10,
    marginBottom: space.xs,
  },
  action: {
    minHeight: MIN_TOUCH - 8,
    justifyContent: 'center',
    alignItems: 'center',
    paddingHorizontal: space.md,
    borderRadius: 8,
    backgroundColor: theme.primary,
    marginTop: space.xs,
  },
  actionText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
  pressed: { opacity: 0.7 },

  item: {
    backgroundColor: theme.surface,
    borderRadius: 8,
    padding: space.sm,
    marginTop: space.xs,
  },
  itemHead: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  itemTitle: { color: theme.text, fontSize: 14, fontWeight: '600' },
  itemMeta: { color: theme.textMuted, fontSize: 12, marginTop: 2 },
  badge: { fontSize: 11, fontWeight: '700', paddingHorizontal: 8, paddingVertical: 2, borderRadius: 10 },
  badgeOk: { color: theme.success, backgroundColor: theme.surfaceAlt },
  badgeWarn: { color: theme.danger, backgroundColor: theme.surfaceAlt },
  remove: { color: theme.danger, fontSize: 13 },
  link: { color: theme.accent, fontSize: 13, fontWeight: '600' },

  copyRow: { flexDirection: 'row', alignItems: 'center', gap: space.sm, paddingVertical: 4 },
  copyLabel: { color: theme.textMuted, fontSize: 12, width: 48 },
  copyValue: { color: theme.body, fontSize: 12, flex: 1, fontFamily: 'Menlo' },

  secret: {
    backgroundColor: theme.surfaceAlt,
    borderRadius: 8,
    padding: space.sm,
    marginBottom: space.sm,
  },
  secretLabel: { color: theme.danger, fontSize: 12, fontWeight: '600', marginBottom: 4 },
})
