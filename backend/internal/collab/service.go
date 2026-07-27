package collab

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
)

// Service is the collaboration layer proper: authorization, the update log and
// the fan-out, in the order they have to happen.
//
// It implements ws.RoomHandler, which is how a socket reaches it without the ws
// package knowing this one exists.
type Service struct {
	repo   *Repository
	authz  Authorizer
	hub    *ws.Hub
	logger *slog.Logger

	// compactionThreshold is how far the log may run ahead of the snapshot
	// before a client is asked to produce a new one.
	compactionThreshold int64

	mu        sync.Mutex
	asked     map[string]time.Time // document id -> when compaction was last requested
	askedTrim time.Time
}

// NewService wires the layer. hub may not be nil — every write is fanned out
// through it, and a Service that could not broadcast would persist edits nobody
// else ever sees, which is worse than failing to start.
func NewService(repo *Repository, az Authorizer, hub *ws.Hub, logger *slog.Logger) *Service {
	return &Service{
		repo:                repo,
		authz:               az,
		hub:                 hub,
		logger:              logger,
		compactionThreshold: DefaultCompactionThreshold,
		asked:               make(map[string]time.Time),
	}
}

// SetCompactionThreshold overrides how many uncompacted updates trigger a
// snapshot request. It exists for tests and for an operator who has measured
// their own documents; the default is the only value the product ships with.
func (s *Service) SetCompactionThreshold(n int64) {
	if n > 0 {
		s.compactionThreshold = n
	}
}

// ---------------------------------------------------------------------------
// ws.RoomHandler
// ---------------------------------------------------------------------------

// Join authorizes a client into a document's room.
func (s *Service) Join(ctx context.Context, documentID, userID string) (ws.RoomAccess, error) {
	return s.access(ctx, documentID, userID)
}

// Recheck re-answers the same question for a live session. It is the same query
// as Join by design: a second, cheaper approximation is how a revocation ends
// up being enforced differently from a join.
func (s *Service) Recheck(ctx context.Context, documentID, userID string) (ws.RoomAccess, error) {
	return s.access(ctx, documentID, userID)
}

func (s *Service) access(ctx context.Context, documentID, userID string) (ws.RoomAccess, error) {
	_, access, err := s.Authorize(ctx, documentID, userID, false)
	return access, err
}

// Authorize resolves a document and answers whether the caller may read it and
// write to it. Both transports go through this one function, so an HTTP append
// and a socket append can never disagree about who is allowed to do what.
//
// It returns the ws.ErrRoom* sentinels rather than a bool so the two callers
// map the same failure to the same thing — a 403 over HTTP and a FORBIDDEN
// frame on the socket — without either of them re-deriving the reason.
func (s *Service) Authorize(ctx context.Context, documentID, userID string, needWrite bool) (*Document, ws.RoomAccess, error) {
	doc, err := s.repo.Get(ctx, documentID)
	if err != nil {
		return nil, ws.RoomAccess{}, err
	}
	if doc == nil {
		return nil, ws.RoomAccess{}, ws.ErrRoomNotFound
	}

	read, err := s.authz.CanRead(ctx, doc, userID)
	if err != nil {
		return nil, ws.RoomAccess{}, err
	}
	if !read {
		return nil, ws.RoomAccess{}, ws.ErrRoomForbidden
	}

	write, err := s.authz.CanWrite(ctx, doc, userID)
	if err != nil {
		return nil, ws.RoomAccess{}, err
	}
	if needWrite && !write {
		return nil, ws.RoomAccess{}, ws.ErrRoomReadOnly
	}
	return doc, ws.RoomAccess{HeadSeq: doc.HeadSeq, CanWrite: write}, nil
}

