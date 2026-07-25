import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Pressable, ScrollView, FlatList, Alert, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { issueApi, categoryColor, PRIORITIES } from '../api/issues'
import type { Issue, IssueState, Project } from '../api/issues'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useResponsive, space, MIN_TOUCH, PANE } from '../lib/responsive'
import { ContentColumn, EmptyState, ErrorState, LoadingState, ScreenHeader } from './internal/ui'

/**
 * The board.
 *
 * One column per state, issues in RANK order. The columns come from the server
 * because state names are per-project — "In review" means different things to
 * two teams — while the CATEGORY is fixed and is what the colour and every
 * report reason about.
 *
 * On a narrow screen the columns stack into a single scrolling list rather than
 * shrinking: three 100pt columns is a board nobody can read, and a horizontal
 * scroll inside a vertical one is a gesture that fights itself.
 */
export default function BoardScreen({ navigation, route }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const workspaceId = activeWorkspace?.id
  const projectId: string | undefined = route.params?.projectId

  const [projects, setProjects] = useState<Project[]>([])
  const [project, setProject] = useState<Project | null>(null)
  const [states, setStates] = useState<IssueState[]>([])
  const [issues, setIssues] = useState<Issue[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const { tier } = useResponsive()
  const stacked = tier === 'compact'

  const loadProject = useCallback(async (id: string) => {
    setError(null)
    try {
      const [p, s, i] = await Promise.all([
        issueApi.project(id),
        issueApi.states(id),
        issueApi.issues(id),
      ])
      setProject(p.data)
      setStates(s.data)
      setIssues(i.data.issues)
    } catch (e) {
      setError(errorMessage(e))
    }
  }, [])

  useEffect(() => {
    if (!workspaceId) return
    let cancelled = false
    ;(async () => {
      setLoading(true)
      try {
        const list = await issueApi.projects(workspaceId)
        if (cancelled) return
        setProjects(list.data)
        const target = projectId || list.data[0]?.id
        if (target) await loadProject(target)
      } catch (e) {
        if (!cancelled) setError(errorMessage(e))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [workspaceId, projectId, loadProject])

  const newProject = useCallback(() => {
    if (!workspaceId) return
    promptFor('New project', 'Untitled', async (name) => {
      // The key is derived from the name and uppercased, because it is what
      // people type and paste — asking for it separately is a second field
      // whose answer is almost always the first one abbreviated.
      const key = name.replace(/[^A-Za-z0-9]/g, '').slice(0, 6).toUpperCase() || 'PROJ'
      try {
        const created = await issueApi.createProject(workspaceId, name, key)
        setProjects((p) => [...p, created.data])
        await loadProject(created.data.id)
      } catch (e) {
        Alert.alert('Could not create the project', errorMessage(e))
      }
    })
  }, [workspaceId, loadProject])

  const newIssue = useCallback(
    (stateId: string) => {
      if (!project) return
      promptFor('New issue', 'Untitled issue', async (title) => {
        try {
          await issueApi.createIssue(project.id, { title, state_id: stateId })
          await loadProject(project.id)
        } catch (e) {
          Alert.alert('Could not create the issue', errorMessage(e))
        }
      })
    },
    [project, loadProject],
  )

  // Moving between columns without reordering: NEITHER anchor is sent, which
  // the server reads as "change the state, keep the position". Sending an
  // anchor here would send the card to the top of its new column, which is not
  // what pressing "move to Done" means.
  const moveTo = useCallback(
    async (issue: Issue, stateId: string) => {
      try {
        await issueApi.move(issue.id, { state_id: stateId })
        await loadProject(issue.project_id)
      } catch (e) {
        Alert.alert('Could not move the issue', errorMessage(e))
      }
    },
    [loadProject],
  )

  if (!workspaceId) {
    return (
      <SafeAreaView style={styles.screen}>
        <EmptyState title="No workspace" body="Choose a workspace to see its work." />
      </SafeAreaView>
    )
  }

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader
        title={project?.name ?? 'Work'}
        subtitle={project ? project.key : undefined}
        onBack={() => navigation.goBack()}
      />

      <ContentColumn>
        <ScrollView horizontal showsHorizontalScrollIndicator={false} style={styles.projectBar}>
          {projects.map((p) => (
            <Pressable
              key={p.id}
              onPress={() => void loadProject(p.id)}
              style={[styles.projectChip, project?.id === p.id && styles.projectChipActive]}
              accessibilityRole="button"
              accessibilityState={{ selected: project?.id === p.id }}
            >
              <Text style={project?.id === p.id ? styles.projectChipTextActive : styles.projectChipText}>
                {p.key}
              </Text>
            </Pressable>
          ))}
          <Pressable onPress={newProject} style={styles.projectChip} accessibilityRole="button">
            <Text style={styles.projectChipText}>+ Project</Text>
          </Pressable>
        </ScrollView>
      </ContentColumn>

      {loading ? (
        <LoadingState label="Loading the board" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => project && void loadProject(project.id)} />
      ) : !project ? (
        <EmptyState title="No projects yet" body="Create one to start tracking work." />
      ) : stacked ? (
        <ScrollView>
          {states.map((s) => (
            <Column
              key={s.id}
              state={s}
              issues={issues.filter((i) => i.state_id === s.id)}
              states={states}
              stacked
              onOpen={(i) => navigation.navigate('IssueDetail', { issueId: i.id })}
              onAdd={() => newIssue(s.id)}
              onMove={moveTo}
            />
          ))}
        </ScrollView>
      ) : (
        <ScrollView horizontal showsHorizontalScrollIndicator>
          {states.map((s) => (
            <Column
              key={s.id}
              state={s}
              issues={issues.filter((i) => i.state_id === s.id)}
              states={states}
              stacked={false}
              onOpen={(i) => navigation.navigate('IssueDetail', { issueId: i.id })}
              onAdd={() => newIssue(s.id)}
              onMove={moveTo}
            />
          ))}
        </ScrollView>
      )}
    </SafeAreaView>
  )
}

function Column({
  state,
  issues,
  states,
  stacked,
  onOpen,
  onAdd,
  onMove,
}: {
  state: IssueState
  issues: Issue[]
  states: IssueState[]
  stacked: boolean
  onOpen: (i: Issue) => void
  onAdd: () => void
  onMove: (i: Issue, stateId: string) => void
}) {
  return (
    <View style={[styles.column, !stacked && { width: PANE.thread }]}>
      <View style={styles.columnHeader}>
        <View style={[styles.dot, { backgroundColor: state.color || categoryColor(state.category) }]} />
        <Text style={styles.columnTitle}>{state.name}</Text>
        <Text style={styles.columnCount}>{issues.length}</Text>
      </View>

      <FlatList
        data={issues}
        scrollEnabled={!stacked}
        keyExtractor={(i) => i.id}
        renderItem={({ item }) => (
          <Pressable
            onPress={() => onOpen(item)}
            // A long press offers the other columns. Drag-and-drop across a
            // horizontally scrolling board is a gesture that fights the scroll,
            // and this works with a screen reader.
            onLongPress={() =>
              Alert.alert(
                item.key,
                'Move to',
                states
                  .filter((s) => s.id !== state.id)
                  .map((s) => ({ text: s.name, onPress: () => onMove(item, s.id) }))
                  .concat([{ text: 'Cancel', onPress: () => {} }]),
              )
            }
            style={({ pressed }) => [styles.card, pressed && styles.cardPressed]}
            accessibilityRole="button"
            accessibilityLabel={`${item.key} ${item.title}`}
            accessibilityHint="Opens the issue. Long press to move it."
          >
            <Text style={styles.cardKey}>{item.key}</Text>
            <Text style={styles.cardTitle} numberOfLines={3}>
              {item.title}
            </Text>
            {item.priority > 0 && (
              <Text style={styles.cardPriority}>
                {PRIORITIES.find((p) => p.value === item.priority)?.label}
              </Text>
            )}
          </Pressable>
        )}
        ListEmptyComponent={<Text style={styles.columnEmpty}>Nothing here</Text>}
      />

      <Pressable onPress={onAdd} style={styles.addButton} accessibilityRole="button">
        <Text style={styles.addButtonText}>+ Add issue</Text>
      </Pressable>
    </View>
  )
}

function promptFor(title: string, suggestion: string, onSubmit: (value: string) => void) {
  const alert = Alert as unknown as {
    prompt?: (t: string, m?: string, cb?: (s: string) => void, type?: string, def?: string) => void
  }
  if (typeof alert.prompt === 'function') {
    alert.prompt(title, undefined, (text) => onSubmit(text?.trim() || suggestion), 'plain-text', suggestion)
    return
  }
  // Android and web have no Alert.prompt. Creating with the suggested name beats
  // a button that does nothing on two of three platforms.
  onSubmit(suggestion)
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  projectBar: { paddingVertical: space.sm },
  projectChip: {
    minHeight: MIN_TOUCH,
    justifyContent: 'center',
    paddingHorizontal: space.md,
    borderRadius: 999,
    backgroundColor: theme.surfaceAlt,
    marginRight: space.sm,
  },
  projectChipActive: { backgroundColor: theme.primary },
  projectChipText: { color: theme.body, fontWeight: '600', fontSize: 13 },
  projectChipTextActive: { color: theme.primaryText, fontWeight: '700', fontSize: 13 },

  column: { paddingHorizontal: space.sm, paddingBottom: space.md, minWidth: 260 },
  columnHeader: { flexDirection: 'row', alignItems: 'center', gap: space.xs, paddingVertical: space.sm },
  dot: { width: 8, height: 8, borderRadius: 4 },
  columnTitle: { color: theme.text, fontWeight: '700', fontSize: 14, flex: 1 },
  columnCount: { color: theme.textFaint, fontSize: 12 },
  columnEmpty: { color: theme.textDim, fontSize: 12, paddingVertical: space.sm },

  card: {
    backgroundColor: theme.surface,
    borderRadius: 8,
    padding: space.sm,
    marginBottom: space.sm,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: theme.border,
  },
  cardPressed: { opacity: 0.7 },
  cardKey: { color: theme.textFaint, fontSize: 11, fontWeight: '600' },
  cardTitle: { color: theme.text, fontSize: 14, marginTop: 2 },
  cardPriority: { color: theme.warning, fontSize: 11, marginTop: 4 },

  addButton: { minHeight: MIN_TOUCH, justifyContent: 'center' },
  addButtonText: { color: theme.textMuted, fontSize: 13 },
})
