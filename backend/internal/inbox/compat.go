package inbox

// Compatibility with the four shipped /api/v1/notifications routes.
//
// The React Native client ships on its own schedule (ROADMAP §3), so those
// routes keep their paths and their JSON shape and are re-pointed at inbox_items
// projected into {id, type, title, body, data, is_read, created_at}. They are
// deleted in the release after the client has moved.
//
// ONE thing about them changes meaning, deliberately and with eyes open:
//
//	GET /api/v1/notifications/unread-count
//
// used to count unread *events* and now counts unread *items*. Forty mentions in
// one channel used to report 40 and now report 1. This is not versioned, for two
// reasons. The badge is what the user compares against the list, and the list is
// now coalesced — a badge of 40 over a single row is the bug, not the fix. And
// the client (app/src/api/notifications.ts) reads the number into a badge and
// does nothing else with it, so there is no arithmetic anywhere that a smaller
// number breaks. The cost is real and is named here rather than discovered: an
// operator comparing dashboards across the deploy sees the number drop, and
// nothing in the response says why.

// legacyType maps an inbox kind onto one of migration 005's five enum values, so
// the shipped client's switch on `type` still matches.
//
// A kind from a pillar the client has never heard of becomes "system", which is
// its default arm. That is the entire compatibility story for new pillars on the
// old client: they arrive, they render generically, and they do not crash it.
func legacyType(kind string) string {
	switch kind {
	case KindMessageMention:
		return "mention"
	case KindMessageDM:
		return "dm"
	case KindMessageThreadReply:
		return "thread_reply"
	case KindChannelInvited:
		return "channel_invite"
	default:
		return "system"
	}
}

// legacyProjection renders an item in the old notification shape.
//
// `data` is a map rather than the old string-encoded JSON blob: the client
// parses it as an object either way, and the old shape was a `json.RawMessage`
// that had been stringified by the repository's `data::text` cast.
func legacyProjection(it *Item) map[string]any {
	return map[string]any{
		"id":      it.ID,
		"user_id": it.UserID,
		"type":    legacyType(it.TopKind),
		"title":   it.Title,
		"body":    it.Preview,
		"data": map[string]string{
			"subject_type": it.SubjectType,
			"subject_id":   it.SubjectID,
			// The shipped client deep-links on channel_id; every subject this
			// build produces is a channel, and a pillar whose subject is not one
			// simply has no deep link on the old client.
			"channel_id": channelIDOf(it),
		},
		"is_read":    it.State != StateUnread,
		"created_at": it.LastAt,
	}
}

func channelIDOf(it *Item) string {
	if it.SubjectType == SubjectChannel {
		return it.SubjectID
	}
	return ""
}
