import React, { useEffect, useState } from 'react'
import { View, Text, Pressable, StyleSheet } from 'react-native'
import { theme } from '../../lib/theme'
import { space } from '../../lib/responsive'
import { driveApi } from '../../api/drive'
import type { Backlink } from '../../api/drive'

/**
 * "Where is this used".
 *
 * FILTERED BY WHAT THE READER MAY SEE, by the server, per row. A document that
 * references this one but is not shared with the reader does not appear — the
 * panel must not become a way to discover the existence of documents, which is
 * exactly what an unfiltered "N documents link here" count would be.
 *
 * It renders NOTHING when the list is empty rather than an empty section. A
 * permanent "No backlinks" heading on every document is noise on the ninety
 * percent of documents nothing links to.
 */
export default function Backlinks({
  refType,
  refId,
  navigation,
}: {
  refType: string
  refId: string
  navigation: any
}) {
  const [links, setLinks] = useState<Backlink[] | null>(null)

  useEffect(() => {
    let cancelled = false
    void driveApi
      .backlinks(refType, refId)
      .then((res) => !cancelled && setLinks(res.data ?? []))
      // A failure is an EMPTY panel, not an error banner. Backlinks are
      // context; interrupting somebody reading a document to tell them a
      // secondary list could not load is the wrong trade.
      .catch(() => !cancelled && setLinks([]))
    return () => {
      cancelled = true
    }
  }, [refType, refId])

  if (!links || links.length === 0) return null

  return (
    <View style={styles.panel}>
      <Text style={styles.heading}>Referenced in</Text>
      {links.map((l) => (
        <Pressable
          key={l.file_id}
          onPress={() =>
            navigation.navigate('DriveFile', { fileId: l.file_id })
          }
          style={({ pressed }) => [styles.row, pressed && styles.pressed]}
          accessibilityRole="button"
          accessibilityLabel={`Open ${l.name}`}
        >
          <Text style={styles.name} numberOfLines={1}>
            {l.name}
          </Text>
          <Text style={styles.meta}>{new Date(l.updated_at).toLocaleDateString()}</Text>
        </Pressable>
      ))}
    </View>
  )
}

const styles = StyleSheet.create({
  panel: {
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: theme.border,
    padding: space.md,
    backgroundColor: theme.surface,
  },
  heading: {
    color: theme.textMuted,
    fontSize: 11,
    fontWeight: '700',
    textTransform: 'uppercase',
    marginBottom: space.xs,
  },
  row: { flexDirection: 'row', alignItems: 'center', gap: space.sm, paddingVertical: 6 },
  name: { color: theme.body, fontSize: 14, flex: 1 },
  meta: { color: theme.textFaint, fontSize: 12 },
  pressed: { opacity: 0.7 },
})
