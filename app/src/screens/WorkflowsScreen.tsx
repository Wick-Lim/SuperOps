import React, { useCallback, useEffect, useMemo, useState } from 'react'
import {
  SafeAreaView, View, Text, Pressable, ScrollView, TextInput, Switch, Alert, StyleSheet,
} from 'react-native'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import { errorMessage } from '../api/client'
import { workflowApi } from '../api/workflows'
import type {
  Catalogue, Workflow, WorkflowRejection, WorkflowRun, WorkflowStep, WorkflowStepRun,
} from '../api/workflows'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

/**
 * Automation.
 *
 * A FORM, NOT A GRAPH. The engine runs an ordered list of at most twenty steps
 * with no branching, so a node canvas would be a picture of something the
 * server cannot execute — it would let somebody draw a fork and then silently
 * run one arm. When branching exists, the editor can grow; until then the form
 * is an honest rendering of the model.
 *
 * The step and trigger vocabularies come from GET /workflow-steps. A hardcoded
 * list would offer steps this deployment has no adapter for, and the user would
 * find out when a run failed.
 */
export default function WorkflowsScreen({ navigation }: { navigation: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const [catalogue, setCatalogue] = useState<Catalogue | null>(null)
  const [workflows, setWorkflows] = useState<Workflow[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editing, setEditing] = useState<Workflow | 'new' | null>(null)

  const load = useCallback(async () => {
    if (!workspace) return
    setError(null)
    try {
      const [cat, list] = await Promise.all([
        workflowApi.catalogue(),
        workflowApi.list(workspace.id),
      ])
      setCatalogue(cat.data ?? null)
      setWorkflows(list.data ?? [])
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
    }
  }, [workspace])

  useEffect(() => {
    void load()
  }, [load])

  const toggle = useCallback(
    async (wf: Workflow, enabled: boolean) => {
      // Optimistic, because the switch must feel immediate — and reverted on
      // failure rather than left lying about the server's state.
      setWorkflows((prev) => prev.map((x) => (x.id === wf.id ? { ...x, enabled } : x)))
      try {
        await workflowApi.update(wf.id, {
          name: wf.name,
          description: wf.description,
          enabled,
          steps: wf.steps,
          triggers: wf.triggers,
        })
      } catch (e) {
        setWorkflows((prev) => prev.map((x) => (x.id === wf.id ? { ...x, enabled: !enabled } : x)))
        Alert.alert('Could not change that workflow', errorMessage(e))
      }
    },
    [],
  )

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Loading automation" />
      </SafeAreaView>
    )
  }

  if (editing && catalogue && workspace) {
    return (
      <WorkflowEditor
        workspaceId={workspace.id}
        catalogue={catalogue}
        workflow={editing === 'new' ? null : editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          void load()
        }}
      />
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title="Automation"
        subtitle={`${workflows.length} workflow${workflows.length === 1 ? '' : 's'}`}
        onBack={() => navigation.goBack()}
      />
      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : (
        <ScrollView>
          <ContentColumn>
            <Pressable
              onPress={() => setEditing('new')}
              style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
              accessibilityRole="button"
            >
              <Text style={styles.primaryText}>New workflow</Text>
            </Pressable>

            {workflows.length === 0 ? (
              <Text style={styles.empty}>
                Nothing automated yet. A workflow watches for something happening and then posts a
                message, notifies someone, or adds a comment.
              </Text>
            ) : (
              workflows.map((wf) => (
                <WorkflowRow key={wf.id} workflow={wf} onToggle={toggle} onOpen={() => setEditing(wf)} />
              ))
            )}
          </ContentColumn>
        </ScrollView>
      )}
    </SafeAreaView>
  )
}

