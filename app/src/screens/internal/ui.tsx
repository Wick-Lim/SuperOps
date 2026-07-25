import React from 'react'
import { View, Text, TextInput, Pressable, ActivityIndicator } from 'react-native'
import { theme } from '../../lib/theme'
import { MIN_TOUCH, touchSlop } from '../../components/a11y'

// Re-exported so screens have one import for chrome + sizing.
export { MIN_TOUCH, touchSlop }

/**
 * Shared screen chrome. The 56px header row was copy-pasted into eight screens
 * with no accessibility props at all; centralising it is what makes labelling
 * the back button (and hitting the 44x44 touch-target minimum) a one-line
 * change instead of an eight-file change.
 */

export function ScreenHeader({
  title,
  onBack,
  backLabel = 'Back',
  right,
  subtitle,
}: {
  title: string
  onBack?: () => void
  backLabel?: string
  right?: React.ReactNode
  subtitle?: string
}) {
  return (
    <View
      style={{
        minHeight: 56,
        paddingHorizontal: 12,
        paddingVertical: 6,
        flexDirection: 'row',
        alignItems: 'center',
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
      }}
    >
      {onBack ? (
        <Pressable
          onPress={onBack}
          accessibilityRole="button"
          accessibilityLabel={backLabel}
          hitSlop={touchSlop(32)}
          style={{ minWidth: MIN_TOUCH, minHeight: MIN_TOUCH, justifyContent: 'center', paddingRight: 8 }}
        >
          <Text style={{ color: theme.accent, fontSize: 16 }}>‹ {backLabel}</Text>
        </Pressable>
      ) : null}
      <View style={{ flex: 1 }}>
        <Text
          accessibilityRole="header"
          style={{ color: theme.text, fontSize: 17, fontWeight: '700' }}
          numberOfLines={1}
        >
          {title}
        </Text>
        {subtitle ? (
          <Text style={{ color: theme.textMuted, fontSize: 12 }} numberOfLines={1}>
            {subtitle}
          </Text>
        ) : null}
      </View>
      {right}
    </View>
  )
}

export function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <View style={{ marginBottom: 28 }}>
      <Text
        accessibilityRole="header"
        style={{
          color: theme.textMuted,
          fontSize: 11,
          fontWeight: '700',
          letterSpacing: 1,
          marginBottom: 10,
          paddingHorizontal: 4,
        }}
      >
        {title.toUpperCase()}
      </Text>
      <View
        style={{
          backgroundColor: theme.surface,
          borderWidth: 1,
          borderColor: theme.border,
          borderRadius: 12,
          padding: 16,
        }}
      >
        {children}
      </View>
    </View>
  )
}

export function Field({
  label,
  value,
  onChangeText,
  placeholder,
  secureTextEntry,
  autoCapitalize,
  keyboardType,
  multiline,
  hint,
  autoFocus,
  maxLength,
}: {
  label: string
  value: string
  onChangeText: (t: string) => void
  placeholder?: string
  secureTextEntry?: boolean
  autoCapitalize?: 'none' | 'sentences' | 'words' | 'characters'
  keyboardType?: 'default' | 'numeric' | 'email-address' | 'number-pad'
  multiline?: boolean
  hint?: string
  autoFocus?: boolean
  maxLength?: number
}) {
  return (
    <View style={{ marginBottom: 12 }}>
      <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 6 }}>{label}</Text>
      <TextInput
        value={value}
        onChangeText={onChangeText}
        placeholder={placeholder}
        placeholderTextColor={theme.textMuted}
        secureTextEntry={secureTextEntry}
        autoCapitalize={autoCapitalize ?? 'none'}
        keyboardType={keyboardType ?? 'default'}
        multiline={multiline}
        autoFocus={autoFocus}
        maxLength={maxLength}
        accessibilityLabel={label}
        accessibilityHint={hint}
        style={{
          backgroundColor: theme.bg,
          borderWidth: 1,
          borderColor: theme.borderStrong,
          borderRadius: 10,
          paddingHorizontal: 12,
          paddingVertical: 12,
          minHeight: multiline ? 96 : MIN_TOUCH,
          textAlignVertical: multiline ? 'top' : 'center',
          color: theme.text,
          fontSize: 15,
        }}
      />
    </View>
  )
}

