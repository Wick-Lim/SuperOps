import { Link } from 'react-router-dom'

export default function NotFoundPage() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-950 px-4">
      <div className="text-center">
        <div className="text-6xl font-bold text-surface-700 mb-4">404</div>
        <h1 className="text-xl font-bold text-white mb-2">Page not found</h1>
        <p className="text-surface-200 text-sm mb-6">The page you're looking for doesn't exist.</p>
        <Link to="/" className="px-4 py-2 bg-brand-600 hover:bg-brand-700 rounded-lg text-white text-sm font-medium transition-colors">
          Back to Home
        </Link>
      </div>
    </div>
  )
}
