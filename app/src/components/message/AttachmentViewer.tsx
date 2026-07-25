import React, { useState } from 'react'
import { ActivityIndicator, Alert, Image, Linking, Modal, Pressable, Text, View } from 'react-native'
import type { FileRef } from '../../lib/types'
import { fileURL } from '../../api/client'
import { theme } from '../../lib/theme'
import { MIN_TOUCH, useModalFocus } from '../a11y'

interface Props {
  file: FileRef | null
  onClose: () => void
}

/**
 * Full-screen image viewer.
 *
 * Attachments previously rendered as a fixed 220×160 crop with no way to see
 * the rest of the image and no way to save it. "Download" hands the URL to the
 * OS: `GET /files/{id}` accepts `?token=` and answers
 * `Content-Disposition: attachment` for anything that is not an image, PDF or
 * plain text.
 */
export default function AttachmentViewer({ file, onClose }: Props) {
  const [loading, setLoading] = useState(true)
  const [failed, setFailed] = useState(false)
  const heading = useModalFocus(!!file)

  const download = () => {
    if (!file) return
    Linking.openURL(fileURL(file.id)).catch(() =>
      Alert.alert('Error', 'Could not open that attachment.'),
    )
  }

  return (
    <Modal
      visible={!!file}
      transparent
      animationType="fade"
      onRequestClose={onClose}
      onShow={() => {
        setLoading(true)
        setFailed(false)
      }}
    >
      <View accessibilityViewIsModal style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.92)' }}>
        <View
          style={{
            height: 56,
            flexDirection: 'row',
            alignItems: 'center',
            paddingHorizontal: 8,
            gap: 8,
          }}
        >
          <Pressable
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Close image"
            style={{
              width: MIN_TOUCH,
              height: MIN_TOUCH,
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.text, fontSize: 20 }}>✕</Text>
          </Pressable>
          <View {...heading} accessible accessibilityRole="header" style={{ flex: 1 }}>
            <Text style={{ color: theme.text, fontSize: 14 }} numberOfLines={1}>
              {file?.name || 'Attachment'}
            </Text>
          </View>
          <Pressable
            onPress={download}
            accessibilityRole="button"
            accessibilityLabel="Download attachment"
            style={{
              minHeight: MIN_TOUCH,
              paddingHorizontal: 14,
              alignItems: 'center',
              justifyContent: 'center',
              borderRadius: 12,
              backgroundColor: theme.surface,
            }}
          >
            <Text style={{ color: theme.accent, fontWeight: '600' }}>Download</Text>
          </Pressable>
        </View>

        <Pressable
          onPress={onClose}
          accessibilityLabel="Close image"
          accessibilityRole="button"
          style={{ flex: 1, alignItems: 'center', justifyContent: 'center', padding: 12 }}
        >
          {file && !failed && (
            <Image
              source={{ uri: fileURL(file.id) }}
              onLoad={() => setLoading(false)}
              onError={() => {
                setLoading(false)
                setFailed(true)
              }}
              style={{ width: '100%', height: '100%' }}
              resizeMode="contain"
              accessibilityLabel={file.name || 'Attachment'}
            />
          )}
          {loading && !failed && (
            <View
              style={{
                position: 'absolute',
                top: 0,
                left: 0,
                right: 0,
                bottom: 0,
                alignItems: 'center',
                justifyContent: 'center',
              }}
            >
              <ActivityIndicator size="large" color={theme.textMuted} />
            </View>
          )}
          {failed && (
            <Text style={{ color: theme.textMuted, fontSize: 15, textAlign: 'center' }}>
              This image could not be loaded. Try downloading it instead.
            </Text>
          )}
        </Pressable>
      </View>
    </Modal>
  )
}
