package drive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Sharing. Two surfaces over one mechanism.
//
// /shares is a thin wrapper over authz.Grant and authz.Revoke, with two guards
// the checker does not have and should not: you may not grant above your own
// capability, and the grantee must be a member of the object's workspace.
//
// /links mints a row whose id is an acl_grant SUBJECT. Resolving one returns a
// session whose key set is exactly that subject's key — never the workspace
// member key — so a link widens exactly one object and can never widen tenancy.

const (
	// tokenBytes is the entropy in a share-link token. 32 bytes is 256 bits;
	// the token is the whole credential, so it is sized as a credential rather
	// than as an identifier.
	tokenBytes = 32

	// linkSessionTTL bounds a resolved link session. Short, because the session
	// is minted from a bearer token that may have been forwarded further than
	// its creator intended.
	linkSessionTTL = 30 * time.Minute
)

var (
	// ErrLinkInvalid covers every reason a token does not resolve — unknown,
	// revoked, expired, used up. ONE error, deliberately: distinguishing them
	// tells somebody walking the token space which guesses were close.
	ErrLinkInvalid = errors.New("drive: this link is not valid")

	// ErrLinkPassword means the link resolves but the password is wrong or
	// missing. Distinct from ErrLinkInvalid because the holder of a valid link
	// needs to be told to enter a password.
	ErrLinkPassword = errors.New("drive: this link requires a password")

	// ErrEscalation is a grant above the granter's own capability.
	ErrEscalation = errors.New("drive: you cannot grant more than you hold")

	// ErrNotAMember is a grant to somebody outside the object's workspace.
	ErrNotAMember = errors.New("drive: the person you are sharing with is not in this workspace")
)

// Share is one entry in the sharing panel.
type Share struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Capability  string `json:"capability"`
	GrantedBy   string `json:"granted_by,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// Link is a share link as the panel sees it. The token is NOT here: it is
// returned once, by the route that creates it, and never again.
type Link struct {
	ID          string     `json:"id"`
	ObjectType  string     `json:"object_type"`
	ObjectID    string     `json:"object_id"`
	Capability  string     `json:"capability"`
	HasPassword bool       `json:"has_password"`
	ExpiresAt   *time.Time `json:"expires_at"`
	MaxUses     *int       `json:"max_uses"`
	UseCount    int        `json:"use_count"`
	CreatedBy   *string    `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (h *Handler) shareTarget(w http.ResponseWriter, r *http.Request, want authz.Capability) (authz.ObjectRef, authz.Capability, bool) {
	objectType := httputil.PathParam(r, "object_type")
	id, err := httputil.RequirePathParam(r, "object_id")
	if err != nil {
		httputil.HandleError(w, err)
		return authz.ObjectRef{}, authz.CapNone, false
	}
	// THE PRODUCT'S ONLY SHARING SURFACE, so it must cover every object anybody
	// needs to share — not only the two that happen to live in Drive.
	//
	// A mailbox was unreachable here, and CreateMailbox writes exactly one grant
	// (CapAdmin to its creator). So the "shared inbox" had one reader for life,
	// and because mailboxes.created_by is ON DELETE SET NULL while the grant's
	// subject is that user, offboarding the creator left the mailbox — and every
	// customer conversation and attachment in it — reachable by nobody and
	// repairable only by hand-written SQL.
	//
	// A conversation is here too: a thread inherits from its mailbox, so
	// granting on one is "bring somebody into this one customer's thread"
	// without giving them the whole inbox.
	var ref authz.ObjectRef
	switch objectType {
	case EntryFolder:
		ref = authz.FolderObject(id)
	case EntryFile:
		ref = authz.FileObject(id)
	case EntryMailbox, EntryConversation:
		ref = authz.ObjectRef{Type: objectType, ID: id}
	default:
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			`object_type must be "folder", "file", "mailbox" or "conversation"`)
		return authz.ObjectRef{}, authz.CapNone, false
	}
	got, ok := h.authorize(w, r, ref, want)
	if !ok {
		return authz.ObjectRef{}, authz.CapNone, false
	}
	return ref, got, true
}

// ListShares returns who can reach an object.
func (h *Handler) ListShares(w http.ResponseWriter, r *http.Request) {
	// Share, not read: who else can reach a thing is not something every reader
	// is entitled to know.
	ref, _, ok := h.shareTarget(w, r, authz.CapShare)
	if !ok {
		return
	}
	grants, err := h.authz.SubjectsOf(r.Context(), ref)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]Share, 0, len(grants))
	for _, g := range grants {
		out = append(out, Share{
			SubjectType: g.Subject.Type,
			SubjectID:   g.Subject.ID,
			Capability:  g.Capability.String(),
			GrantedBy:   g.GrantedBy,
			CreatedAt:   g.CreatedAt,
		})
	}
	httputil.JSON(w, http.StatusOK, out)
}