// OpenDocument returns the collaborative document for a Drive object, creating
// it on first open.
//
// IT AUTHORIZES TWICE, against the workspace and against the object, and the
// second check is the one that matters. This used to say the object's ACL
// belonged to whatever handed out the resource id — but a resource id in a
// request body was not handed out by anything, and treating it as proof of
// access is the confused deputy in its plainest form.
//
// What it cost: workspace_id comes from the path and resource_id from the body,
// nothing related them, and the conflict key is (resource_type, resource_id)
// with resource_type also caller-supplied and validated only as a character
// class. So a caller in ANY tenant could name a file in another one, pick a
// type nobody had used, and take that pair — the row is created in the
// CLAIMANT'S workspace, and the owner opening their own file then gets
// `409 this object already has a collaboration document in another workspace`,
// permanently and for as many types as the claimant cares to spend requests on.
// Measured end to end: the claim answered 200 and the owner's next open 409.
//
// The repository already refused to RETURN a document owned by another
// workspace, for exactly this reason and in those words. That guard was
// one-sided: it stopped a caller reading an object someone else had claimed and
// left them free to claim an unclaimed one.
func (s *Service) OpenDocument(ctx context.Context, workspaceID, resourceType, resourceID, userID string) (*Document, error) {
	ok, err := s.authz.CanCreate(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ws.ErrRoomForbidden
	}
	// Deliberately not folded into the check above: CanCreate is a workspace
	// question and this is an object one, and a caller who passes the first and
	// fails the second is exactly the case this exists for.
	ok, err = s.authz.CanOpenResource(ctx, resourceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ws.ErrRoomForbidden
	}
	return s.repo.EnsureDocument(ctx, workspaceID, resourceType, resourceID, userID)
}

// Update is the socket's append path. Authorization was decided at join time
// and is carried on the connection; see the package documentation for why a
// keystroke does not re-query it.
func (s *Service) Update(ctx context.Context, documentID, userID, connID string, payload []byte) error {
	_, err := s.Append(ctx, documentID, userID, connID, payload)
	return err
}

