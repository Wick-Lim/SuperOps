import { channelApi } from '../../api/channels'
import { AccountCache } from '../../lib/accountCache'

const rosters = new AccountCache<string, readonly string[]>()

export function getCachedDMRoster(channelId: string): readonly string[] | undefined {
  return rosters.get(channelId)
}

export async function loadDMRoster(
  workspaceId: string,
  channelId: string,
): Promise<readonly string[] | undefined> {
  const cached = rosters.get(channelId)
  if (cached) return cached
  const generation = rosters.generation
  const res = await channelApi.listMembers(workspaceId, channelId)
  const ids = (res.data ?? []).map((member) => member.user_id)
  return rosters.setIfCurrent(generation, channelId, ids) ? ids : undefined
}

export function clearDMRosterCache(): void {
  rosters.clear()
}