// PutShare grants a capability to a person.
func (h *Handler) PutShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	var req struct {
		SubjectID  string `json:"subject_id"`
		Capability string `json:"capability"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability, ok := authz.ParseCapability(req.Capability)
	if !ok {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"capability must be one of read, comment, write, share, admin")
		return
	}

	ref, actorCap, ok := h.shareTarget(w, r, authz.CapShare)
	if !ok {
		return
	}

	// THE ESCALATION GUARD, and it lives here rather than in authz because
	// authz.Grant is also how Drive creation gives the workspace admin on a new
	// root — a check inside grantIn would refuse that. Nothing in the checker
	// compares the requested capability to the granter's.
	if !actorCap.Implies(capability) {
		fail(w, fmt.Errorf("%w: you hold %s", ErrEscalation, actorCap))
		return
	}

	member, err := h.authz.IsWorkspaceMember(ctx, refWorkspace(ctx, h, ref), req.SubjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if !member {
		// Sharing OUTSIDE the workspace is what links are for. A user grant to a
		// stranger would put them in an object graph they have no other route
		// into, and every listing would then have to explain why they can see
		// one folder and nothing around it.
		fail(w, ErrNotAMember)
		return
	}

	if err := h.authz.Grant(ctx, authz.UserSubject(actor),
		authz.UserSubject(req.SubjectID), ref, capability); err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, refWorkspace(ctx, h, ref), "drive.shared", ref.Type, ref.ID,
		map[string]interface{}{"subject": req.SubjectID, "capability": capability.String()})

	httputil.JSON(w, http.StatusOK, Share{
		SubjectType: authz.SubjectUser,
		SubjectID:   req.SubjectID,
		Capability:  capability.String(),
		GrantedBy:   actor,
		CreatedAt:   time.Now().Unix(),
	})
}

// DeleteShare withdraws a grant.
func (h *Handler) DeleteShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref, _, ok := h.shareTarget(w, r, authz.CapShare)
	if !ok {
		return
	}
	subjectType := httputil.PathParam(r, "subject_type")
	subjectID := httputil.PathParam(r, "subject_id")

	var subject authz.SubjectRef
	switch subjectType {
	case authz.SubjectUser:
		subject = authz.UserSubject(subjectID)
	case authz.SubjectGroup:
		subject = authz.GroupSubject(subjectID)
	default:
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"subject_type must be user or group; revoke a link through /drive/links/{id}")
		return
	}

	if err := h.authz.Revoke(ctx, authz.UserSubject(authctx.UserID(ctx)), subject, ref); err != nil {
		fail(w, err)
		return
	}

	// POST-COMMIT, and only for a user subject. authz's own live-revocation
	// fan-out covers channels and collab documents and deliberately not files —
	// see collab.Service.RevokeFileAccess for why widening it is the wrong fix.
	// Without this, somebody removed from a document keeps typing in it for up
	// to ws.membershipRecheckPeriod, which is five minutes on the pillar whose
	// entire premise is live collaboration.
	//
	// A group subject is skipped for the same reason liveRevoker skips it: a
	// group holds no connections, and its members' sessions go when their own
	// grants do.
	if subject.Type == authz.SubjectUser {
		h.revokeLiveEditors(ctx, subject.ID, ref)
	}

	h.record(ctx, refWorkspace(ctx, h, ref), "drive.unshared", ref.Type, ref.ID,
		map[string]interface{}{"subject": subjectID})
	w.WriteHeader(http.StatusNoContent)
}

// revokeLiveEditors drops the editor sessions a withdrawn grant covered.
//
// Best effort by design: the grant is already gone, every subsequent HTTP
// request re-authorizes, and failing a completed revocation because the hub was
// unreachable would leave the panel showing a share that no longer exists. What
// it costs when it fails is latency — the membership recheck still closes the
// session — so a warning is the right severity and a 500 is not.
func (h *Handler) revokeLiveEditors(ctx context.Context, userID string, ref authz.ObjectRef) {
	if h.revoker == nil {
		return
	}
	// The PATH, not the id: revoking a share on a folder must cut every editor
	// session beneath it, and inheritance is a materialized path.
	var path string
	if err := h.repo.pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = $1 AND object_id = $2`,
		ref.Type, ref.ID).Scan(&path); err != nil || path == "" {
		h.logger().Warn("could not resolve the ACL path to revoke live editor sessions",
			"object", ref.String(), "error", err)
		return
	}
	n, truncated := h.revoker.RevokeFileAccess(ctx, userID, path)
	if truncated {
		// Loud, because the remainder falls back to the five-minute recheck and
		// nothing else will ever say so.
		h.logger().Warn("live editor revocation truncated; the remainder falls back to the "+
			"periodic membership recheck", "object", ref.String(), "revoked", n)
	}
}

