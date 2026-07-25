import React, { useMemo, useState } from 'react'
import { Image, Modal, Pressable, ScrollView, Text, TextInput, View } from 'react-native'
import { theme } from '../../lib/theme'
import { space, useResponsive } from '../../lib/responsive'
import { MIN_TOUCH, useModalFocus } from '../a11y'
import { clamp, type Anchor } from './anchor'
import { customEmojiUrl, useCustomEmoji } from './customEmoji'

interface Props {
  visible: boolean
  onClose: () => void
  onSelect: (emoji: string) => void
  /**
   * Where the press that opened this landed. With a pointer the picker opens
   * there as a popover; without one (or on a phone) it stays a bottom sheet,
   * which is the right shape for a thumb.
   */
  anchor?: Anchor | null
}

/** Eight columns of ~36px plus the card's own padding. */
const POPOVER_WIDTH = 320
const POPOVER_MAX_HEIGHT = 420

/** `name` doubles as the search index and the accessibility label. */
interface Entry {
  char: string
  name: string
}

const EMOJIS: Entry[] = [
  { char: '👍', name: 'thumbs up yes ok' },
  { char: '👎', name: 'thumbs down no' },
  { char: '❤️', name: 'heart love' },
  { char: '😂', name: 'joy laugh funny' },
  { char: '🎉', name: 'tada party celebrate' },
  { char: '🙏', name: 'pray thanks please' },
  { char: '🔥', name: 'fire hot' },
  { char: '👀', name: 'eyes looking' },
  { char: '😍', name: 'heart eyes love' },
  { char: '😎', name: 'cool sunglasses' },
  { char: '🤔', name: 'thinking hmm' },
  { char: '😢', name: 'sad cry' },
  { char: '😮', name: 'surprised wow' },
  { char: '😡', name: 'angry mad' },
  { char: '🥳', name: 'party celebrate' },
  { char: '😴', name: 'sleep tired' },
  { char: '✅', name: 'check done complete' },
  { char: '❌', name: 'cross no fail' },
  { char: '⭐', name: 'star favourite' },
  { char: '💯', name: 'hundred perfect' },
  { char: '🚀', name: 'rocket ship launch' },
  { char: '👏', name: 'clap applause' },
  { char: '🙌', name: 'raised hands praise' },
  { char: '💪', name: 'muscle strong' },
  { char: '🤝', name: 'handshake deal agree' },
  { char: '👋', name: 'wave hello bye' },
  { char: '🤩', name: 'star struck amazing' },
  { char: '😅', name: 'sweat nervous laugh' },
  { char: '😬', name: 'grimace awkward' },
  { char: '🤯', name: 'mind blown' },
  { char: '💀', name: 'skull dead' },
  { char: '🤖', name: 'robot bot' },
  { char: '🍕', name: 'pizza food' },
  { char: '☕', name: 'coffee break' },
  { char: '🎯', name: 'target goal bullseye' },
  { char: '💡', name: 'idea light bulb' },
  { char: '⚡', name: 'zap fast lightning' },
  { char: '🌟', name: 'glowing star' },
  { char: '✨', name: 'sparkles shiny' },
  { char: '❓', name: 'question help' },
  { char: '🐛', name: 'bug defect' },
  { char: '🚢', name: 'ship shipit deploy' },
  { char: '🔧', name: 'wrench fix tool' },
  { char: '📈', name: 'chart up growth' },
  { char: '📉', name: 'chart down drop' },
  { char: '🧠', name: 'brain smart' },
  { char: '🫡', name: 'salute acknowledged' },
  { char: '🙈', name: 'see no evil monkey oops' },
]

