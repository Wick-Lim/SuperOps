import React, { useCallback, useEffect, useState } from 'react'
import {
  SafeAreaView,
  View,
  Text,
  Pressable,
  FlatList,
  Alert,
  RefreshControl,
  StyleSheet,
} from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage, hasErrorCode } from '../api/client'
import { driveApi, formatBytes } from '../api/drive'
import type { DriveEntry, DriveFolder, DriveUsage, RegistryKind } from '../api/drive'
import { useDriveStore, creatableKinds } from '../stores/driveStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useResponsive, MIN_TOUCH, space } from '../lib/responsive'
import {
  ContentColumn,
  EmptyState,
  ErrorState,
  ListFooter,
  LoadingState,
  ScreenHeader,
} from './internal/ui'

const PAGE_SIZE = 40

/**
 * The Drive browser.
 *
 * It DISPATCHES ON `file_type`, never on a content type: the server publishes
 * what exists at `GET /drive/registry`, and this screen renders the "New" menu
 * from that list. A deployment without the docs editor simply does not offer
 * "New Document" — nothing here has to be taught about it.
 *
 * Folders and files arrive in ONE paginated stream, because that is how a file
 * browser reads and because two lists against one scrollbar is how "the last
 * folder is on page 3" happens.
 */
export default function DriveScreen({ navigation }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const {
    registry,
    registryLoaded,
    rootId,
    folder,
    breadcrumb,
    entries,
    cursor,
    hasMore,
    setRegistry,
    setRoot,
    setFolder,
    setEntries,
    appendEntries,
    removeEntry,
  } = useDriveStore()

  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [usage, setUsage] = useState<DriveUsage | null>(null)
  const { tier } = useResponsive()

  const workspaceId = activeWorkspace?.id

  const loadFolder = useCallback(
    async (folderId: string) => {
      setError(null)
      try {
        const [meta, page] = await Promise.all([
          driveApi.folder(folderId),
          driveApi.children(folderId, undefined, PAGE_SIZE),
        ])
        setFolder(meta.data.folder, meta.data.breadcrumb)
        setEntries(page.data, page.meta?.cursor ?? null, page.meta?.has_more ?? false)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
    [setFolder, setEntries],
  )

  // The registry is a property of the deployment, so it is fetched once. The
  // root is fetched per workspace: `GET /drive/root` creates it on demand, which
  // is what makes Drive work for a workspace that predates it.
  useEffect(() => {
    if (!workspaceId) return
    let cancelled = false
    ;(async () => {
      setLoading(true)
      try {
        if (!registryLoaded) {
          const kinds = await driveApi.registry()
          if (!cancelled) setRegistry(kinds.data)
        }
        const root = await driveApi.root(workspaceId)
        if (cancelled) return
        setRoot(root.data.id)
        await loadFolder(root.data.id)
      } catch (e) {
        if (!cancelled) setError(errorMessage(e))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [workspaceId, registryLoaded, setRegistry, setRoot, loadFolder])

  const loadUsage = useCallback(async () => {
    if (!workspaceId) return
    try {
      const res = await driveApi.usage(workspaceId)
      setUsage(res.data)
    } catch {
      // Usage is decoration. A failure here must not take the browser with it.
    }
  }, [workspaceId])

  useEffect(() => {
    void loadUsage()
  }, [loadUsage])

  const loadMore = useCallback(async () => {
    if (!folder || !hasMore || loadingMore || !cursor) return
    setLoadingMore(true)
    try {
      const page = await driveApi.children(folder.id, cursor, PAGE_SIZE)
      appendEntries(page.data, page.meta?.cursor ?? null, page.meta?.has_more ?? false)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoadingMore(false)
    }
  }, [folder, hasMore, loadingMore, cursor, appendEntries])

  const refresh = useCallback(async () => {
    if (!folder) return
    setRefreshing(true)
    await loadFolder(folder.id)
    await loadUsage()
    setRefreshing(false)
  }, [folder, loadFolder, loadUsage])

  const openEntry = useCallback(
    (entry: DriveEntry) => {
      if (entry.kind === 'folder' && entry.folder) {
        void loadFolder(entry.folder.id)
        return
      }
      if (entry.file) {
        // The descriptor decides the surface, and it is fetched by the detail
        // screen rather than here: a listing must not pay for a presign per row.
        navigation.navigate('DriveFile', { fileId: entry.file.id })
      }
    },
    [loadFolder, navigation],
  )

  const newFolder = useCallback(async () => {
    if (!workspaceId || !folder) return
    // A prompt rather than a modal: creating a folder is one field, and a screen
    // for it is a screen the user has to dismiss.
    promptForName('New folder', 'Untitled folder', async (name) => {
      try {
        await driveApi.createFolder(workspaceId, folder.id, name)
        await loadFolder(folder.id)
      } catch (e) {
        Alert.alert('Could not create the folder', errorMessage(e))
      }
    })
  }, [workspaceId, folder, loadFolder])

  const newOfKind = useCallback(
    (kind: RegistryKind) => {
      if (!workspaceId || !folder) return
      promptForName(`New ${kind.display_name.toLowerCase()}`, `Untitled ${kind.display_name}`, async (name) => {
        try {
          const created = await driveApi.createFile(workspaceId, folder.id, name, kind.type)
          await loadFolder(folder.id)
          navigation.navigate('DriveFile', { fileId: created.data.id })
        } catch (e) {
          Alert.alert(`Could not create the ${kind.display_name.toLowerCase()}`, errorMessage(e))
        }
      })
    },
    [workspaceId, folder, loadFolder, navigation],
  )

  const trashEntry = useCallback(
    (entry: DriveEntry) => {
      const id = entry.kind === 'folder' ? entry.folder?.id : entry.file?.id
      const name = entry.kind === 'folder' ? entry.folder?.name : entry.file?.name
      if (!id) return

      Alert.alert(
        `Move "${name}" to the trash?`,
        entry.kind === 'folder'
          ? 'Everything inside goes with it. You can restore it from the trash.'
          : 'You can restore it from the trash.',
        [
          { text: 'Cancel', style: 'cancel' },
          {
            text: 'Move to trash',
            style: 'destructive',
            onPress: async () => {
              try {
                if (entry.kind === 'folder') await driveApi.trashFolder(id)
                else await driveApi.trashFile(id)
                // Optimistic: the row is gone from this listing immediately, and
                // the usage figure is refetched because trashed files still
                // count towards the quota.
                removeEntry(entry.kind, id)
                void loadUsage()
              } catch (e) {
                Alert.alert('Could not move it to the trash', errorMessage(e))
                if (folder) void loadFolder(folder.id)
              }
            },
          },
        ],
      )
    },
    [removeEntry, loadUsage, folder, loadFolder],
  )

  if (!workspaceId) {
    return (
      <SafeAreaView style={styles.screen}>
        <EmptyState title="No workspace" body="Choose a workspace to open its Drive." />
      </SafeAreaView>
    )
  }

  const creatable = creatableKinds(registry)

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title="Drive"
        subtitle={usage ? usageLabel(usage) : undefined}
        onBack={() => navigation.goBack()}
      />

      <ContentColumn>
        <Breadcrumb
          trail={breadcrumb}
          onNavigate={(f) => void loadFolder(f.id)}
          compact={tier === 'compact'}
        />

        <View style={styles.actions}>
          <ActionButton label="New folder" onPress={newFolder} />
          {creatable.map((kind) => (
            <ActionButton
              key={kind.type}
              label={`New ${kind.display_name.toLowerCase()}`}
              onPress={() => newOfKind(kind)}
            />
          ))}
          <ActionButton
            label="Trash"
            onPress={() => navigation.navigate('DriveTrash')}
            tone="muted"
          />
        </View>
      </ContentColumn>

      {loading ? (
        <LoadingState label="Opening Drive" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => folder && void loadFolder(folder.id)} />
      ) : (
        <FlatList
          data={entries}
          keyExtractor={(e) => `${e.kind}:${e.folder?.id ?? e.file?.id}`}
          renderItem={({ item }) => (
            <EntryRow entry={item} onOpen={() => openEntry(item)} onTrash={() => trashEntry(item)} />
          )}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={refresh} tintColor={theme.textMuted} />}
          onEndReached={loadMore}
          onEndReachedThreshold={0.4}
          ListEmptyComponent={
            <EmptyState
              title="This folder is empty"
              body={
                rootId && folder?.id === rootId
                  ? 'Upload a file or create a document to get started.'
                  : 'Nothing here yet.'
              }
            />
          }
          ListFooterComponent={<ListFooter loading={loadingMore} hasMore={hasMore} onLoadMore={loadMore} />}
        />
      )}
    </SafeAreaView>
  )
}

/** usageLabel says what is used and, when there is a cap, what is left. The
 * server tells us whether trashed files and old versions count; hardcoding that
 * here would be wrong the day the policy changes. */
function usageLabel(usage: DriveUsage): string {
  const used = formatBytes(usage.blob.bytes)
  if (usage.quota_bytes === 0) return `${used} used`
  return `${used} of ${formatBytes(usage.quota_bytes)} used`
}

function Breadcrumb({
  trail,
  onNavigate,
  compact,
}: {
  trail: DriveFolder[]
  onNavigate: (f: DriveFolder) => void
  compact: boolean
}) {
  if (trail.length === 0) return null
  // On a narrow screen only the last two segments fit; the leading ellipsis is
  // still tappable so "up" always works.
  const shown = compact && trail.length > 2 ? trail.slice(-2) : trail
  const elided = shown.length < trail.length

  return (
    <View style={styles.breadcrumb} accessibilityRole="header">
      {elided && (
        <Pressable onPress={() => onNavigate(trail[0])} hitSlop={8}>
          <Text style={styles.crumbMuted}>…/</Text>
        </Pressable>
      )}
      {shown.map((f, i) => {
        const last = i === shown.length - 1
        return (
          <View key={f.id} style={styles.crumbWrap}>
            <Pressable onPress={() => !last && onNavigate(f)} disabled={last} hitSlop={8}>
              <Text style={last ? styles.crumbCurrent : styles.crumbMuted} numberOfLines={1}>
                {f.is_root ? 'Drive' : f.name}
              </Text>
            </Pressable>
            {!last && <Text style={styles.crumbSep}>/</Text>}
          </View>
        )
      })}
    </View>
  )
}

function EntryRow({
  entry,
  onOpen,
  onTrash,
}: {
  entry: DriveEntry
  onOpen: () => void
  onTrash: () => void
}) {
  const isFolder = entry.kind === 'folder'
  const name = isFolder ? entry.folder?.name : entry.file?.name
  const file = entry.file

  return (
    <Pressable
      onPress={onOpen}
      onLongPress={onTrash}
      style={({ pressed }) => [styles.row, pressed && styles.rowPressed]}
      accessibilityRole="button"
      accessibilityLabel={`${isFolder ? 'Folder' : 'File'} ${name}`}
      accessibilityHint={isFolder ? 'Opens the folder' : 'Opens the file'}
    >
      <Text style={styles.rowIcon}>{isFolder ? '📁' : iconFor(file?.file_type)}</Text>
      <View style={styles.rowBody}>
        <Text style={styles.rowName} numberOfLines={1}>
          {name}
        </Text>
        {file && (
          <Text style={styles.rowMeta}>
            {/* A collab-backed object has no meaningful byte count — its state is
                the CRDT log — so it says what it is instead of "0 B". */}
            {file.size_bytes > 0 ? formatBytes(file.size_bytes) : file.file_type}
            {file.current_version > 1 ? ` · v${file.current_version}` : ''}
          </Text>
        )}
      </View>
    </Pressable>
  )
}

/** iconFor is a glyph per registered type. Unknown types get a generic one
 * rather than nothing: an editor this build does not know about is still a file
 * somebody can see. */
function iconFor(fileType?: string): string {
  switch (fileType) {
    case 'document':
      return '📄'
    case 'spreadsheet':
      return '📊'
    case 'design':
      return '🎨'
    default:
      return '📎'
  }
}

function ActionButton({
  label,
  onPress,
  tone = 'default',
}: {
  label: string
  onPress: () => void
  tone?: 'default' | 'muted'
}) {
  return (
    <Pressable
      onPress={onPress}
      style={({ pressed }) => [
        styles.action,
        tone === 'muted' && styles.actionMuted,
        pressed && styles.actionPressed,
      ]}
      accessibilityRole="button"
    >
      <Text style={tone === 'muted' ? styles.actionMutedText : styles.actionText}>{label}</Text>
    </Pressable>
  )
}

/** promptForName wraps Alert.prompt, which does not exist on Android or web.
 * The fallback creates with the suggested name rather than refusing: a "New
 * folder" that does nothing on Android is worse than one called "Untitled". */
function promptForName(title: string, suggestion: string, onSubmit: (name: string) => void) {
  const alert = Alert as unknown as {
    prompt?: (
      title: string,
      message?: string,
      callback?: (text: string) => void,
      type?: string,
      defaultValue?: string,
    ) => void
  }
  if (typeof alert.prompt === 'function') {
    alert.prompt(title, undefined, (text) => onSubmit(text?.trim() || suggestion), 'plain-text', suggestion)
    return
  }
  onSubmit(suggestion)
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },

  breadcrumb: { flexDirection: 'row', alignItems: 'center', flexWrap: 'wrap', paddingVertical: space.sm },
  crumbWrap: { flexDirection: 'row', alignItems: 'center' },
  crumbMuted: { color: theme.textMuted, fontSize: 14 },
  crumbCurrent: { color: theme.text, fontSize: 14, fontWeight: '600' },
  crumbSep: { color: theme.textDim, fontSize: 14, paddingHorizontal: 6 },

  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: space.sm, paddingBottom: space.md },
  action: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    paddingHorizontal: space.md,
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  actionMuted: { backgroundColor: theme.surfaceAlt },
  actionPressed: { opacity: 0.7 },
  actionText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
  actionMutedText: { color: theme.body, fontWeight: '600', fontSize: 14 },

  row: {
    flexDirection: 'row',
    alignItems: 'center',
    minHeight: MIN_TOUCH + 8,
    paddingHorizontal: space.md,
    gap: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
  },
  rowPressed: { backgroundColor: theme.surface },
  rowIcon: { fontSize: 20 },
  rowBody: { flex: 1 },
  rowName: { color: theme.text, fontSize: 15 },
  rowMeta: { color: theme.textFaint, fontSize: 12, marginTop: 2 },
})