// refWorkspace resolves an object's workspace for the audit entry. Best effort:
// losing the attribution must not fail a share that succeeded.
func refWorkspace(ctx context.Context, h *Handler, ref authz.ObjectRef) string {
	var ws string
	_ = h.repo.pool.QueryRow(ctx,
		`SELECT workspace_id::text FROM acl_object WHERE object_type = $1 AND object_id = $2`,
		ref.Type, ref.ID).Scan(&ws)
	return ws
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// CreateLink mints a share link and returns the token ONCE.
func (h *Handler) CreateLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	var req struct {
		Capability string     `json:"capability"`
		Password   string     `json:"password"`
		ExpiresAt  *time.Time `json:"expires_at"`
		MaxUses    *int       `json:"max_uses"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability := authz.CapRead
	if req.Capability != "" {
		parsed, ok := authz.ParseCapability(req.Capability)
		if !ok || (parsed != authz.CapRead && parsed != authz.CapComment) {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
				"a link may grant read or comment; anonymous write is not available")
			return
		}
		capability = parsed
	}

	// LINKS ARE FILE AND FOLDER ONLY, and this is a product decision rather
	// than a limitation to route around.
	//
	// shareTarget is shared by all five sharing routes, so widening it for
	// per-user mailbox grants silently widened this one too — and
	// drive_share_links still CHECKs object_type IN ('file','folder'), so the
	// insert failed with a 500 rather than a refusal. Widening the CHECK would
	// be worse: a link is ANONYMOUS access, and a link over a mailbox is an
	// unauthenticated read of an entire customer inbox handed out by anyone who
	// can share it. RevokeLink also maps everything that is not a file to a
	// FOLDER object, so such a link could not even be revoked through the API.
	if t := httputil.PathParam(r, "object_type"); t != EntryFolder && t != EntryFile {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			`a share link may be created for a folder or a file only; grant a person or a group instead`)
		return
	}

	ref, _, ok := h.shareTarget(w, r, authz.CapShare)
	if !ok {
		return
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	passwordHash := ""
	if req.Password != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		passwordHash = string(hashed)
	}

	workspaceID := refWorkspace(ctx, h, ref)

	var link Link
	if err := database.WithTx(ctx, h.repo.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO drive_share_links
			    (workspace_id, object_type, object_id, token_hash, capability,
			     password_hash, expires_at, max_uses, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id::text, object_type, object_id::text, capability,
			          password_hash <> '', expires_at, max_uses, use_count,
			          created_by::text, created_at`,
			workspaceID, ref.Type, ref.ID, sum[:], capability.String(),
			passwordHash, req.ExpiresAt, req.MaxUses, nullable(actor)).
			Scan(&link.ID, &link.ObjectType, &link.ObjectID, &link.Capability,
				&link.HasPassword, &link.ExpiresAt, &link.MaxUses, &link.UseCount,
				&link.CreatedBy, &link.CreatedAt); err != nil {
			return fmt.Errorf("create share link: %w", err)
		}
		// The link's AUTHORIZATION is an ordinary grant, so SubjectsOf shows it
		// in the same panel and GrantsFor answers with it. The link row carries
		// only what a grant has no column for: expiry, password, use cap.
		return h.authz.GrantTx(ctx, tx, authz.UserSubject(actor),
			authz.LinkSubject(link.ID), ref, capability)
	}); err != nil {
		fail(w, err)
		return
	}

	h.record(ctx, workspaceID, "drive.link_created", ref.Type, ref.ID,
		map[string]interface{}{"link_id": link.ID, "capability": capability.String(),
			"has_password": link.HasPassword})

	httputil.JSON(w, http.StatusCreated, map[string]interface{}{
		"link": link,
		// ONCE. It is not stored, so it cannot be shown again — which is the
		// point: a table an operator can read is a table an operator can read
		// every share link out of.
		"token": token,
		"note":  "this token is shown once and is not recoverable; anyone holding it can read the object",
	})
}

// ListLinks returns the live links on an object.
func (h *Handler) ListLinks(w http.ResponseWriter, r *http.Request) {
	ref, _, ok := h.shareTarget(w, r, authz.CapShare)
	if !ok {
		return
	}
	rows, err := h.repo.pool.Query(r.Context(), `
		SELECT id::text, object_type, object_id::text, capability,
		       password_hash <> '', expires_at, max_uses, use_count,
		       created_by::text, created_at
		  FROM drive_share_links
		 WHERE object_type = $1 AND object_id = $2 AND revoked_at IS NULL
		 ORDER BY created_at DESC`, ref.Type, ref.ID)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()

	out := []Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.ObjectType, &l.ObjectID, &l.Capability,
			&l.HasPassword, &l.ExpiresAt, &l.MaxUses, &l.UseCount,
			&l.CreatedBy, &l.CreatedAt); err != nil {
			fail(w, err)
			return
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, out)
}

