import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  View,
  Text,
  TextInput,
  Pressable,
  FlatList,
  SafeAreaView,
  ScrollView,
  ActivityIndicator,
  Alert,
} from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import { isDriveHit, searchApi, type SearchHit } from '../api/search'
import { channelApi } from '../api/channels'
import { userApi } from '../api/users'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { useUserStore, displayName } from '../stores/userStore'
import { errorMessage } from '../api/client'
import type { PublicUser } from '../lib/types'
import { useResponsive } from '../lib/responsive'
import { Button, Chip, EmptyState, ScreenHeader, contentColumn } from './internal/ui'

/** The backend caps at 100; step up rather than paginate — search has no cursor. */
/** What a Drive hit calls itself in the list. */
function hitKindLabel(hit: SearchHit): string {
  switch (hit.type) {
    case 'document':
      return 'Document'
    case 'spreadsheet':
      return 'Sheet'
    case 'design':
      return 'Design'
    case 'issue':
      return 'Issue'
    case 'file':
      return 'File'
    default:
      return 'Message'
  }
}

const LIMIT_STEPS = [20, 50, 100]

function hitTime(unixSeconds: number): string {
  if (!unixSeconds) return ''
  const d = new Date(unixSeconds * 1000)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString()
}

export default function SearchScreen({ navigation }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channels = useChannelStore((s) => s.channels)
  const users = useUserStore((s) => s.users)

  const me = useAuthStore((s) => s.user)
  const { tier, gutter, minTouch } = useResponsive()
  /**
   * `channel` and `from` have always been in the API and were only ever a tap
   * away behind a toggle, because a phone has no room to keep them on screen.
   * A desktop does: the filters become a rail that stays visible next to the
   * results, so the toggle (and the guessing about what is currently applied)
   * is what the extra width removes.
   */
  const filterRail = tier !== 'compact'

  const [query, setQuery] = useState('')
  const [hits, setHits] = useState<SearchHit[]>([])
  const [total, setTotal] = useState(0)
  const [limit, setLimit] = useState(LIMIT_STEPS[0])
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)
  const [opening, setOpening] = useState<string | null>(null)

  // Filters
  const [showFilters, setShowFilters] = useState(false)
  const [channelFilter, setChannelFilter] = useState<string | null>(null)
  const [fromUser, setFromUser] = useState<PublicUser | null>(null)
  const [peopleQuery, setPeopleQuery] = useState('')
  const [people, setPeople] = useState<PublicUser[]>([])
  const peopleTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const searchableChannels = useMemo(
    () => channels.filter((c) => !c.is_archived),
    [channels],
  )

  useEffect(() => {
    if (hits.length > 0) useUserStore.getState().ensureUsers(hits.map((h) => h.user_id))
  }, [hits])

  useEffect(() => {
    if (peopleTimer.current) clearTimeout(peopleTimer.current)
    const q = peopleQuery.trim()
    if (!q) {
      setPeople([])
      return
    }
    peopleTimer.current = setTimeout(() => {
      userApi
        .search(q)
        .then((res) => setPeople(res.data ?? []))
        .catch(() => setPeople([]))
    }, 300)
    return () => {
      if (peopleTimer.current) clearTimeout(peopleTimer.current)
    }
  }, [peopleQuery])

  const runSearch = useCallback(
    async (nextLimit = limit) => {
      const q = query.trim()
      if (!q || !activeWorkspace) return
      setLoading(true)
      setSearched(true)
      try {
        // The filters the API has always supported were never sent, and no
        // `limit` meant the server's default of 20 with no way past it.
        const res = await searchApi.messages(activeWorkspace.id, q, {
          channel: channelFilter ?? undefined,
          from: fromUser?.id,
          limit: nextLimit,
        })
        setHits(res.data?.hits ?? [])
        setTotal(res.data?.estimated_total ?? 0)
        setLimit(nextLimit)
      } catch (err) {
        Alert.alert('Error', errorMessage(err, 'Search failed'))
        setHits([])
        setTotal(0)
      } finally {
        setLoading(false)
      }
    },
    [query, activeWorkspace, channelFilter, fromUser?.id, limit],
  )

  const channelName = (channelId: string) => {
    const ch = channels.find((c) => c.id === channelId)
    if (!ch) return 'other channel'
    return ch.name || (ch.type === 'dm' || ch.type === 'group_dm' ? 'direct message' : ch.slug || 'channel')
  }

  /**
   * Search is authorised server-side against every channel the caller can
   * *read*, which includes public channels they have not joined. Opening one
   * used to dead-end in an alert; now it offers to join first.
   */
  const openHit = async (hit: SearchHit) => {
    // A DRIVE OBJECT IS NOT A MESSAGE. The server returns six hit types and the
    // client asked for none, so documents and files arrived here and were sent
    // to channelApi.get with an empty channel id — a request that cannot
    // resolve, ending in "That channel is no longer available."
    if (isDriveHit(hit)) {
      navigation.navigate('DriveFile', { fileId: hit.id })
      return
    }
    const known = channels.find((c) => c.id === hit.channel_id)
    if (known) {
      useChannelStore.getState().setActiveChannel(known)
      navigation.navigate('Workspace', {})
      return
    }
    if (!activeWorkspace) return
    setOpening(hit.id)
    try {
      const res = await channelApi.get(activeWorkspace.id, hit.channel_id)
      const ch = res.data
      const label = ch.name || ch.slug || 'this channel'
      Alert.alert('Join channel?', `This message is in #${label}, which you have not joined.`, [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Join & open',
          onPress: async () => {
            try {
              await channelApi.join(activeWorkspace.id, ch.id)
              useChannelStore.getState().addChannel(ch)
              useChannelStore.getState().setActiveChannel(ch)
              navigation.navigate('Workspace', {})
            } catch (err) {
              Alert.alert('Error', errorMessage(err, 'Could not join that channel'))
            }
          },
        },
      ])
    } catch (err) {
      Alert.alert('Unavailable', errorMessage(err, 'That channel is no longer available.'))
    } finally {
      setOpening(null)
    }
  }

  const activeFilterCount = (channelFilter ? 1 : 0) + (fromUser ? 1 : 0)
  const nextLimit = LIMIT_STEPS.find((l) => l > limit)
  const canShowMore = !!nextLimit && hits.length >= limit && total > hits.length

  const renderHit = ({ item }: { item: SearchHit }) => (
    <Pressable
      onPress={() => openHit(item)}
      disabled={opening === item.id}
      accessibilityRole="button"
      accessibilityLabel={
        isDriveHit(item)
          ? `${hitKindLabel(item)} ${item.title || 'Untitled'}: ${item.content}`
          : `Message from ${displayName(users, item.user_id)} in ${channelName(item.channel_id)}: ${item.content}`
      }
      accessibilityHint={isDriveHit(item) ? 'Opens the file' : 'Opens the channel'}
      style={{
        flexDirection: 'row',
        gap: 10,
        paddingHorizontal: gutter,
        paddingVertical: tier === 'compact' ? 12 : 9,
        minHeight: minTouch,
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
        opacity: opening === item.id ? 0.5 : 1,
      }}
    >
      <View
        style={{
          width: 36,
          height: 36,
          borderRadius: 10,
          backgroundColor: avatarColor(item.user_id),
          alignItems: 'center',
          justifyContent: 'center',
        }}
      >
        <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>
          {isDriveHit(item)
            ? hitKindLabel(item).slice(0, 2).toUpperCase()
            : displayName(users, item.user_id).slice(0, 2).toUpperCase()}
        </Text>
      </View>
      <View style={{ flex: 1 }}>
        <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8, marginBottom: 2 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }} numberOfLines={1}>
            {isDriveHit(item)
              ? item.title || 'Untitled'
              : displayName(users, item.user_id)}
          </Text>
          <Text style={{ color: theme.textMuted, fontSize: 12, flex: 1 }} numberOfLines={1}>
            {isDriveHit(item) ? hitKindLabel(item) : `#${channelName(item.channel_id)}`}
          </Text>
          <Text style={{ color: theme.textMuted, fontSize: 11 }}>{hitTime(item.created_at)}</Text>
        </View>
        <Text style={{ color: theme.body, fontSize: 15, lineHeight: 21 }} numberOfLines={3}>
          {item.content}
        </Text>
      </View>
    </Pressable>
  )

  const channelChips = [
    <Chip key="all" label="All" selected={!channelFilter} onPress={() => setChannelFilter(null)} />,
    ...searchableChannels.map((c) => (
      <Chip
        key={c.id}
        label={c.name || 'dm'}
        selected={channelFilter === c.id}
        onPress={() => setChannelFilter(channelFilter === c.id ? null : c.id)}
        accessibilityLabel={`Only in ${c.name || 'this direct message'}`}
      />
    )),
  ]

  const peopleChips = people.map((p) => (
    <Chip
      key={p.id}
      label={p.full_name || p.username}
      selected={fromUser?.id === p.id}
      onPress={() => {
        setFromUser(p)
        setPeopleQuery('')
        setPeople([])
      }}
    />
  ))

  /**
   * `rail` chips wrap down a narrow column; on a phone the same chips scroll
   * sideways, because a wrapped list of every channel would push the results
   * off the screen.
   */
  const chipGroup = (children: React.ReactNode, rail: boolean, paddingTop = 0) =>
    rail ? (
      <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 8, paddingTop }}>{children}</View>
    ) : (
      <ScrollView
        horizontal
        showsHorizontalScrollIndicator={false}
        contentContainerStyle={{ gap: 8, paddingTop, paddingBottom: 4 }}
      >
        {children}
      </ScrollView>
    )

  const filters = (rail: boolean) => (
    <View>
      <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 6 }}>Channel</Text>
      {chipGroup(channelChips, rail)}

      <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 12, marginBottom: 6 }}>From</Text>
      <View style={{ flexDirection: 'row', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <Chip label="Anyone" selected={!fromUser} onPress={() => setFromUser(null)} />
        {me ? (
          <Chip
            label="Me"
            selected={fromUser?.id === me.id}
            onPress={() =>
              setFromUser(
                fromUser?.id === me.id
                  ? null
                  : {
                      id: me.id,
                      username: me.username,
                      full_name: me.full_name,
                      avatar_url: me.avatar_url,
                      status_text: me.status_text,
                      status_emoji: me.status_emoji,
                      is_bot: me.is_bot,
                    },
              )
            }
          />
        ) : null}
        {fromUser && fromUser.id !== me?.id ? (
          <Chip label={fromUser.full_name || fromUser.username} selected onPress={() => setFromUser(null)} />
        ) : null}
      </View>

      <TextInput
        value={peopleQuery}
        onChangeText={setPeopleQuery}
        placeholder="Find a person…"
        placeholderTextColor={theme.textMuted}
        autoCapitalize="none"
        accessibilityLabel="Filter by author"
        style={{
          marginTop: 8,
          backgroundColor: theme.surface,
          borderWidth: 1,
          borderColor: theme.borderStrong,
          borderRadius: 10,
          paddingHorizontal: 12,
          paddingVertical: rail ? 8 : 10,
          minHeight: minTouch,
          color: theme.text,
          fontSize: 14,
        }}
      />
      {people.length > 0 ? chipGroup(peopleChips, rail, 8) : null}

      <View style={{ marginTop: 12 }}>
        <Button label="Apply filters" onPress={() => runSearch(LIMIT_STEPS[0])} />
      </View>
    </View>
  )

  const searchField = (
    <TextInput
      value={query}
      onChangeText={setQuery}
      onSubmitEditing={() => runSearch(LIMIT_STEPS[0])}
      returnKeyType="search"
      autoFocus
      placeholder="Search messages..."
      placeholderTextColor={theme.textMuted}
      accessibilityLabel="Search messages"
      style={{
        backgroundColor: theme.surface,
        borderWidth: 1,
        borderColor: theme.borderStrong,
        borderRadius: 12,
        paddingHorizontal: 14,
        paddingVertical: filterRail ? 9 : 12,
        minHeight: minTouch,
        color: theme.text,
        fontSize: 15,
      }}
    />
  )

  const resultCount =
    searched && !loading ? (
      <Text
        accessibilityRole="text"
        style={{ color: theme.textMuted, fontSize: 12, paddingHorizontal: gutter, paddingVertical: 8 }}
      >
        Showing {hits.length} of about {total} {total === 1 ? 'result' : 'results'}
      </Text>
    ) : null

  const results = loading ? (
    <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center' }}>
      <ActivityIndicator color={theme.accent} accessibilityLabel="Searching" />
    </View>
  ) : (
    <FlatList
      data={hits}
      keyExtractor={(item) => item.id}
      renderItem={renderHit}
      keyboardShouldPersistTaps="handled"
      // A hit is a quoted message: prose, so it stops at the reading measure.
      contentContainerStyle={contentColumn()}
      ListEmptyComponent={
        <EmptyState
          title={searched ? 'No messages found' : 'Search your workspace'}
          body={
            searched
              ? 'Try fewer words, or clear the channel and author filters.'
              : 'Results only include channels you can read.'
          }
        />
      }
      ListFooterComponent={
        canShowMore ? (
          <View style={{ padding: 16, alignItems: filterRail ? 'center' : 'stretch' }}>
            <View style={{ minWidth: filterRail ? 220 : undefined }}>
              <Button
                label={`Show more (up to ${nextLimit})`}
                onPress={() => runSearch(nextLimit as number)}
                variant="ghost"
              />
            </View>
          </View>
        ) : null
      }
    />
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title="Search"
        onBack={() => navigation.goBack()}
        right={
          // The rail already shows what is applied, so the toggle is redundant.
          filterRail ? undefined : (
            <Pressable
              onPress={() => setShowFilters((v) => !v)}
              accessibilityRole="button"
              accessibilityLabel={activeFilterCount ? `Filters, ${activeFilterCount} active` : 'Filters'}
              accessibilityState={{ expanded: showFilters }}
              hitSlop={8}
              style={{ minHeight: minTouch, minWidth: minTouch, alignItems: 'center', justifyContent: 'center' }}
            >
              <Text style={{ color: theme.accent, fontSize: 14 }}>
                Filters{activeFilterCount ? ` (${activeFilterCount})` : ''}
              </Text>
            </Pressable>
          )
        }
      />

      {filterRail ? (
        <View style={{ flex: 1, flexDirection: 'row' }}>
          <View style={{ width: 268, borderRightWidth: 1, borderRightColor: theme.border }}>
            <ScrollView contentContainerStyle={{ padding: gutter }} keyboardShouldPersistTaps="handled">
              <Text
                accessibilityRole="header"
                style={{
                  color: theme.textMuted,
                  fontSize: 11,
                  fontWeight: '700',
                  letterSpacing: 1,
                  marginBottom: 12,
                }}
              >
                FILTERS
              </Text>
              {filters(true)}
            </ScrollView>
          </View>

          <View style={{ flex: 1 }}>
            <View style={{ ...contentColumn(), paddingHorizontal: gutter, paddingTop: 12 }}>{searchField}</View>
            <View style={contentColumn()}>{resultCount}</View>
            {results}
          </View>
        </View>
      ) : (
        <>
          <View style={{ paddingHorizontal: gutter, paddingTop: 12 }}>{searchField}</View>
          {showFilters ? (
            <View style={{ paddingHorizontal: gutter, paddingTop: 12 }}>{filters(false)}</View>
          ) : null}
          {resultCount}
          {results}
        </>
      )}
    </SafeAreaView>
  )
}
