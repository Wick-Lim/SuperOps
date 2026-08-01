/** Only the roster fetched for the currently selected channel may label it. */
export function visibleDMRoster(
  currentChannelId: string,
  rosterChannelId: string,
  rosterIds: string[],
): string[] {
  return currentChannelId === rosterChannelId ? rosterIds : []
}
