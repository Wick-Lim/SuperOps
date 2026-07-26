import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Pressable, ScrollView, Alert, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { driveApi } from '../api/drive'
import type { Share, ShareLink, ShareObjectType, LinkObjectType } from '../api/drive'
import { useUserStore } from '../stores/userStore'
import { space, MIN_TOUCH } from '../lib/responsive'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

/** Share links point at a folder or a file; the server answers 400 for the
 * rest. A type predicate rather than a plain boolean so a link call on a mailbox
 * is a compile error instead of a 400 found at runtime. */
function isLinkable(t: ShareObjectType): t is LinkObjectType {
  return t === 'folder' || t === 'file'
}

/**
 * Sharing.
 *
 * Two mechanisms, and the difference is not cosmetic:
 *
 *   PEOPLE — an ordinary grant, inherited by everything inside. Only for members
 *   of this workspace; the server refuses anyone else, because a grant to a
 *   stranger puts them in an object graph they have no other route into.
 *
 *   LINKS — anyone holding the URL. The token is shown ONCE and is not
 *   recoverable, so this screen puts it in front of the user the moment it
 *   exists rather than assuming it can be fetched again.
 */
export default function DriveShareScreen({ navigation, route }: { navigation: any; route: any }) {
  // NOT NARROWED TO folder|file: this screen is also reached by the deep link
  // `drive/:objectType/:objectId/share`, where the value is whatever is in the
  // URL. The server's sharing surface deliberately covers mailboxes and
  // conversations too — a mailbox with no grants is reachable by nobody — and
  // this screen is the only place in the app that can grant on one.
  const objectType: ShareObjectType = route.params?.objectType ?? 'file'
  const objectId: string = route.params?.objectId
  const name: string = route.params?.name ?? 'this item'
  // The user store already resolves ids to names and caches them across
  // screens, so this needs no fetch of its own.
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [shares, setShares] = useState<Share[]>([])
  const [links, setLinks] = useState<ShareLink[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [freshToken, setFreshToken] = useState<string | null>(null)
  const [linksError, setLinksError] = useState<string | null>(null)

  // Share links exist for a folder or a file only; the server answers 400 for
  // anything else, and tells you to grant a person or a group instead.
  //
  // `load` calls isLinkable(objectType) again rather than reading this. An
  // earlier comment claimed that was required for the narrowing to reach the
  // request — it is not: TypeScript has narrowed through a const boolean
  // holding a type-predicate result since 4.4, and this repo is on 5.9. Calling
  // the predicate is still what the callback does, because `linkable` is then
  // not a dependency of it.
  const linkable = isLinkable(objectType)

  const load = useCallback(async () => {
    setError(null)
    try {
      // TWO INDEPENDENT REQUESTS, NOT ONE Promise.all.
      //
      // They were awaited together, so the links call failing took the whole
      // screen down with it — and it ALWAYS fails for a mailbox or a
      // conversation. The grants had loaded; the user saw an error instead of
      // them, and the only surface for granting on a shared inbox was
      // unreachable. Whether an object has links is not a reason to hide who
      // can already open it.
      const s = await driveApi.shares(objectType, objectId)
      setShares(s.data)
    } catch (e) {
      setError(errorMessage(e))
      setLoading(false)
      // Nothing below can be shown next to a failed grants list, and asking for
      // links after the first request already failed is a wasted round trip.
      return
    }
    setLoading(false)

    if (!isLinkable(objectType)) {
      setLinks([])
      setLinksError(null)
      return
    }
    try {
      const l = await driveApi.links(objectType, objectId)
      setLinks(l.data)
      setLinksError(null)
    } catch (e) {
      // NOT SILENT. This branch is reached only for a folder or a file — the
      // case where links are supposed to work — so a failure here is
      // information. Swallowing it rendered "No links yet.", an affirmative
      // statement that nobody has link access, out of a 500 or a dropped
      // connection. Someone auditing who can reach a file would believe it.
      setLinks([])
      setLinksError(errorMessage(e))
    }
  }, [objectType, objectId])

  useEffect(() => {
    void load()
  }, [load])

  // Resolve the people named by the grants. Ids that are not users — a link, the
  // workspace itself — are filtered out rather than requested, or the store
  // would mark them permanently failed.
  useEffect(() => {
    ensureUsers(shares.filter((s) => s.subject_type === 'user').map((s) => s.subject_id))
  }, [shares, ensureUsers])

  const createLink = useCallback(async () => {
    // Unreachable through the UI — the button is behind `linkable` — but the
    // narrowing has to be here for the call to type-check, and a guard is the
    // honest way to get it rather than a cast.
    if (!isLinkable(objectType)) return
    try {
      const res = await driveApi.createLink(objectType, objectId, { capability: 'read' })
      // Shown immediately, and kept on screen until the user copies it. There is
      // no second chance: the server stores only the hash.
      setFreshToken(res.data.token)
      await load()
    } catch (e) {
      Alert.alert('Could not create a link', errorMessage(e))
    }
  }, [objectType, objectId, load])

  const revokeLink = useCallback(
    (link: ShareLink) => {
      Alert.alert(
        'Revoke this link?',
        'Anyone holding the URL loses access immediately. This cannot be undone.',
        [
          { text: 'Cancel', style: 'cancel' },
          {
            text: 'Revoke',
            style: 'destructive',
            onPress: async () => {
              try {
                await driveApi.revokeLink(link.id)
                await load()
              } catch (e) {
                Alert.alert('Could not revoke the link', errorMessage(e))
              }
            },
          },
        ],
      )
    },
    [load],
  )

  const unshare = useCallback(
    async (share: Share) => {
      try {
        await driveApi.unshare(objectType, objectId, share.subject_id)
        await load()
      } catch (e) {
        Alert.alert('Could not remove access', errorMessage(e))
      }
    },
    [objectType, objectId, load],
  )

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Loading sharing" />
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader title="Share" subtitle={name} onBack={() => navigation.goBack()} />
      <ScrollView>
        <ContentColumn>
          {error && <ErrorState message={error} onRetry={load} />}

          {freshToken && (
            <View style={styles.tokenBox}>
              <Text style={styles.tokenTitle}>Copy this token now</Text>
              <Text style={styles.tokenBody} selectable>
                {freshToken}
              </Text>
              <Text style={styles.tokenNote}>
                It is shown once and cannot be recovered.
                {'\n\n'}
                A holder cannot open {name} with it yet: the server validates the
                token and describes what it points at, but nothing turns that
                into access. Grant a person or a group instead.
              </Text>
              {/* No clipboard dependency: the text above is selectable on every
                  platform, and a "Copy" button that silently did nothing on one
                  of them would be worse than none. */}
              <Pressable
                style={styles.tokenButton}
                accessibilityRole="button"
                onPress={() => setFreshToken(null)}
              >
                <Text style={styles.tokenButtonText}>I have copied it</Text>
              </Pressable>
            </View>
          )}

          <Section title="People with access">
            {shares.length === 0 ? (
              <Text style={styles.empty}>
                Everyone in the workspace can already open this. Share to give someone more than
                that.
              </Text>
            ) : (
              shares.map((s) => (
                <View key={`${s.subject_type}:${s.subject_id}`} style={styles.row}>
                  <View style={{ flex: 1 }}>
                    <Text style={styles.rowTitle}>{nameFor(users, s)}</Text>
                    <Text style={styles.rowMeta}>{s.capability}</Text>
                  </View>
                  {s.subject_type === 'user' && (
                    <Pressable onPress={() => unshare(s)} hitSlop={8} accessibilityRole="button">
                      <Text style={styles.danger}>Remove</Text>
                    </Pressable>
                  )}
                </View>
              ))
            )}
          </Section>

          <Section title="Links">
            {!linkable ? (
              <Text style={styles.empty}>
                Links are for a folder or a file. Grant a person or a group above instead.
              </Text>
            ) : linksError ? (
              <ErrorState message={linksError} onRetry={load} />
            ) : links.length === 0 ? (
              <Text style={styles.empty}>No links yet.</Text>
            ) : (
              links.map((l) => (
                <View key={l.id} style={styles.row}>
                  <View style={{ flex: 1 }}>
                    <Text style={styles.rowTitle}>
                      {l.capability} link{l.has_password ? ' · password' : ''}
                    </Text>
                    <Text style={styles.rowMeta}>
                      {l.use_count} use(s)
                      {l.max_uses ? ` of ${l.max_uses}` : ''}
                      {l.expires_at ? ` · expires ${new Date(l.expires_at).toLocaleDateString()}` : ''}
                    </Text>
                  </View>
                  <Pressable onPress={() => revokeLink(l)} hitSlop={8} accessibilityRole="button">
                    <Text style={styles.danger}>Revoke</Text>
                  </Pressable>
                </View>
              ))
            )}
            {linkable && (
              <Pressable style={styles.button} onPress={createLink} accessibilityRole="button">
                <Text style={styles.buttonText}>Create a read-only link</Text>
              </Pressable>
            )}
          </Section>
        </ContentColumn>
      </ScrollView>
    </SafeAreaView>
  )
}

