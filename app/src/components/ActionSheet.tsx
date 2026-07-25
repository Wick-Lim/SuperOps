import React from 'react'
import { Modal, Pressable, ScrollView, Text, View } from 'react-native'
import { theme } from '../lib/theme'
import { MIN_TOUCH, useModalFocus } from './a11y'

export interface SheetAction {
  key: string
  label: string
  icon?: string
  destructive?: boolean
  disabled?: boolean
  onPress: () => void
}

interface Props {
  visible: boolean
  title?: string
  actions: SheetAction[]
  onClose: () => void
}

/**
 * Bottom-sheet replacement for `Alert.alert(title, undefined, buttons)`.
 *
 * Android's Alert renders at most three buttons (neutral/negative/positive) and
 * silently drops the rest, so the six-entry message menu meant Android users
 * could not Forward, Edit or Delete anything. A sheet also matches the modal
 * convention already used in this file's callers.
 */
export default function ActionSheet({ visible, title, actions, onClose }: Props) {
  const heading = useModalFocus(visible)

  return (
    <Modal visible={visible} transparent animationType="fade" onRequestClose={onClose}>
      <Pressable
        onPress={onClose}
        accessibilityRole="button"
        accessibilityLabel="Close menu"
        style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.6)', justifyContent: 'flex-end' }}
      >
        <Pressable
          onPress={(e) => e.stopPropagation()}
          accessibilityViewIsModal
          accessibilityRole="menu"
          style={{
            backgroundColor: theme.surface,
            borderTopLeftRadius: 20,
            borderTopRightRadius: 20,
            paddingHorizontal: 8,
            paddingTop: 12,
            paddingBottom: 24,
            borderTopWidth: 1,
            borderColor: theme.border,
            maxHeight: '80%',
          }}
        >
          <View style={{ alignItems: 'center', marginBottom: 8 }}>
            <View style={{ width: 40, height: 4, borderRadius: 2, backgroundColor: theme.borderStrong }} />
          </View>

          {/* A Modal traps touch but not the screen-reader cursor; without
              this the sheet opens silently behind whatever was focused. */}
          <View
            {...heading}
            accessible
            accessibilityRole="header"
            style={{ paddingHorizontal: 12, paddingBottom: 6 }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '700', letterSpacing: 1 }}>
              {(title ?? 'Actions').toUpperCase()}
            </Text>
          </View>

          <ScrollView>
            {actions.map((a) => (
              <Pressable
                key={a.key}
                disabled={a.disabled}
                onPress={() => {
                  onClose()
                  a.onPress()
                }}
                accessibilityRole="menuitem"
                accessibilityLabel={a.label}
                accessibilityState={{ disabled: !!a.disabled }}
                style={({ pressed }) => ({
                  flexDirection: 'row',
                  alignItems: 'center',
                  gap: 12,
                  minHeight: MIN_TOUCH + 4,
                  paddingHorizontal: 12,
                  borderRadius: 12,
                  opacity: a.disabled ? 0.4 : 1,
                  backgroundColor: pressed ? theme.surfaceAlt : 'transparent',
                })}
              >
                {!!a.icon && <Text style={{ fontSize: 16, width: 22 }}>{a.icon}</Text>}
                <Text
                  style={{
                    color: a.destructive ? theme.danger : theme.body,
                    fontSize: 16,
                    fontWeight: '500',
                  }}
                >
                  {a.label}
                </Text>
              </Pressable>
            ))}
          </ScrollView>

          <Pressable
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Cancel"
            style={{
              marginTop: 8,
              marginHorizontal: 4,
              minHeight: MIN_TOUCH,
              alignItems: 'center',
              justifyContent: 'center',
              borderRadius: 12,
              backgroundColor: theme.surfaceAlt,
            }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 15, fontWeight: '600' }}>Cancel</Text>
          </Pressable>
        </Pressable>
      </Pressable>
    </Modal>
  )
}
