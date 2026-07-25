import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Pressable, ScrollView, Alert, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { driveApi } from '../api/drive'
import type { Share, ShareLink } from '../api/drive'
import { useUserStore } from '../stores/userStore'
import { space, MIN_TOUCH } from '../lib/responsive'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

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
  const objectType: 'folder' | 'file' = route.params?.objectType ?? 'file'
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

  const load = useCallback(async () => {
    setError(null)
    try {
      const [s, l] = await Promise.all([
        driveApi.shares(objectType, objectId),
        driveApi.links(objectType, objectId),
      ])
      setShares(s.data)
      setLinks(l.data)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
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
              <Text style={styles.tokenTitle}>Copy this link now</Text>
              <Text style={styles.tokenBody} selectable>
                {freshToken}
              </Text>
              <Text style={styles.tokenNote}>
                It is shown once and cannot be recovered. Anyone holding it can open {name}.
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
            {links.length === 0 ? (
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
            <Pressable style={styles.button} onPress={createLink} accessibilityRole="button">
              <Text style={styles.buttonText}>Create a read-only link</Text>
            </Pressable>
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
