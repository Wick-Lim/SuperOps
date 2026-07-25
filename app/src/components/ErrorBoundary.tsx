import React from 'react'
import { ScrollView, Text, Pressable, View } from 'react-native'
import { theme } from '../lib/theme'
import { MIN_TOUCH } from './a11y'

interface Props {
  children: React.ReactNode
  /** Ask the host to drop whatever state produced the crash before remounting. */
  onReset?: () => void
}

interface State {
  error: Error | null
  componentStack: string | null
  /** Changing this remounts the subtree. */
  generation: number
  resetCount: number
}

const MAX_QUIET_RESETS = 2

/**
 * Top-level error boundary.
 *
 * "Try again" used to only clear `error`, re-rendering the *same* element tree
 * with the same props — a deterministic render crash re-threw immediately and
 * the button did nothing. The children are now keyed on a generation counter,
 * so a retry genuinely remounts them (fresh state, effects re-run). After a
 * couple of failed retries the copy stops promising a fix.
 */
export default class ErrorBoundary extends React.Component<Props, State> {
  state: State = { error: null, componentStack: null, generation: 0, resetCount: 0 }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    this.setState({ componentStack: info.componentStack ?? null })
    // The component stack is the only thing that identifies WHICH screen threw;
    // logging the error alone made these reports unactionable.
    // eslint-disable-next-line no-console
    console.error('Unhandled UI error:', error, info.componentStack)
  }

  private reset = () => {
    this.props.onReset?.()
    this.setState((s) => ({
      error: null,
      componentStack: null,
      generation: s.generation + 1,
      resetCount: s.resetCount + 1,
    }))
  }

  render() {
    const { error, componentStack, generation, resetCount } = this.state

    if (error) {
      const repeated = resetCount >= MAX_QUIET_RESETS
      return (
        <View style={{ flex: 1, backgroundColor: theme.bg, justifyContent: 'center', padding: 32 }}>
          <Text
            accessibilityRole="header"
            style={{ color: theme.text, fontSize: 18, fontWeight: '700', marginBottom: 8, textAlign: 'center' }}
          >
            Something went wrong
          </Text>
          <Text style={{ color: theme.textMuted, textAlign: 'center', marginBottom: 20 }}>
            {error.message || 'The screen could not be rendered.'}
          </Text>

          {repeated && (
            <Text style={{ color: theme.warning, textAlign: 'center', fontSize: 13, marginBottom: 20 }}>
              This keeps failing. Restarting the app may be the only way out.
            </Text>
          )}

          {__DEV__ && !!componentStack && (
            <ScrollView style={{ maxHeight: 160, marginBottom: 20 }}>
              <Text style={{ color: theme.textMuted, fontSize: 11, fontFamily: 'monospace' }}>
                {componentStack.trim()}
              </Text>
            </ScrollView>
          )}

          <Pressable
            onPress={this.reset}
            accessibilityRole="button"
            accessibilityLabel="Try again"
            accessibilityHint="Reloads the screen"
            style={{
              alignSelf: 'center',
              backgroundColor: theme.primary,
              borderRadius: 12,
              paddingHorizontal: 24,
              minHeight: MIN_TOUCH,
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.primaryText, fontWeight: '600' }}>Try again</Text>
          </Pressable>
        </View>
      )
    }

    // Keyed fragment: the remount is the whole point of the retry.
    return <React.Fragment key={generation}>{this.props.children}</React.Fragment>
  }
}
