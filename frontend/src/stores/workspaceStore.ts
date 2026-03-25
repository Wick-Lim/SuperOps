import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import type { Workspace } from '@/lib/types'

interface WorkspaceState {
  workspaces: Workspace[]
  activeWorkspace: Workspace | null
  setWorkspaces: (ws: Workspace[]) => void
  setActiveWorkspace: (ws: Workspace) => void
}

export const useWorkspaceStore = create<WorkspaceState>()(
  persist(
    (set) => ({
      workspaces: [],
      activeWorkspace: null,
      setWorkspaces: (workspaces) => set({ workspaces }),
      setActiveWorkspace: (ws) => set({ activeWorkspace: ws }),
    }),
    { name: 'superops-workspace' }
  )
)