// Awareness relays a cursor/selection state to the room and returns. Nothing on
// this path touches Postgres, which is the whole point of awareness being
// ephemeral — it is not a policy, it is the absence of a write.
func (s *Service) Awareness(_ context.Context, documentID, userID, connID string, payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxUpdateBytes {
		return ws.ErrRoomInvalidPayload
	}
	s.hub.BroadcastToRoom(documentID, ws.TypeCollabAwareness, map[string]interface{}{
		"document_id": documentID,
		"actor_id":    userID,
		"origin_conn": connID,
		"state":       payload,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Append and compaction
// ---------------------------------------------------------------------------

// Append persists one update and fans it out, returning the sequence number it
// was given. The HTTP path calls it directly; the socket goes through Update.
//
// The broadcast happens after the commit, so it is possible for seq 6 to reach
// a client before seq 5. That is safe for the document (CRDT updates are
// order-independent) but not for a watermark, which is why a client must only
// advance its watermark over a contiguous prefix.
func (s *Service) Append(ctx context.Context, documentID, userID, connID string, payload []byte) (int64, error) {
	if len(payload) == 0 {
		return 0, ws.ErrRoomInvalidPayload
	}
	if len(payload) > MaxUpdateBytes {
		return 0, ws.ErrRoomPayloadTooLarge
	}

	seq, snapshotSeq, err := s.repo.AppendUpdate(ctx, documentID, userID, payload)
	if err != nil {
		return 0, err
	}

	s.hub.BroadcastToRoom(documentID, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": documentID,
		"seq":         seq,
		"actor_id":    userID,
		"origin_conn": connID,
		"update":      payload,
	})

	s.maybeRequestCompaction(documentID, seq, snapshotSeq)
	return seq, nil
}

// SaveSnapshot stores a client-produced snapshot and compacts the log behind
// it. Losing the race to another compactor is ErrStaleSnapshot, not a failure.
func (s *Service) SaveSnapshot(ctx context.Context, documentID string, throughSeq int64, userID string, payload []byte) (int64, error) {
	if len(payload) == 0 {
		return 0, ws.ErrRoomInvalidPayload
	}
	if len(payload) > MaxSnapshotBytes {
		return 0, ws.ErrRoomPayloadTooLarge
	}

	compacted, err := s.repo.SaveSnapshot(ctx, documentID, throughSeq, userID, payload)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	delete(s.asked, documentID)
	s.mu.Unlock()

	s.logger.Info("collaboration log compacted",
		"document_id", documentID, "through_seq", throughSeq, "updates_removed", compacted)
	return compacted, nil
}

// Load reads a document's state from a watermark.
func (s *Service) Load(ctx context.Context, documentID string, since int64) (*State, error) {
	return s.repo.Load(ctx, documentID, since, maxStateUpdates)
}

// maybeRequestCompaction asks one client in the room for a snapshot once the
// log has run far enough ahead of the last one.
//
// The server cannot produce the snapshot itself — it never interprets an
// update — so this is the only way a long-lived document's log stops growing.
// A document with nobody in the room is not asked and does not need to be: its
// log is not growing either, because updates only arrive from clients that are
// connected.
func (s *Service) maybeRequestCompaction(documentID string, headSeq, snapshotSeq int64) {
	if headSeq-snapshotSeq < s.compactionThreshold {
		return
	}
	if !s.claimCompaction(documentID, time.Now()) {
		return
	}

	sent := s.hub.AskRoomLeader(documentID, ws.TypeCollabCompact, map[string]interface{}{
		"document_id":  documentID,
		"head_seq":     headSeq,
		"snapshot_seq": snapshotSeq,
	})
	if !sent {
		// Nobody local to ask. Another replica's leader may still answer; if
		// none does, the request is retried after the cooldown.
		s.logger.Debug("collaboration compaction requested with no local client",
			"document_id", documentID, "head_seq", headSeq)
	}
}

// claimCompaction rate-limits requests per document so a client that ignores
// them is not asked on every append. It also trims its own bookkeeping, which
// is otherwise a map that grows with every document the replica ever served.
func (s *Service) claimCompaction(documentID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if last, ok := s.asked[documentID]; ok && now.Sub(last) < compactionCooldown {
		return false
	}
	if now.Sub(s.askedTrim) >= compactionCooldown {
		for id, at := range s.asked {
			if now.Sub(at) >= compactionCooldown {
				delete(s.asked, id)
			}
		}
		s.askedTrim = now
	}
	s.asked[documentID] = now
	return true
}

// ---------------------------------------------------------------------------
// Revocation
// ---------------------------------------------------------------------------

// RevokeAccess cuts one user out of one document's room across every replica.
// Call it wherever access is withdrawn; the socket's periodic recheck is the
// backstop, not the mechanism, and it is five minutes wide.
func (s *Service) RevokeAccess(userID, documentID string) {
	s.hub.RevokeRoom(userID, documentID)
}

// RevokeDocument cuts everyone out, for a document that has been deleted.
func (s *Service) RevokeDocument(documentID string) {
	s.hub.RevokeRoomForAll(documentID)
}

// maxRevokedRooms bounds one fan-out. Revoking a share on a workspace's Drive
// root would otherwise enumerate every editor object in the tenant inside a
// request. Truncation is LOGGED by the caller and the five-minute membership
// recheck (ws.membershipRecheckPeriod) remains the backstop for whatever was
// past the limit — which is exactly the position every file was in before this
// existed.
const maxRevokedRooms = 512

// RevokeFileAccess cuts one user out of the rooms of every editor object at or
// beneath an ACL path, immediately and across replicas.
//
// It exists because authz's live-revocation fan-out deliberately does NOT cover
// files. `liveTypes` is {channel, doc} and the comment there is honest about
// why: "there is no such thing as an open file session; the next download
// re-authorizes". That was true until a file could be an open editor. Widening
// liveTypes is the wrong fix — liveDescendants filters on it, so a folder Move
// would enumerate every file beneath it against the same 512 cap and truncate
// on any real folder, taking the channel revocations down with it.
//
// So this is targeted: Drive calls it for the one object whose share changed,
// post-commit and best effort, and the collab layer resolves the rooms.
//
// Returns how many rooms were revoked and whether the list was truncated.
func (s *Service) RevokeFileAccess(ctx context.Context, userID, path string) (int, bool) {
	ids, err := s.repo.DocumentIDsUnderPath(ctx, path, maxRevokedRooms+1)
	if err != nil {
		s.logger.Error("resolve collaboration rooms for revocation", "path", path, "error", err)
		return 0, false
	}
	truncated := len(ids) > maxRevokedRooms
	if truncated {
		ids = ids[:maxRevokedRooms]
	}
	for _, id := range ids {
		if userID == "" {
			s.hub.RevokeRoomForAll(id)
		} else {
			s.hub.RevokeRoom(userID, id)
		}
	}
	return len(ids), truncated
}
