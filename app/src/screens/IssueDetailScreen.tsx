import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, View, Text, Pressable, ScrollView, Alert, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { errorMessage } from '../api/client'
import { issueApi, categoryColor, PRIORITIES } from '../api/issues'
import type { Issue, IssueState } from '../api/issues'
import { useAuthStore } from '../stores/authStore'
import { useUserStore } from '../stores/userStore'
import CommentThread from '../components/CommentThread'
import { space, MIN_TOUCH } from '../lib/responsive'
import { ContentColumn, ErrorState, LoadingState, ScreenHeader, Section } from './internal/ui'

/**
 * One issue.
 *
 * The comment thread is the SHARED component, given an object type and an id —
 * exactly what a Drive file or a document would give it. Nothing here is a
 * comment feature.
 */
export default function IssueDetailScreen({ navigation, route }: { navigation: any; route: any }) {
  const issueId: string = route.params?.issueId
  const currentUserId = useAuthStore((s) => s.user?.id ?? '')
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [issue, setIssue] = useState<Issue | null>(null)
  const [states, setStates] = useState<IssueState[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      const res = await issueApi.issue(issueId)
      setIssue(res.data)
      const s = await issueApi.states(res.data.project_id)
      setStates(s.data)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setLoading(false)
    }
  }, [issueId])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    if (issue?.assignee_id) ensureUsers([issue.assignee_id])
  }, [issue?.assignee_id, ensureUsers])

  const setState = useCallback(
    async (stateId: string) => {
      if (!issue) return
      try {
        // No anchor: this is a state change, not a reorder, so the issue keeps
        // its position. Sending one would send it to the top of the new column.
        const res = await issueApi.move(issue.id, { state_id: stateId })
        setIssue(res.data)
      } catch (e) {
        Alert.alert('Could not change the state', errorMessage(e))
      }
    },
    [issue],
  )

  const setPriority = useCallback(
    async (priority: number) => {
      if (!issue) return
      try {
        const res = await issueApi.update(issue.id, { priority })
        setIssue(res.data)
      } catch (e) {
        Alert.alert('Could not change the priority', errorMessage(e))
      }
    },
    [issue],
  )

  if (loading) {
    return (
      <SafeAreaView style={styles.screen}>
        <LoadingState label="Opening" />
      </SafeAreaView>
    )
  }
  if (error || !issue) {
    return (
      <SafeAreaView style={styles.screen}>
        <ScreenHeader title="Issue" onBack={() => navigation.goBack()} />
        <ErrorState message={error ?? 'Not found'} onRetry={load} />
      </SafeAreaView>
    )
  }

  const current = states.find((s) => s.id === issue.state_id)

  return (
    <SafeAreaView style={styles.screen}>
      <ScreenHeader title={issue.key} subtitle={issue.title} onBack={() => navigation.goBack()} />
      <ScrollView>
        <ContentColumn>
          <Text style={styles.title}>{issue.title}</Text>
          {issue.description !== '' && <Text style={styles.description}>{issue.description}</Text>}

          <Section title="State">
            <View style={styles.chips}>
              {states.map((s) => (
                <Pressable
                  key={s.id}
                  onPress={() => setState(s.id)}
                  style={[styles.chip, s.id === issue.state_id && styles.chipActive]}
                  accessibilityRole="button"
                  accessibilityState={{ selected: s.id === issue.state_id }}
                >
                  <View style={[styles.dot, { backgroundColor: s.color || categoryColor(s.category) }]} />
                  <Text style={s.id === issue.state_id ? styles.chipTextActive : styles.chipText}>
                    {s.name}
                  </Text>
                </Pressable>
              ))}
            </View>
            {/* completed_at is a fact reports read, so it is shown rather than
                inferred from the state's name — which is per-project. */}
            {issue.completed_at && (
              <Text style={styles.meta}>
                Completed {new Date(issue.completed_at).toLocaleString()}
              </Text>
            )}
            {current && <Text style={styles.meta}>Category: {current.category}</Text>}
          </Section>

          <Section title="Priority">
            <View style={styles.chips}>
              {PRIORITIES.map((p) => (
                <Pressable
                  key={p.value}
                  onPress={() => setPriority(p.value)}
                  style={[styles.chip, p.value === issue.priority && styles.chipActive]}
                  accessibilityRole="button"
                  accessibilityState={{ selected: p.value === issue.priority }}
                >
                  <Text style={p.value === issue.priority ? styles.chipTextActive : styles.chipText}>
                    {p.label}
                  </Text>
                </Pressable>
              ))}
            </View>
          </Section>

          <Section title="Details">
            <Detail
              label="Assignee"
              value={
                issue.assignee_id
                  ? users[issue.assignee_id]?.full_name || users[issue.assignee_id]?.username || 'someone'
                  : 'Unassigned'
              }
            />
            <Detail label="Created" value={new Date(issue.created_at).toLocaleString()} />
            <Detail label="Updated" value={new Date(issue.updated_at).toLocaleString()} />
          </Section>

          {/* THE SHARED SURFACE. It is given an object type and an id, which is
              exactly what a Drive file gives it. */}
          <CommentThread
            objectType="issue"
            objectId={issue.id}
            canComment
            currentUserId={currentUserId}
          />
        </ContentColumn>
      </ScrollView>
    </SafeAreaView>
  )
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.detail}>
      <Text style={styles.detailLabel}>{label}</Text>
      <Text style={styles.detailValue}>{value}</Text>
    </View>
  )
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: theme.bg },
  title: { color: theme.text, fontSize: 20, fontWeight: '700', paddingTop: space.md },
  description: { color: theme.body, fontSize: 15, paddingTop: space.sm },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: space.xs },
  chip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    minHeight: MIN_TOUCH,
    paddingHorizontal: space.sm,
    borderRadius: 999,
    backgroundColor: theme.surfaceAlt,
  },
  chipActive: { backgroundColor: theme.primary },
  chipText: { color: theme.body, fontSize: 13 },
  chipTextActive: { color: theme.primaryText, fontSize: 13, fontWeight: '600' },
  dot: { width: 8, height: 8, borderRadius: 4 },
  meta: { color: theme.textFaint, fontSize: 12, paddingTop: space.xs },
  detail: { flexDirection: 'row', justifyContent: 'space-between', paddingVertical: 6 },
  detailLabel: { color: theme.textMuted, fontSize: 13 },
  detailValue: { color: theme.body, fontSize: 13 },
})
