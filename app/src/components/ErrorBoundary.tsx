import React from 'react'
import { View, Text, Pressable } from 'react-native'
import { theme } from '../lib/theme'

interface State {
  error: Error | null
}

// Top-level error boundary so a render error in any screen shows a recovery
// UI instead of a blank/crashed app.
export default class ErrorBoundary extends React.Component<{ children: React.ReactNode }, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error) {
    // eslint-disable-next-line no-console
    console.error('Unhandled UI error:', error)
  }

  render() {
    if (this.state.error) {
      return (
        <View style={{ flex: 1, backgroundColor: theme.bg, justifyContent: 'center', alignItems: 'center', padding: 32 }}>
          <Text style={{ color: theme.text, fontSize: 18, fontWeight: '700', marginBottom: 8 }}>Something went wrong</Text>
          <Text style={{ color: theme.textMuted, textAlign: 'center', marginBottom: 24 }}>
            {this.state.error.message}
          </Text>
          <Pressable
            onPress={() => this.setState({ error: null })}
            style={{ backgroundColor: theme.primary, borderRadius: 12, paddingHorizontal: 24, paddingVertical: 12 }}
          >
            <Text style={{ color: theme.primaryText, fontWeight: '600' }}>Try again</Text>
          </Pressable>
        </View>
      )
    }
    return this.props.children
  }
}