// RevokeLink kills a link.
func (h *Handler) RevokeLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	linkID, err := httputil.RequirePathParam(r, "link_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	var (
		objectType, objectID, workspaceID string
	)
	if err := h.repo.pool.QueryRow(ctx,
		`SELECT object_type, object_id::text, workspace_id::text
		   FROM drive_share_links WHERE id = $1`, linkID).
		Scan(&objectType, &objectID, &workspaceID); err != nil {
		fail(w, ErrNotFound)
		return
	}
	// EXPLICIT, not "file or else folder". The fallback silently authorized a
	// link on any other object type against folder:<thatUUID> — an acl_object
	// row that does not exist, so CapNone, so a 404 the holder could never
	// resolve. CreateLink now refuses those types outright; this refuses a row
	// that predates that or arrived some other way, rather than guessing.
	var ref authz.ObjectRef
	switch objectType {
	case EntryFolder:
		ref = authz.FolderObject(objectID)
	case EntryFile:
		ref = authz.FileObject(objectID)
	default:
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"that link is for an object type this route cannot authorize")
		return
	}
	if _, ok := h.authorize(w, r, ref, authz.CapShare); !ok {
		return
	}

	if err := database.WithTx(ctx, h.repo.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE drive_share_links SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`,
			linkID); err != nil {
			return fmt.Errorf("revoke share link: %w", err)
		}
		// The grant goes too. Leaving it would mean a revoked link still
		// materializes its key on every object in the subtree — revocation that
		// only changed a flag the resolve path reads, and every other path does
		// not.
		return h.authz.RevokeTx(ctx, tx, authz.LinkSubject(linkID), ref)
	}); err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, workspaceID, "drive.link_revoked", objectType, objectID,
		map[string]interface{}{"link_id": linkID})
	w.WriteHeader(http.StatusNoContent)
}

// ResolveLink exchanges a token for a link-scoped session.
//
// UNAUTHENTICATED, and POST rather than GET: a token in a GET lands in a
// Referer, in a browser history and in a prefetch. It is rate limited by IP
// under a FIXED bucket — see ratelimit.MiddlewareByIPBucket, because the default
// buckets on the request path and this path CONTAINS the token, which would give
// every guess its own budget.
func (h *Handler) ResolveLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := httputil.PathParam(r, "token")
	if token == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "missing token")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	// A body is optional: a link without a password resolves with none.
	_ = httputil.DecodeJSON(r, &req)

	sum := sha256.Sum256([]byte(token))

	var (
		linkID, objectType, objectID, workspaceID, capability, passwordHash string
		expiresAt                                                           *time.Time
		maxUses                                                             *int
		useCount                                                            int
		revokedAt                                                           *time.Time
	)
	err := h.repo.pool.QueryRow(ctx, `
		SELECT id::text, object_type, object_id::text, workspace_id::text, capability,
		       password_hash, expires_at, max_uses, use_count, revoked_at
		  FROM drive_share_links WHERE token_hash = $1`, sum[:]).
		Scan(&linkID, &objectType, &objectID, &workspaceID, &capability,
			&passwordHash, &expiresAt, &maxUses, &useCount, &revokedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		fail(w, ErrLinkInvalid)
		return
	case err != nil:
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// ONE answer for every reason a link does not work. Distinguishing "expired"
	// from "unknown" tells somebody walking the token space which guesses landed
	// on a real row.
	switch {
	case revokedAt != nil,
		expiresAt != nil && expiresAt.Before(time.Now()),
		maxUses != nil && useCount >= *maxUses:
		fail(w, ErrLinkInvalid)
		return
	}

	if passwordHash != "" {
		if req.Password == "" || bcrypt.CompareHashAndPassword(
			[]byte(passwordHash), []byte(req.Password)) != nil {
			fail(w, ErrLinkPassword)
			return
		}
	}

	// Counted after every check passes, so a wrong password does not burn a use.
	if _, err := h.repo.pool.Exec(ctx,
		`UPDATE drive_share_links SET use_count = use_count + 1 WHERE id = $1`, linkID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.record(ctx, workspaceID, "drive.link_resolved", objectType, objectID,
		map[string]interface{}{"link_id": linkID})

	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"link_id":     linkID,
		"object_type": objectType,
		"object_id":   objectID,
		"capability":  capability,
		// The key set this session holds, and it is EXACTLY one key. Never the
		// workspace member key: acl_object.workspace_id stays authoritative for
		// tenancy, and a link that carried 'w-' would read the whole workspace.
		"access_keys": []string{authz.LinkSubject(linkID).Key()},
		"expires_in":  int(linkSessionTTL.Seconds()),
	})
}
