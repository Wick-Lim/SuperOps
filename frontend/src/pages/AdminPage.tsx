import { useState, useEffect } from 'react'
import { adminApi } from '@/api/admin'
import Button from '@/components/shared/Button'

type Tab = 'stats' | 'users' | 'audit'

export default function AdminPage() {
  const [tab, setTab] = useState<Tab>('stats')

  return (
    <div className="flex-1 flex flex-col overflow-hidden">
      <header className="h-14 px-5 flex items-center gap-4 border-b border-surface-700/50 bg-surface-950 shrink-0">
        <h2 className="font-semibold text-white">Admin</h2>
        <div className="flex gap-1 ml-4">
          {(['stats', 'users', 'audit'] as Tab[]).map((t) => (
            <button key={t} onClick={() => setTab(t)}
              className={`px-3 py-1.5 rounded-md text-sm transition-colors ${tab === t ? 'bg-brand-600/20 text-brand-300' : 'text-surface-200 hover:bg-surface-800'}`}>
              {t === 'stats' ? 'Stats' : t === 'users' ? 'Users' : 'Audit Log'}
            </button>
          ))}
        </div>
      </header>
      <div className="flex-1 overflow-y-auto p-5">
        {tab === 'stats' && <StatsTab />}
        {tab === 'users' && <UsersTab />}
        {tab === 'audit' && <AuditTab />}
      </div>
    </div>
  )
}

function StatsTab() {
  const [stats, setStats] = useState<{ users: number; workspaces: number; channels: number; messages: number } | null>(null)
  useEffect(() => { adminApi.getStats().then((r) => setStats(r.data)).catch(() => {}) }, [])
  if (!stats) return <div className="text-surface-200/50">Loading...</div>
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
      {Object.entries(stats).map(([k, v]) => (
        <div key={k} className="bg-surface-900 border border-surface-700/50 rounded-xl p-5">
          <div className="text-3xl font-bold text-white">{v}</div>
          <div className="text-sm text-surface-200 mt-1 capitalize">{k}</div>
        </div>
      ))}
    </div>
  )
}

function UsersTab() {
  const [users, setUsers] = useState<Array<{ id: string; email: string; username: string; full_name: string; is_active: boolean }>>([])
  useEffect(() => { adminApi.listUsers().then((r) => setUsers(r.data)).catch(() => {}) }, [])
  return (
    <div className="bg-surface-900 border border-surface-700/50 rounded-xl overflow-hidden">
      <table className="w-full text-sm">
        <thead><tr className="border-b border-surface-700/50 text-surface-200 text-left">
          <th className="px-4 py-3 font-medium">Username</th>
          <th className="px-4 py-3 font-medium">Email</th>
          <th className="px-4 py-3 font-medium">Name</th>
          <th className="px-4 py-3 font-medium">Status</th>
          <th className="px-4 py-3 font-medium">Actions</th>
        </tr></thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} className="border-b border-surface-700/30">
              <td className="px-4 py-3 text-white">{u.username}</td>
              <td className="px-4 py-3 text-surface-200">{u.email}</td>
              <td className="px-4 py-3 text-surface-200">{u.full_name}</td>
              <td className="px-4 py-3">
                <span className={`text-xs px-2 py-0.5 rounded-full ${u.is_active ? 'bg-green-500/10 text-green-400' : 'bg-red-500/10 text-red-400'}`}>
                  {u.is_active ? 'Active' : 'Inactive'}
                </span>
              </td>
              <td className="px-4 py-3">
                <Button size="sm" variant={u.is_active ? 'danger' : 'primary'}
                  onClick={() => {
                    adminApi.updateUser(u.id, { is_active: !u.is_active })
                      .then(() => setUsers(users.map((x) => x.id === u.id ? { ...x, is_active: !u.is_active } : x)))
                  }}>
                  {u.is_active ? 'Deactivate' : 'Activate'}
                </Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function AuditTab() {
  const [logs, setLogs] = useState<Array<{ id: string; action: string; resource_type: string; actor_id: string; created_at: string }>>([])
  useEffect(() => { adminApi.getAuditLogs().then((r) => setLogs(r.data)).catch(() => {}) }, [])
  if (logs.length === 0) return <div className="text-surface-200/50">No audit logs yet.</div>
  return (
    <div className="bg-surface-900 border border-surface-700/50 rounded-xl overflow-hidden">
      <table className="w-full text-sm">
        <thead><tr className="border-b border-surface-700/50 text-surface-200 text-left">
          <th className="px-4 py-3 font-medium">Action</th>
          <th className="px-4 py-3 font-medium">Resource</th>
          <th className="px-4 py-3 font-medium">Actor</th>
          <th className="px-4 py-3 font-medium">Time</th>
        </tr></thead>
        <tbody>
          {logs.map((l) => (
            <tr key={l.id} className="border-b border-surface-700/30">
              <td className="px-4 py-3 text-white">{l.action}</td>
              <td className="px-4 py-3 text-surface-200">{l.resource_type}</td>
              <td className="px-4 py-3 text-surface-200">{String(l.actor_id || '').slice(0, 8)}</td>
              <td className="px-4 py-3 text-surface-200">{new Date(l.created_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
