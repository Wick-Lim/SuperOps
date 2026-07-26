import { api, withPaging } from './client'

/**
 * Automation.
 *
 * The step and trigger vocabularies come from the SERVER, not from a constant
 * here. A client with its own list offers steps the executor has no adapter for
 * and triggers nothing is bound to, and the user finds out when a run fails —
 * which is the same reason the Drive registry is fetched rather than hardcoded.
 */

export interface WorkflowStep {
  kind: string
  config: Record<string, unknown>
}

export interface WorkflowTrigger {
  event_type: string
  filter?: Record<string, unknown>
}

export interface Workflow {
  id: string
  workspace_id: string
  /** WHO THE WORKFLOW RUNS AS. Every action authorizes against this person, so
   * a workflow can never do something its owner could not do by hand. */
  owner_id: string
  name: string
  description: string
  enabled: boolean
  version: number
  steps: WorkflowStep[]
  triggers: WorkflowTrigger[]
  created_at: string
  archived_at: string | null
}

export interface WorkflowRun {
  id: string
  workflow_id: string
  event_type: string
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  /** Carried verbatim, because the run list is the only place anybody finds out
   * why their automation is not working. */
  error: string
  started_at: string | null
  finished_at: string | null
  created_at: string
}

export interface WorkflowStepRun {
  step_index: number
  kind: string
  status: 'pending' | 'succeeded' | 'failed' | 'skipped'
  error: string
  output: unknown
  finished_at: string | null
}

/** A trigger that matched and was refused. The two ways automation fails
 * invisibly — never fired, and fired then dropped — look identical without it. */
export interface WorkflowRejection {
  event_type: string
  reason: string
  created_at: string
}

export interface StepDescriptor {
  kind: string
  display_name: string
  fields: string[]
}

export interface Catalogue {
  steps: StepDescriptor[]
  triggers: string[]
}

export interface WorkflowInput {
  name: string
  description?: string
  enabled: boolean
  steps: WorkflowStep[]
  triggers: WorkflowTrigger[]
}

export const workflowApi = {
  /** What THIS deployment can actually do. */
  catalogue() {
    return api.get<Catalogue>('/workflow-steps')
  },

  list(workspaceId: string) {
    return api.get<Workflow[]>(`/workspaces/${workspaceId}/workflows`)
  },

  get(workflowId: string) {
    return api.get<Workflow>(`/workflows/${workflowId}`)
  },

  create(workspaceId: string, input: WorkflowInput) {
    return api.post<Workflow>(`/workspaces/${workspaceId}/workflows`, input)
  },

  update(workflowId: string, input: WorkflowInput) {
    return api.patch<Workflow>(`/workflows/${workflowId}`, input)
  },

  /** Archives AND disables. A hidden workflow that still fires is the worst
   * combination, so the two are one action. */
  archive(workflowId: string) {
    return api.del<void>(`/workflows/${workflowId}`)
  },

  runs(workflowId: string, cursor?: string) {
    return api.get<WorkflowRun[]>(withPaging(`/workflows/${workflowId}/runs`, cursor))
  },

  run(runId: string) {
    return api.get<{ run: WorkflowRun; steps: WorkflowStepRun[] }>(`/runs/${runId}`)
  },

  rejections(workflowId: string) {
    return api.get<WorkflowRejection[]>(`/workflows/${workflowId}/rejections`)
  },
}
