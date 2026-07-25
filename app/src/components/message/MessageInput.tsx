import React, { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActivityIndicator,
  Alert,
  NativeSyntheticEvent,
  Platform,
  Pressable,
  Text,
  TextInput,
  TextInputKeyPressEventData,
  View,
} from 'react-native'
import * as DocumentPicker from 'expo-document-picker'
import { theme } from '../../lib/theme'
import { fileApi, type PickedFile, type UploadedFile } from '../../api/files'
import { errorMessage } from '../../api/client'
import { wsManager } from '../../lib/websocket'
import { useWorkspaceStore } from '../../stores/workspaceStore'
import { space, useResponsive } from '../../lib/responsive'
import { touchSlop } from '../a11y'

/** A file the user picked but that has not been sent to the server yet. */
interface StagedFile {
  key: string
  file: PickedFile
}

interface Props {
  /** Called with the already-uploaded files, so the caller can render them optimistically. */
  onSend: (content: string, files?: UploadedFile[]) => void
  channelId: string
  channelName?: string
  placeholder?: string
  autoFocus?: boolean
}

let stagedSeq = 0

export default function MessageInput({
  onSend,
  channelId,
  channelName,
  placeholder,
  autoFocus,
}: Props) {
  const [content, setContent] = useState('')
  const [staged, setStaged] = useState<StagedFile[]>([])
  const [sending, setSending] = useState(false)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const { tier, minTouch } = useResponsive()
  const lastTypingRef = useRef(0)
  const typingActiveRef = useRef(false)

  const stopTyping = useCallback(() => {
    if (!typingActiveRef.current) return
    typingActiveRef.current = false
    lastTypingRef.current = 0
    wsManager.sendTypingStop(channelId)
  }, [channelId])

  // Leaving a channel mid-sentence left the indicator up for everyone else
  // until the 4s TTL expired on each of their clients.
  useEffect(() => stopTyping, [stopTyping])

  const handleChange = (text: string) => {
    setContent(text)
    if (!text.trim()) {
      stopTyping()
      return
    }
    const now = Date.now()
    if (now - lastTypingRef.current > 2000) {
      lastTypingRef.current = now
      typingActiveRef.current = true
      wsManager.sendTyping(channelId)
    }
  }

  const handleAttach = async () => {
    try {
      const result = await DocumentPicker.getDocumentAsync({})
      if (result.canceled || !result.assets || result.assets.length === 0) return
      const asset = result.assets[0]
      // Deliberately NOT uploaded here. Uploading on pick orphaned the blob
      // server-side whenever the composer was abandoned.
      setStaged((prev) => [
        ...prev,
        {
          key: `staged-${++stagedSeq}`,
          file: { uri: asset.uri, name: asset.name, mimeType: asset.mimeType },
        },
      ])
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not open the file picker'))
    }
  }

  const removeStaged = (key: string) => {
    setStaged((prev) => prev.filter((s) => s.key !== key))
  }

  const handleSend = async () => {
    if (sending) return
    const trimmed = content.trim()
    if (!trimmed && staged.length === 0) return
    if (staged.length > 0 && !activeWorkspace) {
      Alert.alert('Error', 'No active workspace')
      return
    }

    setSending(true)
    const uploaded: UploadedFile[] = []
    try {
      for (const s of staged) {
        const res = await fileApi.upload(activeWorkspace!.id, s.file)
        uploaded.push(res.data)
      }
    } catch (err) {
      // Roll the partial upload back so a retry does not leak the blobs that
      // did land.
      uploaded.forEach((f) => {
        fileApi.remove(f.id).catch(() => {})
      })
      setSending(false)
      Alert.alert('Error', errorMessage(err, 'Upload failed'))
      return
    }

    setSending(false)
    stopTyping()
    setContent('')
    setStaged([])
    onSend(trimmed, uploaded.length ? uploaded : undefined)
  }

  /**
   * Enter sends, Shift+Enter inserts a newline — the expectation with a physical
   * keyboard, and the reason a chat composer feels wrong on a desktop without it.
   *
   * Gated on the tier, not just the platform: a phone-width browser window is
   * still a thumb-driven layout with an on-screen keyboard whose Enter key is a
   * newline, and `onSubmitEditing` on a multiline TextInput does not fire on
   * Android at all. Below `medium` the Send button stays the only send path,
   * exactly as before.
   */
  const enterSends = Platform.OS === 'web' && tier !== 'compact'

  const handleKeyPress = (e: NativeSyntheticEvent<TextInputKeyPressEventData>) => {
    const ne = e.nativeEvent as TextInputKeyPressEventData & { shiftKey?: boolean }
    if (ne.key !== 'Enter' || ne.shiftKey) return
    ;(e as unknown as { preventDefault?: () => void }).preventDefault?.()
    void handleSend()
  }

  const canSend = (!!content.trim() || staged.length > 0) && !sending

  return (
    <View style={{ borderTopWidth: 1, borderTopColor: theme.border, backgroundColor: theme.bg }}>
      {staged.length > 0 && (
        <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6, paddingHorizontal: 12, paddingTop: 8 }}>
          {staged.map((s) => (
            <View
              key={s.key}
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
                {s.file.name}
              </Text>
              <Pressable
                onPress={() => removeStaged(s.key)}
                hitSlop={touchSlop(20)}
                accessibilityRole="button"
                accessibilityLabel={`Remove attachment ${s.file.name}`}
                style={{ paddingHorizontal: 4 }}
              >
                <Text style={{ color: theme.textMuted, fontSize: 14 }}>✕</Text>
              </Pressable>
            </View>
          ))}
        </View>
      )}

      <View style={{ flexDirection: 'row', alignItems: 'flex-end', paddingHorizontal: 12, paddingVertical: 8, gap: 8 }}>
        <Pressable
          onPress={handleAttach}
          disabled={sending}
          accessibilityRole="button"
          accessibilityLabel="Attach a file"
          accessibilityState={{ disabled: sending }}
          style={{
            width: minTouch,
            height: minTouch,
            borderRadius: 12,
            alignItems: 'center',
            justifyContent: 'center',
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
            opacity: sending ? 0.5 : 1,
          }}
        >
          <Text style={{ fontSize: 18 }}>📎</Text>
        </Pressable>

        <TextInput
          value={content}
          onChangeText={handleChange}
          onBlur={stopTyping}
          autoFocus={autoFocus}
          placeholder={placeholder ?? `Message #${channelName || 'channel'}`}
          placeholderTextColor={theme.textMuted}
          accessibilityLabel={placeholder ?? `Message ${channelName || 'channel'}`}
          multiline
          submitBehavior="newline"
          onKeyPress={enterSends ? handleKeyPress : undefined}
          style={{
            flex: 1,
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
            borderRadius: 12,
            paddingHorizontal: 14,
            paddingVertical: 9,
            color: theme.text,
            fontSize: 15,
            minHeight: minTouch,
            maxHeight: 120,
          }}
        />

        <Pressable
          onPress={handleSend}
          disabled={!canSend}
          accessibilityRole="button"
          accessibilityLabel={sending ? 'Sending' : 'Send message'}
          accessibilityState={{ disabled: !canSend, busy: sending }}
          style={{
            backgroundColor: canSend ? theme.primary : theme.surfaceAlt,
            borderRadius: 12,
            paddingHorizontal: enterSends ? 14 : 16,
            minWidth: enterSends ? 60 : 72,
            minHeight: minTouch,
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          {sending ? (
            <ActivityIndicator size="small" color="#fff" />
          ) : (
            <Text style={{ color: '#fff', fontWeight: '600', fontSize: 14 }}>Send</Text>
          )}
        </Pressable>
      </View>

      {/* A shortcut nobody is told about does not exist. Shown while there is
          something to send, but the row is always laid out where the shortcut is
          bound: appearing on the first keystroke would shove the whole
          conversation up 19px, and sending would drop it back down again. */}
      {enterSends && (
        <View
          style={{
            paddingHorizontal: space.md,
            paddingBottom: 6,
            marginTop: -4,
            opacity: content ? 1 : 0,
          }}
        >
          <Text style={{ color: theme.textDim, fontSize: 11 }}>
            Enter to send · Shift+Enter for a new line
          </Text>
        </View>
      )}
    </View>
  )
}
