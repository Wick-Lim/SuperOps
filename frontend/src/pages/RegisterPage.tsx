import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { authApi } from '@/api/auth'
import { useAuthStore } from '@/stores/authStore'

export default function RegisterPage() {
  const [form, setForm] = useState({ email: '', username: '', full_name: '', password: '' })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()
  const { setTokens, setUser } = useAuthStore()

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)

    try {
      await authApi.register(form)
      const loginRes = await authApi.login({ email: form.email, password: form.password })
      setTokens(loginRes.data.access_token, loginRes.data.refresh_token)
      const me = await authApi.getMe()
      setUser(me.data)
      navigate('/setup')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Registration failed')
    } finally {
      setLoading(false)
    }
  }

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-950 px-4">
      <div className="w-full max-w-sm">
        <div className="text-center mb-8">
          <div className="w-12 h-12 bg-brand-600 rounded-xl mx-auto mb-4 flex items-center justify-center text-white font-bold text-xl">
            S
          </div>
          <h1 className="text-2xl font-bold text-white">Create Account</h1>
          <p className="text-surface-200 mt-1">Get started with SuperOps</p>
        </div>

        <form onSubmit={handleSubmit} className="space-y-4">
          {error && (
            <div className="bg-red-500/10 border border-red-500/20 rounded-lg p-3 text-red-400 text-sm">
              {error}
            </div>
          )}

          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">Full Name</label>
            <input type="text" value={form.full_name} onChange={update('full_name')} required
              className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="John Doe" />
          </div>
          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">Username</label>
            <input type="text" value={form.username} onChange={update('username')} required
              className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="johndoe" />
          </div>
          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">Email</label>
            <input type="email" value={form.email} onChange={update('email')} required
              className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="you@example.com" />
          </div>
          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">Password</label>
            <input type="password" value={form.password} onChange={update('password')} required minLength={8}
              className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="Min. 8 characters" />
          </div>

          <button type="submit" disabled={loading}
            className="w-full py-2.5 bg-brand-600 hover:bg-brand-700 disabled:opacity-50 rounded-lg text-white font-medium transition-colors">
            {loading ? 'Creating account...' : 'Create Account'}
          </button>
        </form>

        <p className="text-center mt-6 text-surface-200 text-sm">
          Already have an account?{' '}
          <Link to="/login" className="text-brand-400 hover:text-brand-300">Sign in</Link>
        </p>
      </div>
    </div>
  )
}
