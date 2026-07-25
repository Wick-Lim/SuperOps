import React from 'react'
import { View, Text, TextInput, Pressable, ActivityIndicator } from 'react-native'
import type { StyleProp, ViewStyle } from 'react-native'
import { theme } from '../../lib/theme'
import { MIN_TOUCH, touchSlop } from '../../components/a11y'
import { CONTENT_MAX_WIDTH, byTier, useResponsive } from '../../lib/responsive'

// Re-exported so screens have one import for chrome + sizing.
export { MIN_TOUCH, touchSlop }
export { CONTENT_MAX_WIDTH }

/**
 * Widest a table-shaped list is allowed to get.
 *
 * `CONTENT_MAX_WIDTH` is a reading measure: it exists so a sentence does not
 * run the width of a monitor. A roster or an audit log is not a sentence — it
 * is four or five short columns — so it earns more room before the columns
 * start drifting apart, but still nothing like a full 1600px window.
 */
export const TABLE_MAX_WIDTH = 960

/**
 * Style for a scroll/list content container that should stop growing at a
 * measure and sit in the middle of whatever is left.
 *
 * Deliberately carries no horizontal padding: rows that draw their own divider
 * need to reach the edge of the column, so they pad themselves instead.
 */
export function contentColumn(maxWidth: number = CONTENT_MAX_WIDTH): ViewStyle {
  return { width: '100%', maxWidth, alignSelf: 'center' }
}

/**
 * The same measure as a wrapper, with the tier's gutter, for screens whose
 * content is a form or prose rather than full-bleed rows.
 */
export function ContentColumn({
  children,
  maxWidth = CONTENT_MAX_WIDTH,
  style,
}: {
  children: React.ReactNode
  maxWidth?: number
  style?: StyleProp<ViewStyle>
}) {
  const { gutter } = useResponsive()
  return <View style={[contentColumn(maxWidth), { paddingHorizontal: gutter }, style]}>{children}</View>
}

/** One column of a list that has been laid out as a table on medium/wide. */
export type Column = {
  key: string
  label: string
  /** Fixed width, for columns whose content has a known size (a status word). */
  width?: number
  /** Share of the leftover, for columns holding names or free text. */
  flex?: number
  align?: 'left' | 'right'
}

/** Cell box for `col`, so the header row and every data row cannot drift. */
export function cell(col: Column): ViewStyle {
  return {
    ...(col.flex ? { flex: col.flex, minWidth: 0 } : { width: col.width }),
    alignItems: col.align === 'right' ? 'flex-end' : 'flex-start',
  }
}

/**
 * Aligned header for a table-shaped list. Compact never renders one — there
 * are no columns there to label.
 */
export function TableHeader({ columns, gap = 12 }: { columns: Column[]; gap?: number }) {
  const { gutter } = useResponsive()
  return (
    <View
      style={{
        flexDirection: 'row',
        alignItems: 'center',
        gap,
        paddingHorizontal: gutter,
        paddingVertical: 8,
        borderBottomWidth: 1,
        borderBottomColor: theme.borderStrong,
      }}
    >
      {columns.map((c) => (
        <View key={c.key} style={cell(c)}>
          {/* An action column has no name; an empty header would still be
              announced, so it stays a spacer. */}
          {c.label ? (
            <Text
              accessibilityRole="header"
              style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 1 }}
              numberOfLines={1}
            >
              {c.label.toUpperCase()}
            </Text>
          ) : null}
        </View>
      ))}
    </View>
  )
}

/**
 * Shared screen chrome. The 56px header row was copy-pasted into eight screens
 * with no accessibility props at all; centralising it is what makes labelling
 * the back button (and hitting the 44x44 touch-target minimum) a one-line
 * change instead of an eight-file change.
 *
 * The bar itself always spans the window — a rule that stops mid-screen looks
 * like a bug — but `maxWidth` lines its contents up with a centred content
 * column so the title is not stranded on the far left of a monitor.
 */
export function ScreenHeader({
  title,
  onBack,
  backLabel = 'Back',
  right,
  subtitle,
  maxWidth,
}: {
  title: string
  onBack?: () => void
  backLabel?: string
  right?: React.ReactNode
  subtitle?: string
  maxWidth?: number
}) {
  const { tier, gutter, minTouch } = useResponsive()
  return (
    <View
      style={{
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
      }}
    >
      <View
        style={{
          ...(maxWidth ? contentColumn(maxWidth) : null),
          minHeight: byTier(tier, { compact: 56, medium: 48 }),
          paddingHorizontal: byTier(tier, { compact: 12, medium: gutter }),
          paddingVertical: 6,
          flexDirection: 'row',
          alignItems: 'center',
        }}
      >
        {onBack ? (
          <Pressable
            onPress={onBack}
            accessibilityRole="button"
            accessibilityLabel={backLabel}
            hitSlop={touchSlop(32)}
            style={{ minWidth: minTouch, minHeight: minTouch, justifyContent: 'center', paddingRight: 8 }}
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
  // Density follows the pointer: a 44px row is right under a thumb and looks
  // like a blown-up phone under a mouse.
  const { tier, minTouch } = useResponsive()
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
          paddingVertical: byTier(tier, { compact: 12, medium: 9 }),
          minHeight: multiline ? 96 : minTouch,
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
  inline,
}: {
  label: string
  onPress: () => void
  loading?: boolean
  disabled?: boolean
  variant?: 'primary' | 'danger' | 'ghost'
  hint?: string
  /**
   * Size to the label once there is room. A full-bleed button is the right
   * answer on a phone and absurd at 760px, but it stays opt-in: callers that
   * want a block button (a sign-in card, a list footer) keep one.
   */
  inline?: boolean
}) {
  const { tier, minTouch } = useResponsive()
  const bg = variant === 'danger' ? theme.danger : variant === 'ghost' ? 'transparent' : theme.primary
  const borderColor = variant === 'ghost' ? theme.borderStrong : bg
  const textColor = variant === 'ghost' ? theme.body : theme.primaryText
  const inert = !!loading || !!disabled
  const fitted = inline && tier !== 'compact'
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
        paddingVertical: byTier(tier, { compact: 12, medium: 9 }),
        paddingHorizontal: fitted ? 20 : 0,
        alignSelf: fitted ? 'flex-start' : 'auto',
        minWidth: fitted ? 160 : undefined,
        minHeight: minTouch,
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
    <View
      style={{
        alignItems: 'center',
        paddingTop: 56,
        paddingHorizontal: 32,
        // Two centred lines stretched across a monitor read as a mistake.
        maxWidth: 420,
        alignSelf: 'center',
      }}
    >
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
  const { tier } = useResponsive()
  if (loading) {
    return (
      <View style={{ paddingVertical: 20 }}>
        <ActivityIndicator color={theme.accent} accessibilityLabel="Loading more" />
      </View>
    )
  }
  if (!hasMore) return null
  const wide = tier !== 'compact'
  // Full-bleed under a thumb; a centred button under the list on a monitor.
  return (
    <View style={{ padding: 16, alignItems: wide ? 'center' : 'stretch' }}>
      <View style={{ minWidth: wide ? 220 : undefined }}>
        <Button label={label} onPress={onLoadMore} variant="ghost" />
      </View>
    </View>
  )
}
