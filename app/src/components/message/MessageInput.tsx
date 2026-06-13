import React, { useRef, useState } from 'react'
import { View, TextInput, Pressable, Text, Alert, ActivityIndicator } from 'react-native'
import * as DocumentPicker from 'expo-document-picker'
import { theme } from '../../lib/theme'
import { fileApi } from '../../api/files'
import { wsManager } from '../../lib/websocket'
import { useWorkspaceStore } from '../../stores/workspaceStore'

interface PendingFile {
  id: string
  name: string
}

interface Props {
  onSend: (content: string, fileIds?: string[]) => void
  channelName: string
  channelId: string
}

export default function MessageInput({ onSend, channelName, channelId }: Props) {
  const [content, setContent] = useState('')
  const [pending, setPending] = useState<PendingFile[]>([])
  const [uploading, setUploading] = useState(false)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const lastTypingRef = useRef(0)

  const handleChange = (text: string) => {
    setContent(text)
    // Throttle typing notifications to ~once per 2s.
    const now = Date.now()
    if (now - lastTypingRef.current > 2000) {
      lastTypingRef.current = now
      wsManager.sendTyping(channelId)
    }
  }

  const handleSend = () => {
    const trimmed = content.trim()
    const fileIds = pending.map((p) => p.id)
    if (!trimmed && fileIds.length === 0) return
    onSend(trimmed, fileIds.length ? fileIds : undefined)
    setContent('')
    setPending([])
  }

  const handleAttach = async () => {
    if (!activeWorkspace) {
      Alert.alert('Error', 'No active workspace')
      return
    }
    try {
      const result = await DocumentPicker.getDocumentAsync({})
      if (result.canceled || !result.assets || result.assets.length === 0) return
      const asset = result.assets[0]
      setUploading(true)
      const res = await fileApi.upload(activeWorkspace.id, {
        uri: asset.uri,
        name: asset.name,
        mimeType: asset.mimeType,
      })
      setPending((prev) => [...prev, { id: res.data.id, name: res.data.name || asset.name }])
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Upload failed')
    } finally {
      setUploading(false)
    }
  }

  const removePending = (id: string) => {
    setPending((prev) => prev.filter((p) => p.id !== id))
    fileApi.remove(id).catch(() => {})
  }

  const canSend = !!content.trim() || pending.length > 0

  return (
    <View style={{ borderTopWidth: 1, borderTopColor: theme.border, backgroundColor: theme.bg }}>
      {pending.length > 0 && (
        <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6, paddingHorizontal: 12, paddingTop: 8 }}>
          {pending.map((p) => (
            <View
              key={p.id}
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                gap: 6,
                paddingLeft: 10,
                paddingRight: 6,
                paddingVertical: 6,
                backgroundColor: theme.surface,
                borderWidth: 1,
                borderColor: theme.border,
                borderRadius: 10,
                maxWidth: 200,
              }}
            >
              <Text style={{ fontSize: 13 }}>📎</Text>
              <Text style={{ color: theme.body, fontSize: 12 }} numberOfLines={1}>
                {p.name}
              </Text>
              <Pressable onPress={() => removePending(p.id)} hitSlop={8} style={{ paddingHorizontal: 4 }}>
                <Text style={{ color: theme.textFaint, fontSize: 14 }}>✕</Text>
              </Pressable>
            </View>
          ))}
        </View>
      )}

      <View style={{ flexDirection: 'row', alignItems: 'flex-end', paddingHorizontal: 12, paddingVertical: 8, gap: 8 }}>
        <Pressable
          onPress={handleAttach}
          disabled={uploading}
          style={{
            width: 40,
            height: 40,
            borderRadius: 12,
            alignItems: 'center',
            justifyContent: 'center',
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
          }}
        >
          {uploading ? <ActivityIndicator size="small" color={theme.textMuted} /> : <Text style={{ fontSize: 18 }}>📎</Text>}
        </Pressable>

        <TextInput
          value={content}
          onChangeText={handleChange}
          placeholder={`Message #${channelName}`}
          placeholderTextColor={theme.textDim}
          multiline
          onSubmitEditing={handleSend}
          blurOnSubmit={false}
          style={{
            flex: 1,
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
            borderRadius: 12,
            paddingHorizontal: 14,
            paddingVertical: 10,
            color: theme.text,
            fontSize: 15,
            maxHeight: 120,
          }}
        />

        <Pressable
          onPress={handleSend}
          disabled={!canSend}
          style={{
            backgroundColor: canSend ? theme.primary : theme.surfaceAlt,
            borderRadius: 12,
            paddingHorizontal: 16,
            paddingVertical: 10,
            alignSelf: 'stretch',
            justifyContent: 'center',
          }}
        >
          <Text style={{ color: '#fff', fontWeight: '600', fontSize: 14 }}>Send</Text>
        </Pressable>
      </View>
    </View>
  )
}