export function Button({
  label,
  onPress,
  loading,
  disabled,
  variant,
  hint,
}: {
  label: string
  onPress: () => void
  loading?: boolean
  disabled?: boolean
  variant?: 'primary' | 'danger' | 'ghost'
  hint?: string
}) {
  const bg = variant === 'danger' ? theme.danger : variant === 'ghost' ? 'transparent' : theme.primary
  const borderColor = variant === 'ghost' ? theme.borderStrong : bg
  const textColor = variant === 'ghost' ? theme.body : theme.primaryText
  const inert = !!loading || !!disabled
  return (
    <Pressable
      onPress={onPress}
      disabled={inert}
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityHint={hint}
      accessibilityState={{ disabled: inert, busy: !!loading }}
      style={{
        backgroundColor: bg,
        borderWidth: 1,
        borderColor,
        borderRadius: 10,
        paddingVertical: 12,
        minHeight: MIN_TOUCH,
        justifyContent: 'center',
        alignItems: 'center',
        opacity: inert ? 0.6 : 1,
      }}
    >
      {loading ? (
        <ActivityIndicator color={textColor} />
      ) : (
        <Text style={{ color: textColor, fontSize: 15, fontWeight: '600' }}>{label}</Text>
      )}
    </Pressable>
  )
}

/** Chip used for filters and single-choice pickers. */
export function Chip({
  label,
  selected,
  onPress,
  accessibilityLabel,
}: {
  label: string
  selected: boolean
  onPress: () => void
  accessibilityLabel?: string
}) {
  return (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={accessibilityLabel ?? label}
      accessibilityState={{ selected }}
      hitSlop={{ top: 8, bottom: 8, left: 4, right: 4 }}
      style={{
        paddingHorizontal: 12,
        paddingVertical: 8,
        minHeight: 36,
        justifyContent: 'center',
        borderRadius: 16,
        borderWidth: 1,
        borderColor: selected ? theme.primary : theme.borderStrong,
        backgroundColor: selected ? theme.primary : 'transparent',
      }}
    >
      <Text style={{ color: selected ? theme.primaryText : theme.body, fontSize: 13, fontWeight: '600' }}>
        {label}
      </Text>
    </Pressable>
  )
}

export function Centered({ children }: { children: React.ReactNode }) {
  return (
    <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center', padding: 32 }}>{children}</View>
  )
}

export function LoadingState({ label = 'Loading' }: { label?: string }) {
  return (
    <Centered>
      <ActivityIndicator color={theme.accent} accessibilityLabel={label} />
    </Centered>
  )
}

export function ErrorState({
  message,
  onRetry,
  retryLabel = 'Try again',
}: {
  message: string
  onRetry?: () => void
  retryLabel?: string
}) {
  return (
    <Centered>
      <Text
        accessibilityRole="alert"
        style={{ color: theme.danger, fontSize: 15, textAlign: 'center', marginBottom: 16 }}
      >
        {message}
      </Text>
      {onRetry ? (
        <View style={{ minWidth: 160 }}>
          <Button label={retryLabel} onPress={onRetry} variant="ghost" />
        </View>
      ) : null}
    </Centered>
  )
}

export function EmptyState({ title, body }: { title: string; body?: string }) {
  return (
    <View style={{ alignItems: 'center', paddingTop: 56, paddingHorizontal: 32 }}>
      <Text style={{ color: theme.body, fontSize: 15, fontWeight: '600', textAlign: 'center' }}>{title}</Text>
      {body ? (
        <Text style={{ color: theme.textMuted, fontSize: 13, textAlign: 'center', marginTop: 6, lineHeight: 19 }}>
          {body}
        </Text>
      ) : null}
    </View>
  )
}

/** Footer for a paginated list: spinner, "load more", or nothing. */
export function ListFooter({
  loading,
  hasMore,
  onLoadMore,
  label = 'Load more',
}: {
  loading: boolean
  hasMore: boolean
  onLoadMore: () => void
  label?: string
}) {
  if (loading) {
    return (
      <View style={{ paddingVertical: 20 }}>
        <ActivityIndicator color={theme.accent} accessibilityLabel="Loading more" />
      </View>
    )
  }
  if (!hasMore) return null
  return (
    <View style={{ padding: 16 }}>
      <Button label={label} onPress={onLoadMore} variant="ghost" />
    </View>
  )
}