/** nameFor renders a subject. A link and the workspace have no person behind
 * them, so they say what they are rather than showing a uuid nobody can act on. */
function nameFor(users: Record<string, { full_name?: string; username?: string }>, share: Share): string {
  if (share.subject_type === 'link') return 'Anyone with the link'
  if (share.subject_type === 'workspace') return 'Everyone in this workspace'
  const u = users[share.subject_id]
  return u?.full_name || u?.username || share.subject_id
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingVertical: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
  },
  rowTitle: { color: theme.text, fontSize: 14 },
  rowMeta: { color: theme.textFaint, fontSize: 12, marginTop: 2 },
  danger: { color: theme.danger, fontSize: 14, fontWeight: '600' },
  empty: { color: theme.textFaint, fontSize: 13, paddingVertical: space.sm },

  button: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    backgroundColor: theme.surfaceAlt,
    marginTop: space.sm,
  },
  buttonText: { color: theme.body, fontWeight: '600', fontSize: 14 },

  tokenBox: {
    backgroundColor: theme.surface,
    borderWidth: 1,
    borderColor: theme.warning,
    borderRadius: 10,
    padding: space.md,
    marginVertical: space.md,
    gap: space.sm,
  },
  tokenTitle: { color: theme.warning, fontWeight: '700', fontSize: 14 },
  tokenBody: { color: theme.text, fontFamily: 'monospace', fontSize: 12 },
  tokenNote: { color: theme.textMuted, fontSize: 12 },
  tokenButton: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  tokenButtonText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
})