function WorkflowRow({
  workflow,
  onToggle,
  onOpen,
}: {
  workflow: Workflow
  onToggle: (wf: Workflow, enabled: boolean) => void
  onOpen: () => void
}) {
  const [runs, setRuns] = useState<WorkflowRun[]>([])
  const [rejections, setRejections] = useState<WorkflowRejection[]>([])
  const [open, setOpen] = useState(false)

  useEffect(() => {
    if (!open) return
    // ONE PAGE, deliberately.
    //
    // I paginated this and the reasoning was wrong: the list renders
    // `runs.slice(0, 10)`, so walking twenty pages fetched a thousand runs to
    // show ten. Worse, `lastFailure` scans whatever was fetched — so a workflow
    // whose last thousand runs are green except one from months ago grew a
    // permanent red error banner about a failure nobody can act on.
    //
    // The most recent page IS the question this panel answers: is it running,
    // and did anything just break.
    let cancelled = false
    void workflowApi
      .runs(workflow.id)
      .then((r) => {
        if (!cancelled) setRuns(r.data ?? [])
      })
      .catch(() => undefined)
    void workflowApi
      .rejections(workflow.id)
      .then((r) => {
        if (!cancelled) setRejections(r.data ?? [])
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [open, workflow.id])

  // Among the runs SHOWN. Scoping it to the rendered slice is what keeps the
  // banner about something the reader can see for themselves.
  const shown = runs.slice(0, 10)
  const lastFailure = shown.find((r) => r.status === 'failed')

  return (
    <View style={styles.card}>
      <View style={styles.cardHead}>
        <Pressable onPress={() => setOpen((v) => !v)} style={{ flex: 1 }} accessibilityRole="button">
          <Text style={styles.cardTitle}>{workflow.name}</Text>
          <Text style={styles.cardMeta}>
            {workflow.triggers.map((t) => t.event_type).join(', ') || 'no trigger'} ·{' '}
            {workflow.steps.length} step{workflow.steps.length === 1 ? '' : 's'} · v{workflow.version}
          </Text>
        </Pressable>
        <Switch
          value={workflow.enabled}
          onValueChange={(v) => onToggle(workflow, v)}
          accessibilityLabel={`${workflow.enabled ? 'Disable' : 'Enable'} ${workflow.name}`}
        />
      </View>

      {open && (
        <View style={styles.cardBody}>
          <Pressable onPress={onOpen} accessibilityRole="button" hitSlop={8}>
            <Text style={styles.link}>Edit steps</Text>
          </Pressable>

          {/* THE TWO INVISIBLE FAILURES, made visible. "It never fired" and "it
              fired and was dropped" look identical on an enabled workflow. */}
          {rejections.length > 0 && (
            <Section title="Refused">
              {rejections.slice(0, 5).map((x, i) => (
                <Text key={i} style={styles.rejection}>
                  {x.event_type} — {x.reason}
                </Text>
              ))}
            </Section>
          )}

          <Section title="Recent runs">
            {runs.length === 0 ? (
              <Text style={styles.empty}>
                It has not run yet. Nothing it watches for has happened since it was enabled.
              </Text>
            ) : (
              shown.map((r) => <RunRow key={r.id} run={r} />)
            )}
            {/* The error verbatim: this is the only place anybody finds out
                why their automation is not working. */}
            {lastFailure?.error ? (
              <Text style={styles.failure}>{lastFailure.error}</Text>
            ) : null}
          </Section>
        </View>
      )}
    </View>
  )
}

/** One run, expandable to its steps.
 *
 * The run list says a run FAILED. Only the step detail says which step and
 * why — and "step 2 (post_message): the workflow owner may not write channel X"
 * is the difference between fixing it and guessing. */
function RunRow({ run }: { run: WorkflowRun }) {
  const [steps, setSteps] = useState<WorkflowStepRun[] | null>(null)
  const [open, setOpen] = useState(false)

  useEffect(() => {
    if (!open || steps) return
    void workflowApi
      .run(run.id)
      .then((r) => setSteps(r.data?.steps ?? []))
      .catch(() => setSteps([]))
  }, [open, steps, run.id])

  return (
    <View>
      <Pressable
        onPress={() => setOpen((v) => !v)}
        style={styles.runRow}
        accessibilityRole="button"
        accessibilityLabel={`Run ${run.status}, ${run.event_type}`}
      >
        <Text style={[styles.runStatus, statusStyle(run.status)]}>{run.status}</Text>
        <Text style={styles.runMeta} numberOfLines={1}>
          {run.event_type} · {new Date(run.created_at).toLocaleString()}
        </Text>
      </Pressable>
      {open &&
        (steps ?? []).map((s) => (
          <View key={s.step_index} style={styles.stepRow}>
            <Text style={[styles.stepStatus, statusStyle(s.status as WorkflowRun['status'])]}>
              {s.step_index + 1}. {s.kind} — {s.status}
            </Text>
            {s.error ? <Text style={styles.failure}>{s.error}</Text> : null}
          </View>
        ))}
    </View>
  )
}

function WorkflowEditor({
  workspaceId,
  catalogue,
  workflow,
  onClose,
  onSaved,
}: {
  workspaceId: string
  catalogue: Catalogue
  workflow: Workflow | null
  onClose: () => void
  onSaved: () => void
}) {
  const [name, setName] = useState(workflow?.name ?? '')
  const [trigger, setTrigger] = useState(workflow?.triggers[0]?.event_type ?? catalogue.triggers[0] ?? '')
  const [steps, setSteps] = useState<WorkflowStep[]>(workflow?.steps ?? [])
  const [saving, setSaving] = useState(false)

  const byKind = useMemo(
    () => Object.fromEntries(catalogue.steps.map((s) => [s.kind, s])),
    [catalogue],
  )

  const save = useCallback(async () => {
    if (name.trim() === '' || trigger === '') {
      Alert.alert('A workflow needs a name and a trigger')
      return
    }
    setSaving(true)
    try {
      const input = {
        name: name.trim(),
        // Saved DISABLED when it is new. A workflow that started running the
        // moment it was saved would fire on the half-built version somebody was
        // still editing.
        enabled: workflow?.enabled ?? false,
        steps,
        triggers: [{ event_type: trigger }],
      }
      if (workflow) await workflowApi.update(workflow.id, input)
      else await workflowApi.create(workspaceId, input)
      onSaved()
    } catch (e) {
      Alert.alert('Could not save that workflow', errorMessage(e))
    } finally {
      setSaving(false)
    }
  }, [name, trigger, steps, workflow, workspaceId, onSaved])

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={workflow ? 'Edit workflow' : 'New workflow'}
        subtitle={workflow ? `Version ${workflow.version}` : 'Saved disabled until you turn it on'}
        onBack={onClose}
      />
      <ScrollView>
        <ContentColumn>
          <Text style={styles.label}>Name</Text>
          <TextInput
            style={styles.input}
            value={name}
            onChangeText={setName}
            placeholder="Notify the on-call channel"
            placeholderTextColor={theme.textFaint}
            accessibilityLabel="Workflow name"
          />

          <Text style={styles.label}>When this happens</Text>
          <View style={styles.chips}>
            {catalogue.triggers.map((t) => (
              <Pressable
                key={t}
                onPress={() => setTrigger(t)}
                style={[styles.chip, trigger === t && styles.chipOn]}
                accessibilityRole="button"
              >
                <Text style={[styles.chipText, trigger === t && styles.chipTextOn]}>{t}</Text>
              </Pressable>
            ))}
          </View>

          <Section title="Do this, in order">
            {steps.map((step, i) => (
              <View key={i} style={styles.step}>
                <View style={styles.stepHead}>
                  <Text style={styles.stepTitle}>
                    {i + 1}. {byKind[step.kind]?.display_name ?? step.kind}
                  </Text>
                  <Pressable
                    onPress={() => setSteps((s) => s.filter((_, j) => j !== i))}
                    hitSlop={8}
                    accessibilityRole="button"
                  >
                    <Text style={styles.remove}>Remove</Text>
                  </Pressable>
                </View>
                {(byKind[step.kind]?.fields ?? []).map((field) => (
                  <View key={field}>
                    <Text style={styles.fieldLabel}>{field}</Text>
                    <TextInput
                      style={styles.input}
                      value={String(step.config[field] ?? '')}
                      onChangeText={(v) =>
                        setSteps((s) =>
                          s.map((x, j) => (j === i ? { ...x, config: { ...x.config, [field]: v } } : x)),
                        )
                      }
                      placeholder={field === 'body' ? 'Use {{event.user_id}} to insert a value' : field}
                      placeholderTextColor={theme.textFaint}
                      accessibilityLabel={`${step.kind} ${field}`}
                    />
                  </View>
                ))}
              </View>
            ))}

            <View style={styles.chips}>
              {catalogue.steps.map((s) => (
                <Pressable
                  key={s.kind}
                  onPress={() => setSteps((prev) => [...prev, { kind: s.kind, config: {} }])}
                  style={styles.chip}
                  accessibilityRole="button"
                >
                  <Text style={styles.chipText}>+ {s.display_name}</Text>
                </Pressable>
              ))}
            </View>
          </Section>

          <Pressable
            onPress={save}
            disabled={saving}
            style={({ pressed }) => [styles.primary, (pressed || saving) && styles.pressed]}
            accessibilityRole="button"
          >
            <Text style={styles.primaryText}>{saving ? 'Saving' : 'Save workflow'}</Text>
          </Pressable>

          {/* Deleting the workflow, not a step. The editor's other "Remove"
              controls remove a step, which is a different and much smaller
              thing — so this one says what it deletes and confirms. */}
          {workflow && (
            <Pressable
              onPress={() =>
                Alert.alert('Delete this workflow?', 'It stops running immediately.', [
                  { text: 'Cancel', style: 'cancel' },
                  {
                    text: 'Delete',
                    style: 'destructive',
                    onPress: async () => {
                      try {
                        await workflowApi.archive(workflow.id)
                        onSaved()
                      } catch (e) {
                        Alert.alert('Could not delete that workflow', errorMessage(e))
                      }
                    },
                  },
                ])
              }
              style={({ pressed }) => [styles.destructive, pressed && styles.pressed]}
              accessibilityRole="button"
            >
              <Text style={styles.destructiveText}>Delete workflow</Text>
            </Pressable>
          )}
        </ContentColumn>
      </ScrollView>
    </SafeAreaView>
  )
}

function statusStyle(status: WorkflowRun['status']) {
  switch (status) {
    case 'succeeded':
      return { color: theme.success }
    case 'failed':
      return { color: theme.danger }
    default:
      return { color: theme.textMuted }
  }
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  primary: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    backgroundColor: theme.primary,
    borderRadius: 8,
    marginVertical: space.md,
  },
  primaryText: { color: theme.primaryText, fontWeight: '600', fontSize: 15 },
  pressed: { opacity: 0.7 },
  empty: { color: theme.textFaint, fontSize: 14, paddingVertical: space.sm, lineHeight: 20 },

  card: {
    backgroundColor: theme.surface,
    borderRadius: 10,
    padding: space.sm,
    marginBottom: space.sm,
  },
  cardHead: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  cardTitle: { color: theme.text, fontSize: 15, fontWeight: '600' },
  cardMeta: { color: theme.textMuted, fontSize: 12, marginTop: 2 },
  cardBody: { marginTop: space.sm },
  link: { color: theme.accent, fontSize: 14, fontWeight: '600', paddingVertical: 6 },

  runRow: { flexDirection: 'row', alignItems: 'center', gap: space.sm, paddingVertical: 4 },
  runStatus: { fontSize: 12, fontWeight: '700', width: 78 },
  runMeta: { color: theme.textMuted, fontSize: 12, flex: 1 },
  failure: { color: theme.danger, fontSize: 12, marginTop: 6, lineHeight: 17 },
  rejection: { color: theme.textMuted, fontSize: 12, paddingVertical: 2 },

  label: { color: theme.textMuted, fontSize: 12, marginTop: space.sm, marginBottom: 4 },
  fieldLabel: { color: theme.textFaint, fontSize: 11, marginTop: 6 },
  input: {
    color: theme.text,
    fontSize: 14,
    backgroundColor: theme.surface,
    borderRadius: 8,
    paddingHorizontal: space.sm,
    paddingVertical: 10,
  },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: 6, marginBottom: space.sm },
  chip: {
    paddingHorizontal: space.sm,
    paddingVertical: 8,
    borderRadius: 16,
    backgroundColor: theme.surfaceAlt,
  },
  chipOn: { backgroundColor: theme.primary },
  chipText: { color: theme.body, fontSize: 13 },
  chipTextOn: { color: theme.primaryText, fontWeight: '600' },

  step: {
    backgroundColor: theme.surfaceAlt,
    borderRadius: 8,
    padding: space.sm,
    marginBottom: space.sm,
  },
  stepHead: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  stepTitle: { color: theme.text, fontSize: 14, fontWeight: '600' },
  remove: { color: theme.danger, fontSize: 13 },
  destructive: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 8,
    backgroundColor: theme.surfaceAlt,
    marginBottom: space.lg,
  },
  destructiveText: { color: theme.danger, fontWeight: '600', fontSize: 15 },
  stepRow: { paddingLeft: space.md, paddingVertical: 2 },
  stepStatus: { fontSize: 12 },
})
