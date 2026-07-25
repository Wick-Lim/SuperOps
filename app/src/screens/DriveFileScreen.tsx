import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Image, Pressable, ScrollView, Alert, Linking, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { driveApi, formatBytes } from '../api/drive'
import type { DriveDescriptor, DriveVersion, RegistryKind } from '../api/drive'
import { useDriveStore } from '../stores/driveStore'
import { space, MIN_TOUCH } from '../lib/responsive'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

/**
 * One Drive object.
 *
 * THE DISPATCH IS THE POINT. The descriptor carries `file_type` and
 * `storage_mode`, and this screen picks a surface from the first and decides
 * what it can offer from the second. It never looks at `content_type`.
 *
 * `storage_mode: 'collab'` means the bytes in storage are NOT the document —
 * its state is a CRDT log the server cannot interpret — so there is no download
 * and no "upload a new version". Offering either would be a control that
 * silently discards what the user gave it.
 */
export default function DriveFileScreen({ navigation, route }: { navigation: any; route: any }) {
  const fileId: string = route.params?.fileId
  const registry = useDriveStore((s) => s.registry)

  const [descriptor, setDescriptor] = useState<DriveDescriptor | null>(null)
  const [versions, setVersions] = useState<DriveVersion[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      const res = await driveApi.open(fileId)
      setDescriptor(res.data)
      // Versions are fetched alongside because the panel is on this screen; a
      // collab type answers with an empty list rather than an error, which is
      // the honest answer for a thing that genuinely has none.
      const v = await driveApi.versions(fileId)
      setVersions(v.data)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
    }
  }, [fileId])

  useEffect(() => {
    void load()
  }, [load])

  const kind: RegistryKind | undefined = descriptor
    ? registry.find((k) => k.type === descriptor.file_type)
    : undefined

  const download = useCallback(() => {
    if (!descriptor?.content_url) return
    // The URL is presigned and short-lived, and it carries
    // `Content-Disposition: attachment` — the server forces that because a
    // presigned URL cannot carry a CSP header, so the browser must never render
    // the bytes on the bucket's origin.
    void Linking.openURL(descriptor.content_url)
  }, [descriptor])

  const restore = useCallback(
    (version: number) => {
      Alert.alert(
        `Restore version ${version}?`,
        'It becomes the current version. Nothing is deleted — the newer versions stay in the history.',
        [
          { text: 'Cancel', style: 'cancel' },
          {
            text: 'Restore',
            onPress: async () => {
              try {
                await driveApi.restoreVersion(fileId, version)
                await load()
              } catch (e) {
                Alert.alert('Could not restore that version', errorMessage(e))
              }
            },
          },
        ],
      )
    },
    [fileId, load],
  )

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Opening" />
      </SafeAreaView>
    )
  }
  if (error || !descriptor) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title="File" onBack={() => navigation.goBack()} />
        <ErrorState message={error ?? 'Not found'} onRetry={load} />
      </SafeAreaView>
    )
  }

  const canWrite = ['write', 'share', 'admin'].includes(descriptor.capability)
  const isCollab = descriptor.storage_mode === 'collab'

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={descriptor.name}
        subtitle={kind?.display_name ?? descriptor.file_type}
        onBack={() => navigation.goBack()}
      />
      <ScrollView>
        <ContentColumn>
          {/* The thumbnail is the ONE thing served inline from a presigned URL.
              It is safe because the SERVER generated it and its type comes from
              a closed set — see the server's thumbMediaTypes. */}
          {descriptor.thumbnail_url && (
            <Image
              source={{ uri: descriptor.thumbnail_url }}
              style={styles.preview}
              resizeMode="contain"
              accessibilityLabel={`Preview of ${descriptor.name}`}
            />
          )}

          {/* A read-only surface is RENDERED as read-only rather than failing on
              submit. The capability is in the descriptor precisely so this needs
              no extra request. */}
          {!canWrite && (
            <View style={styles.notice}>
              <Text style={styles.noticeText}>
                You have read access to this file.
              </Text>
            </View>
          )}

          <Section title="Details">
            <Detail label="Type" value={kind?.display_name ?? descriptor.file_type} />
            <Detail
              label="Size"
              value={
                isCollab
                  ? 'Stored as collaborative edits'
                  : formatBytes(descriptor.size_bytes)
              }
            />
            <Detail label="Version" value={`${descriptor.current_version}`} />
            <Detail label="Updated" value={new Date(descriptor.updated_at).toLocaleString()} />
          </Section>

          <View style={styles.actions}>
            {isCollab ? (
              <Action
                label={canWrite ? 'Open editor' : 'Open'}
                onPress={() =>
                  navigation.navigate('CollabDocument', {
                    documentId: descriptor.collab_document_id,
                    fileId: descriptor.id,
                    name: descriptor.name,
                  })
                }
                disabled={!descriptor.collab_document_id}
              />
            ) : (
              <Action label="Download" onPress={download} disabled={!descriptor.content_url} />
            )}
            <Action
              label="Share"
              tone="muted"
              onPress={() =>
                navigation.navigate('DriveShare', {
                  objectType: 'file',
                  objectId: descriptor.id,
                  name: descriptor.name,
                })
              }
              disabled={!['share', 'admin'].includes(descriptor.capability)}
            />
          </View>

          {/* The versions panel is absent for a collab type rather than empty.
              Its history is the edit log, and a panel that always said "no
              versions" would read as a feature that is broken rather than one
              that does not apply. */}
          {!isCollab && (
            <Section title="Version history">
              {versions.length === 0 ? (
                <Text style={styles.empty}>No history yet.</Text>
              ) : (
                versions.map((v) => (
                  <View key={v.version} style={styles.versionRow}>
                    <View style={{ flex: 1 }}>
                      <Text style={styles.versionTitle}>
                        Version {v.version}
                        {v.is_current ? ' · current' : ''}
                      </Text>
                      <Text style={styles.versionMeta}>
                        {formatBytes(v.size_bytes)} · {new Date(v.created_at).toLocaleString()}
                      </Text>
                    </View>
                    {!v.is_current && canWrite && (
                      <Pressable
                        onPress={() => restore(v.version)}
                        hitSlop={8}
                        accessibilityRole="button"
                      >
                        <Text style={styles.link}>Restore</Text>
                      </Pressable>
                    )}
                  </View>
                ))
              )}
            </Section>
          )}
        </ContentColumn>
      </ScrollView>
    </SafeAreaView>
  )
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.detail}>
      <Text style={styles.detailLabel}>{label}</Text>
      <Text style={styles.detailValue}>{value}</Text>
    </View>
  )
}

