import React, { useEffect, useState } from 'react'
import {
  Modal,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native'
import { theme } from '../lib/theme'
import { MIN_TOUCH, space } from '../lib/responsive'

export interface PromptModalProps {
  visible: boolean
  title: string
  defaultValue?: string
  placeholder?: string
  submitLabel?: string
  onCancel: () => void
  onSubmit: (value: string) => void
}

/**
 * Cross-platform single-field prompt dialog.
 *
 * Replaces iOS-only `Alert.prompt` so Android and Web users can name folders,
 * documents, projects, and issues instead of silently creating "Untitled".
 */
export default function PromptModal({
  visible,
  title,
  defaultValue = '',
  placeholder,
  submitLabel = 'Create',
  onCancel,
  onSubmit,
}: PromptModalProps) {
  const [value, setValue] = useState(defaultValue)

  useEffect(() => {
    if (visible) {
      setValue(defaultValue)
    }
  }, [visible, defaultValue])

  const submit = () => {
    const trimmed = value.trim() || defaultValue.trim()
    if (!trimmed) return
    onSubmit(trimmed)
  }

  return (
    <Modal visible={visible} transparent animationType="fade" onRequestClose={onCancel}>
      <Pressable style={styles.backdrop} onPress={onCancel}>
        <Pressable style={styles.card} onPress={() => {}}>
          <Text style={styles.title}>{title}</Text>
          <TextInput
            value={value}
            onChangeText={setValue}
            placeholder={placeholder ?? defaultValue}
            placeholderTextColor={theme.textFaint}
            autoFocus
            onSubmitEditing={submit}
            style={styles.input}
            accessibilityLabel={title}
          />
          <View style={styles.actions}>
            <Pressable
              onPress={onCancel}
              style={[styles.button, styles.cancelButton]}
              accessibilityRole="button"
            >
              <Text style={styles.cancelText}>Cancel</Text>
            </Pressable>
            <Pressable
              onPress={submit}
              style={[styles.button, styles.submitButton]}
              accessibilityRole="button"
            >
              <Text style={styles.submitText}>{submitLabel}</Text>
            </Pressable>
          </View>
        </Pressable>
      </Pressable>
    </Modal>
  )
}

const styles = StyleSheet.create({
  backdrop: {
    flex: 1,
    backgroundColor: 'rgba(0,0,0,0.55)',
    justifyContent: 'center',
    alignItems: 'center',
    padding: space.lg,
  },
  card: {
    width: '100%',
    maxWidth: 400,
    backgroundColor: theme.surface,
    borderRadius: 12,
    padding: space.md,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: theme.border,
    gap: space.md,
  },
  title: {
    color: theme.text,
    fontSize: 16,
    fontWeight: '700',
  },
  input: {
    minHeight: MIN_TOUCH,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: theme.border,
    backgroundColor: theme.bg,
    color: theme.text,
    paddingHorizontal: space.sm,
    fontSize: 14,
  },
  actions: {
    flexDirection: 'row',
    justifyContent: 'flex-end',
    gap: space.sm,
  },
  button: {
    minHeight: MIN_TOUCH,
    paddingHorizontal: space.md,
    borderRadius: 8,
    justifyContent: 'center',
    alignItems: 'center',
  },
  cancelButton: {
    backgroundColor: theme.surfaceAlt,
  },
  submitButton: {
    backgroundColor: theme.primary,
  },
  cancelText: {
    color: theme.body,
    fontWeight: '600',
    fontSize: 14,
  },
  submitText: {
    color: theme.primaryText,
    fontWeight: '600',
    fontSize: 14,
  },
})
