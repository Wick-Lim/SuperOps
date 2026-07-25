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
// it on first open. Creating is authorized against the workspace, not the
// object: the object's own ACL check belongs to whatever hands out the
// resource id, and duplicating a guess at it here would be a second, weaker
// answer to the same question.
func (s *Service) OpenDocument(ctx context.Context, workspaceID, resourceType, resourceID, userID string) (*Document, error) {
	ok, err := s.authz.CanCreate(ctx, workspaceID, userID)
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

	sent := s.hub.SendToRoomLeader(documentID, ws.TypeCollabCompact, map[string]interface{}{
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