function Action({
  label,
  onPress,
  tone = 'default',
  disabled,
}: {
  label: string
  onPress: () => void
  tone?: 'default' | 'muted'
  disabled?: boolean
}) {
  return (
    <Pressable
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => [
        styles.action,
        tone === 'muted' && styles.actionMuted,
        disabled && styles.actionDisabled,
        pressed && !disabled && styles.actionPressed,
      ]}
      accessibilityRole="button"
      accessibilityState={{ disabled: !!disabled }}
    >
      <Text style={tone === 'muted' ? styles.actionMutedText : styles.actionText}>{label}</Text>
    </Pressable>
  )
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  preview: {
    width: '100%',
    height: 220,
    borderRadius: 10,
    backgroundColor: theme.surface,
    marginVertical: space.md,
  },
  notice: {
    backgroundColor: theme.surfaceAlt,
    borderRadius: 8,
    padding: space.sm,
    marginBottom: space.md,
  },
  noticeText: { color: theme.textMuted, fontSize: 13 },

  detail: { flexDirection: 'row', justifyContent: 'space-between', paddingVertical: 6 },
  detailLabel: { color: theme.textMuted, fontSize: 13 },
  detailValue: { color: theme.body, fontSize: 13 },

  actions: { flexDirection: 'row', gap: space.sm, paddingVertical: space.md, flexWrap: 'wrap' },
  action: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    paddingHorizontal: space.md,
    borderRadius: 8,
    backgroundColor: theme.primary,
  },
  actionMuted: { backgroundColor: theme.surfaceAlt },
  actionDisabled: { opacity: 0.4 },
  actionPressed: { opacity: 0.7 },
  actionText: { color: theme.primaryText, fontWeight: '600', fontSize: 14 },
  actionMutedText: { color: theme.body, fontWeight: '600', fontSize: 14 },

  versionRow: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingVertical: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
  },
  versionTitle: { color: theme.text, fontSize: 14 },
  versionMeta: { color: theme.textFaint, fontSize: 12, marginTop: 2 },
  link: { color: theme.accent, fontSize: 14, fontWeight: '600' },
  empty: { color: theme.textFaint, fontSize: 13, paddingVertical: space.sm },
})
