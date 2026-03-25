import { Component, type ReactNode } from 'react'

interface Props {
  children: ReactNode
}

interface State {
  hasError: boolean
  error: Error | null
}

export default class ErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = { hasError: false, error: null }
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error }
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="min-h-screen flex items-center justify-center bg-surface-950 px-4">
          <div className="text-center max-w-md">
            <div className="w-16 h-16 bg-red-500/10 rounded-2xl mx-auto mb-6 flex items-center justify-center text-red-400 text-2xl">
              !
            </div>
            <h1 className="text-xl font-bold text-white mb-2">Something went wrong</h1>
            <p className="text-surface-200 text-sm mb-6">
              {this.state.error?.message || 'An unexpected error occurred'}
            </p>
            <button
              onClick={() => { this.setState({ hasError: false, error: null }); window.location.href = '/' }}
              className="px-4 py-2 bg-brand-600 hover:bg-brand-700 rounded-lg text-white text-sm font-medium transition-colors"
            >
              Back to Home
            </button>
          </div>
        </div>
      )
    }

    return this.props.children
  }
}