export default function EmojiPicker({ visible, onClose, onSelect, anchor }: Props) {
  const [query, setQuery] = useState('')
  const custom = useCustomEmoji()
  const heading = useModalFocus(visible)
  const { tier, minTouch, width, height } = useResponsive()

  const q = query.trim().toLowerCase()
  const builtins = useMemo(() => (q ? EMOJIS.filter((e) => e.name.includes(q)) : EMOJIS), [q])
  const customs = useMemo(
    () => (q ? custom.filter((e) => e.name.toLowerCase().includes(q)) : custom),
    [custom, q],
  )

  const choose = (value: string) => {
    setQuery('')
    onSelect(value)
    onClose()
  }

  const close = () => {
    setQuery('')
    onClose()
  }

  // A popover needs a pointer to hang off AND room to hang in; a phone-width
  // window has neither, so it keeps the sheet.
  const popover = tier !== 'compact' && !!anchor

  const cell = {
    width: '12.5%' as const,
    aspectRatio: 1,
    minHeight: popover ? minTouch : MIN_TOUCH,
    alignItems: 'center' as const,
    justifyContent: 'center' as const,
  }

  const cardHeight = Math.min(POPOVER_MAX_HEIGHT, height - 2 * space.lg)
  const card = popover
    ? {
        position: 'absolute' as const,
        width: POPOVER_WIDTH,
        maxHeight: cardHeight,
        // Centred under the pointer, then pushed back inside the window rather
        // than allowed to hang off the edge of a message near the right rail.
        left: clamp(anchor!.x - POPOVER_WIDTH / 2, space.lg, width - POPOVER_WIDTH - space.lg),
        top: clamp(anchor!.y + space.md, space.lg, height - cardHeight - space.lg),
        backgroundColor: theme.surface,
        borderRadius: 14,
        borderWidth: 1,
        borderColor: theme.borderStrong,
        paddingHorizontal: space.md,
        paddingTop: space.md,
        paddingBottom: space.md,
      }
    : {
        backgroundColor: theme.surface,
        borderTopLeftRadius: 20,
        borderTopRightRadius: 20,
        paddingHorizontal: 16,
        paddingTop: 12,
        paddingBottom: 28,
        borderTopWidth: 1,
        borderColor: theme.border,
      }

  return (
    <Modal visible={visible} transparent animationType={popover ? 'none' : 'fade'} onRequestClose={close}>
      <Pressable
        onPress={close}
        accessibilityRole="button"
        accessibilityLabel="Close emoji picker"
        style={{
          flex: 1,
          // A popover is a light-touch attachment to the thing it acts on;
          // dimming the whole app for it would read as a full modal.
          backgroundColor: popover ? 'transparent' : 'rgba(0,0,0,0.6)',
          justifyContent: 'flex-end',
        }}
      >
        <Pressable onPress={(e) => e.stopPropagation()} accessibilityViewIsModal style={card}>
          {!popover && (
            <View style={{ alignItems: 'center', marginBottom: 12 }}>
              <View style={{ width: 40, height: 4, borderRadius: 2, backgroundColor: theme.borderStrong }} />
            </View>
          )}

          <View {...heading} accessible accessibilityRole="header">
            <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '700', letterSpacing: 1 }}>
              PICK A REACTION
            </Text>
          </View>

          <TextInput
            value={query}
            onChangeText={setQuery}
            placeholder="Search emoji"
            placeholderTextColor={theme.textMuted}
            accessibilityLabel="Search emoji"
            autoCapitalize="none"
            autoCorrect={false}
            style={{
              marginTop: 10,
              marginBottom: 8,
              backgroundColor: theme.bg,
              borderWidth: 1,
              borderColor: theme.borderStrong,
              borderRadius: 12,
              paddingHorizontal: 12,
              minHeight: popover ? minTouch : MIN_TOUCH,
              color: theme.text,
              fontSize: 15,
            }}
          />

          <ScrollView
            style={{ maxHeight: popover ? cardHeight - 120 : 280 }}
            keyboardShouldPersistTaps="handled"
          >
            {customs.length > 0 && (
              <>
                <Text style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', marginBottom: 6 }}>
                  WORKSPACE
                </Text>
                <View style={{ flexDirection: 'row', flexWrap: 'wrap', marginBottom: 12 }}>
                  {customs.map((e) => (
                    <Pressable
                      key={e.id}
                      onPress={() => choose(`:${e.name}:`)}
                      accessibilityRole="button"
                      accessibilityLabel={`React with ${e.name}`}
                      style={cell}
                    >
                      <Image
                        source={{ uri: customEmojiUrl(e) }}
                        style={{ width: 26, height: 26 }}
                        resizeMode="contain"
                      />
                    </Pressable>
                  ))}
                </View>
              </>
            )}

            {builtins.length > 0 ? (
              <View style={{ flexDirection: 'row', flexWrap: 'wrap' }}>
                {builtins.map((e) => (
                  <Pressable
                    key={e.char}
                    onPress={() => choose(e.char)}
                    accessibilityRole="button"
                    accessibilityLabel={`React with ${e.name.split(' ')[0]}`}
                    style={cell}
                  >
                    <Text style={{ fontSize: 26 }}>{e.char}</Text>
                  </Pressable>
                ))}
              </View>
            ) : (
              customs.length === 0 && (
                <Text style={{ color: theme.textMuted, fontSize: 14, paddingVertical: 16, textAlign: 'center' }}>
                  No emoji match “{query.trim()}”.
                </Text>
              )
            )}
          </ScrollView>
        </Pressable>
      </Pressable>
    </Modal>
  )
}
