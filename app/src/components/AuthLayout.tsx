import type { ReactNode } from 'react'
import { SafeAreaView, ScrollView, View } from 'react-native'

/**
 * Centred, width-capped shell for the unauthenticated screens.
 *
 * These screens were laid out for a phone, where "fill the width minus 32px of
 * padding" is right. The same rule on a desktop browser — the app also ships as
 * a react-native-web build — stretches a two-field sign-in form across the whole
 * monitor, so the inputs read as absurdly long and the eye has to travel the
 * full width between label and value.
 *
 * `maxWidth` with `alignSelf: 'center'` is the whole fix: below the cap the
 * layout is unchanged on phones, above it the form stays a readable column.
 *
 * The ScrollView matters on short desktop windows and on a phone with the
 * keyboard up — without it a vertically-centred form simply gets clipped.
 */

/** Roughly 60 characters at the form's font size: comfortable for a short form. */
export const AUTH_MAX_WIDTH = 420

export function AuthLayout({
  children,
  maxWidth = AUTH_MAX_WIDTH,
}: {
  children: ReactNode
  maxWidth?: number
}) {
  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <ScrollView
        style={{ flex: 1 }}
        contentContainerStyle={{
          flexGrow: 1,
          justifyContent: 'center',
          paddingHorizontal: 32,
          paddingVertical: 24,
        }}
        keyboardShouldPersistTaps="handled"
      >
        <View style={{ width: '100%', maxWidth, alignSelf: 'center' }}>{children}</View>
      </ScrollView>
    </SafeAreaView>
  )
}
