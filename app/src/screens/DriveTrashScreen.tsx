import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Pressable, FlatList, Alert, RefreshControl, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { driveApi, formatBytes } from '../api/drive'
import type { TrashEntry } from '../api/drive'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { space, MIN_TOUCH } from '../lib/responsive'
import { ContentColumn, EmptyState, ErrorState, LoadingState, ScreenHeader } from './internal/ui'

/**
 * The trash.
 *
 * It shows the TOP of each trashed tree and nothing below it: a folder whose
 * parent is also trashed went with its ancestor, and listing it separately would
 * offer a restore that cannot mean anything on its own. `item_count` is what
 * makes the size of the decision visible before the tap.
 *
 * `purge_after` comes from the server. It is the promise made to whoever deleted
 * the thing, and computing it here from a retention setting the client cannot
 * see would be a different promise.
 */
export default function DriveTrashScreen({ navigation }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const workspaceId = activeWorkspace?.id

  const [entries, setEntries] = useState<TrashEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!workspaceId) return
    setError(null)
    try {
      const res = await driveApi.trash(workspaceId)
      setEntries(res.data)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
      setRefreshing(false)
    }
  }, [workspaceId])

  useEffect(() => {
    void load()
  }, [load])

  const restore = useCallback(
    async (entry: TrashEntry) => {
      try {
        const res = await driveApi.restore(entry.kind, entry.id)
        // The server says whether it landed somewhere else, and that is worth
        // surfacing: the user will look where they deleted it.
        if (res.data.relocated) {
          Alert.alert('Restored to the Drive root', res.data.note)
        }
        await load()
      } catch (e) {
        Alert.alert('Could not restore it', errorMessage(e))
      }
    },
    [load],
  )

  const empty = useCallback(() => {
    if (!workspaceId) return
    Alert.alert(
      'Empty the trash?',
      'Everything in the trash is deleted permanently. This cannot be undone.',
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Delete permanently',
          style: 'destructive',
          onPress: async () => {
            try {
              const res = await driveApi.emptyTrash(workspaceId)
              Alert.alert(
                'Trash emptied',
                `${res.data.files_purged} file(s) and ${res.data.folders_purged} folder(s) deleted, ` +
                  `${formatBytes(res.data.bytes_freed)} freed.`,
              )
              await load()
            } catch (e) {
              Alert.alert('Could not empty the trash', errorMessage(e))
            }
          },
        },
      ],
    )
  }, [workspaceId, load])

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader title="Trash" onBack={() => navigation.goBack()} />
      {entries.length > 0 && (
        <ContentColumn>
          <Pressable onPress={empty} style={styles.dangerButton} accessibilityRole="button">
            <Text style={styles.dangerText}>Empty the trash</Text>
          </Pressable>
        </ContentColumn>
      )}

      {loading ? (
        <LoadingState label="Loading the trash" />
      ) : error ? (
        <ErrorState message={error} onRetry={load} />
      ) : (
        <FlatList
          data={entries}
          keyExtractor={(e) => `${e.kind}:${e.id}`}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => {
                setRefreshing(true)
                void load()
              }}
              tintColor={theme.textMuted}
            />
          }
          renderItem={({ item }) => (
            <View style={styles.row}>
              <Text style={styles.icon}>{item.kind === 'folder' ? '📁' : '📎'}</Text>
              <View style={{ flex: 1 }}>
                <Text style={styles.name} numberOfLines={1}>
                  {item.name}
                </Text>
                <Text style={styles.meta}>
                  {item.kind === 'folder' && item.item_count > 0
                    ? `${item.item_count} item(s) inside · `
                    : item.size_bytes > 0
                      ? `${formatBytes(item.size_bytes)} · `
                      : ''}
                  {item.purge_after
                    ? `deleted permanently ${new Date(item.purge_after).toLocaleDateString()}`
                    : 'kept until you empty the trash'}
                </Text>
              </View>
              <Pressable onPress={() => restore(item)} hitSlop={8} accessibilityRole="button">
                <Text style={styles.link}>Restore</Text>
              </Pressable>
            </View>
          )}
          ListEmptyComponent={
            <EmptyState title="The trash is empty" body="Deleted files and folders appear here." />
          }
        />
      )}
    </SafeAreaView>
  )
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  dangerButton: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    borderWidth: 1,
    borderColor: theme.danger,
    marginVertical: space.sm,
  },
  dangerText: { color: theme.danger, fontWeight: '600', fontSize: 14 },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    minHeight: MIN_TOUCH + 8,
    paddingHorizontal: space.md,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
  },
  icon: { fontSize: 20 },
  name: { color: theme.text, fontSize: 15 },
  meta: { color: theme.textFaint, fontSize: 12, marginTop: 2 },
  link: { color: theme.accent, fontSize: 14, fontWeight: '600' },
})
