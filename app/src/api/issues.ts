import { api } from './client'

/**
 * Work tracking.
 *
 * THE CLIENT NEVER SENDS A RANK. A move names the ANCHOR it was dropped next to
 * and the server derives the order — because the rank algebra is one
 * implementation on the server, and a client-computed rank would be a second one
 * evaluated against a board that may have moved underneath it.
 */

export type StateCategory = 'backlog' | 'unstarted' | 'started' | 'completed' | 'cancelled'

export interface Project {
  id: string
  workspace_id: string
  name: string
  key: string
  description: string
  archived_at: string | null
  created_at: string
}

export interface IssueState {
  id: string
  project_id: string
  name: string
  /** What everything generic reasons about — reports, "is it done", workflow
   * triggers. The NAMES are per-project and mean different things to two teams;
   * the categories are fixed. */
  category: StateCategory
  position: number
  color: string
}

export interface Issue {
  id: string
  workspace_id: string
  project_id: string
  number: number
  /** "PROJ-14" — what people paste into chat, so the server sends it rather than
   * every client assembling it from a prefix it would have to fetch. */
  key: string
  title: string
  description: string
  state_id: string
  assignee_id: string | null
  cycle_id: string | null
  priority: number
  rank: string
  completed_at: string | null
  archived_at: string | null
  created_by: string | null
  created_at: string
  updated_at: string
}

export interface IssuePage {
  issues: Issue[]
  /** (rank, id), not a time cursor: the list is ordered by RANK, and paginating
   * a rank-ordered list by time skips and repeats rows at every boundary. */
  next: { after_rank: string; after_id: string }
  has_more: boolean
}

/** PRIORITIES are ordered so a sort is a sort. 0 is "none" rather than "lowest"
 * — an unset priority is not a statement about urgency. */
export const PRIORITIES = [
  { value: 0, label: 'No priority' },
  { value: 1, label: 'Urgent' },
  { value: 2, label: 'High' },
  { value: 3, label: 'Medium' },
  { value: 4, label: 'Low' },
] as const

export const issueApi = {
  projects(workspaceId: string) {
    return api.get<Project[]>(`/workspaces/${workspaceId}/projects`)
  },

  createProject(workspaceId: string, name: string, key: string, description = '') {
    return api.post<Project>(`/workspaces/${workspaceId}/projects`, { name, key, description })
  },

  project(projectId: string) {
    return api.get<Project>(`/projects/${projectId}`)
  },

  states(projectId: string) {
    return api.get<IssueState[]>(`/projects/${projectId}/states`)
  },

  issues(projectId: string, opts: { stateId?: string; afterRank?: string; afterId?: string } = {}) {
    const q = new URLSearchParams()
    if (opts.stateId) q.set('state_id', opts.stateId)
    if (opts.afterRank) q.set('after_rank', opts.afterRank)
    if (opts.afterId) q.set('after_id', opts.afterId)
    const query = q.toString()
    return api.get<IssuePage>(`/projects/${projectId}/issues${query ? `?${query}` : ''}`)
  },

  /**
   * Every issue in a project, following the cursor.
   *
   * THE BOARD READ ONE PAGE. `issues()` returns the server's default 50 and the
   * caller read `data.issues` alone, so a project with more than 50 cards
   * silently lost the rest — and because the page is ordered by rank across all
   * states, whole columns came back empty while each header printed its own
   * length as if it were the total.
   *
   * Bounded rather than unbounded: a runaway cursor is worse than a truncated
   * board, and 20 pages is 1000 issues, well past any board a person reads. The
   * caller is told when it stopped early.
   */
  async allIssues(
    projectId: string,
    opts: { stateId?: string; maxPages?: number } = {},
  ): Promise<{ issues: Issue[]; truncated: boolean }> {
    const maxPages = opts.maxPages ?? 20
    const issues: Issue[] = []
    let after: { after_rank: string; after_id: string } | undefined
    for (let page = 0; page < maxPages; page++) {
      const res = await this.issues(projectId, {
        stateId: opts.stateId,
        afterRank: after?.after_rank,
        afterId: after?.after_id,
      })
      // `?? []` for the same reason collectPages guards its own: two functions
      // written together should not disagree about whether a null page is a
      // crash. The server does not send one; the asymmetry was the bug.
      issues.push(...(res.data?.issues ?? []))
      if (!res.data?.has_more || !res.data.next?.after_id) {
        return { issues, truncated: false }
      }
      after = res.data.next
    }
    return { issues, truncated: true }
  },

  createIssue(
    projectId: string,
    body: { title: string; description?: string; state_id?: string; assignee_id?: string; priority?: number },
  ) {
    return api.post<Issue>(`/projects/${projectId}/issues`, body)
  },

  issue(issueId: string) {
    return api.get<Issue>(`/issues/${issueId}`)
  },

  /** Every field is optional and `null` CLEARS. Omitting a field and sending null
   * are different requests — unassigning is `assignee_id: null`. */
  update(
    issueId: string,
    body: Partial<{ title: string; description: string; assignee_id: string | null; priority: number }>,
  ) {
    return api.patch<Issue>(`/issues/${issueId}`, body)
  },

  /**
   * Moves an issue. `after_id` / `before_id` are ANCHORS, not an order: the
   * server finds the real adjacent card, which is a different one each time as
   * a gap fills.
   *
   * Sending NEITHER anchor is a state change that preserves the position —
   * which is what dragging between columns without reordering means, and what
   * a "mark as done" button does.
   */
  move(issueId: string, body: { after_id?: string; before_id?: string; state_id?: string }) {
    return api.post<Issue>(`/issues/${issueId}/move`, body)
  },
}

/** categoryColor is the fallback when a state carries no colour of its own. The
 * CATEGORY decides it, not the name, because the name is per-project. */
export function categoryColor(category: StateCategory): string {
  switch (category) {
    case 'backlog':
      return '#64748b'
    case 'unstarted':
      return '#94a3b8'
    case 'started':
      return '#f59e0b'
    case 'completed':
      return '#22c55e'
    case 'cancelled':
      return '#ef4444'
  }
}
