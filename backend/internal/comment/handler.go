package comment

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Notifier is the part of internal/inbox this package uses.
//
// One method, and it is deliberately the SHAPE of both paths rather than either
// one: the API publishes to NATS and the worker's inbox-fanout durable consumes,
// while an in-process producer calls Deliver directly. docs/plans/README.md
// ruling 1 makes inbox.Deliver the only writer of the coalesced counter, so a
// package holding the concrete notifier could reach past it.
//
// A nil Notifier is a working no-op, so a deployment or a test without one needs
// no branch at the call site.
type Notifier interface {
	Deliver(ctx context.Context, req inbox.Request) error
}

// NATSNotifier is the API-side implementation: it publishes
// superops.{ws}.inbox.requested and the worker fans it out. A new pillar adds
// zero durables and zero lines in cmd/worker — which is ruling 1's whole point.
type NATSNotifier struct {
	nats *natspkg.Client
}

func NewNATSNotifier(nc *natspkg.Client) Notifier {
	if nc == nil {
		return nil
	}
	return &NATSNotifier{nats: nc}
}

func (n *NATSNotifier) Deliver(ctx context.Context, req inbox.Request) error {
	// Not the request context: the response is about to be written and
	// cancelling the publish with it would drop the notification for a client
	// that hung up a millisecond early.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return inbox.Publish(pubCtx, n.nats, req)
}

// Titler turns a comment target into something a notification can say.
//
// It exists because this package must not know what an issue or a file is, and
// "somebody commented on 5d3f…" is a notification nobody can act on. The
// composition root supplies it; an unknown type falls back to the object type,
// which is worse than a title and much better than a uuid.
type Titler interface {
	Title(ctx context.Context, obj authz.ObjectRef) (string, error)
}

type Handler struct {
	repo     *Repository
	authz    *authz.Checker
	notifier Notifier
	titles   Titler
	audit    *audit.Service
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, notifier Notifier, titles Titler, auditor *audit.Service) *Handler {
	return &Handler{
		repo:     NewRepository(pool, az),
		authz:    az,
		notifier: notifier,
		titles:   titles,
		audit:    auditor,
	}
}

// RegisterRoutes mounts the comment surface under any commentable object.
//
// ONE route shape for every type. `{object_type}` is validated against
// acl_object rather than against a list here, so a pillar that introduces a new
// commentable object gets comments by registering an acl_object — not by
// editing this file.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/comments/{object_type}/{object_id}", authMw(http.HandlerFunc(h.List)))
	mux.Handle("POST /api/v1/comments/{object_type}/{object_id}", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("GET /api/v1/comments/{comment_id}/replies", authMw(http.HandlerFunc(h.Replies)))
	mux.Handle("PATCH /api/v1/comments/{comment_id}", authMw(http.HandlerFunc(h.Update)))
	mux.Handle("DELETE /api/v1/comments/{comment_id}", authMw(http.HandlerFunc(h.Delete)))
}

// target resolves the object a request is about and authorizes against IT.
//
// The comment surface has no permission model of its own. CapComment is the rung
// the ladder already carries for exactly this: it sits between read and write so
// somebody can discuss a thing without being able to change it.
func (h *Handler) target(w http.ResponseWriter, r *http.Request, want authz.Capability) (authz.ObjectRef, authz.Capability, bool) {
	objectType := httputil.PathParam(r, "object_type")
	objectID, err := httputil.RequirePathParam(r, "object_id")
	if err != nil {
		httputil.HandleError(w, err)
		return authz.ObjectRef{}, authz.CapNone, false
	}
	ref := authz.ObjectRef{Type: objectType, ID: objectID}

	got, err := h.authz.Capability(r.Context(), authz.UserSubject(authctx.UserID(r.Context())), ref)
	if err != nil {
		// A malformed type — one that fails acl_object's shape rule — is a client
		// bug, and saying so is more useful than a 404 the client cannot act on.
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"object_type must be a lowercase alphanumeric name")
		return authz.ObjectRef{}, authz.CapNone, false
	}
	switch {
	case !got.Implies(authz.CapRead):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
		return authz.ObjectRef{}, authz.CapNone, false
	case !got.Implies(want):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN",
			"you do not have permission to comment on this")
		return authz.ObjectRef{}, authz.CapNone, false
	}
	return ref, got, true
}

// forComment resolves the comment AND its target's capability in one step, so
// every mutation authorizes against the thing the comment is about.
func (h *Handler) forComment(w http.ResponseWriter, r *http.Request, want authz.Capability) (*Comment, authz.Capability, bool) {
	id, err := httputil.RequirePathParam(r, "comment_id")
	if err != nil {
		httputil.HandleError(w, err)
		return nil, authz.CapNone, false
	}
	c, err := h.repo.Get(r.Context(), id)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, authz.CapNone, false
	}
	if c == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "comment not found")
		return nil, authz.CapNone, false
	}

	got, err := h.authz.Capability(r.Context(),
		authz.UserSubject(authctx.UserID(r.Context())),
		authz.ObjectRef{Type: c.ObjectType, ID: c.ObjectID})
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, authz.CapNone, false
	}
	if !got.Implies(want) {
		// 404 rather than 403 when they cannot even read the target: a 403 on a
		// comment about something you cannot see confirms it exists.
		status, code := http.StatusForbidden, "FORBIDDEN"
		if !got.Implies(authz.CapRead) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		}
		httputil.JSONError(w, status, code, "you do not have permission to do that")
		return nil, authz.CapNone, false
	}
	return c, got, true
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	ref, _, ok := h.target(w, r, authz.CapRead)
	if !ok {
		return
	}
	comments, cursor, hasMore, err := h.repo.Thread(r.Context(), ref, page)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSONList(w, http.StatusOK, comments, cursor, hasMore)
}

func (h *Handler) Replies(w http.ResponseWriter, r *http.Request) {
	c, _, ok := h.forComment(w, r, authz.CapRead)
	if !ok {
		return
	}
	replies, err := h.repo.Replies(r.Context(), c.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, replies)
}

type writeRequest struct {
	Body     string `json:"body"`
	ParentID string `json:"parent_id"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	var req writeRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	// CapComment, not CapWrite. The rung exists so a reviewer can discuss
	// something they may not edit, which is most of what commenting is for.
	ref, _, ok := h.target(w, r, authz.CapComment)
	if !ok {
		return
	}

	created, mentions, err := h.repo.Create(ctx, ref, actor, req.ParentID, req.Body)
	if err != nil {
		fail(w, err)
		return
	}

	h.record(ctx, created, "comment.created")
	h.notify(ctx, created, mentions.UserIDs, nil)

	httputil.JSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	var req writeRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	existing, capability, ok := h.forComment(w, r, authz.CapComment)
	if !ok {
		return
	}

	before := ExtractMentions(existing.Body)
	updated, mentions, err := h.repo.Update(ctx, existing.ID, actor, req.Body,
		capability.Implies(authz.CapAdmin))
	if err != nil {
		fail(w, err)
		return
	}

	h.record(ctx, updated, "comment.edited")
	// Only the NEWLY mentioned. Re-notifying everybody on every typo fix is how
	// a mention stops meaning anything.
	h.notify(ctx, updated, mentions.UserIDs, before)

	httputil.JSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	existing, capability, ok := h.forComment(w, r, authz.CapComment)
	if !ok {
		return
	}
	if err := h.repo.Delete(ctx, existing.ID, authctx.UserID(ctx),
		capability.Implies(authz.CapAdmin)); err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, existing, "comment.deleted")
	w.WriteHeader(http.StatusNoContent)
}

// notify fans a comment out to the people it names.
//
// docs/plans/README.md ruling 1, applied literally:
//
//   - the recipient list is DIRECTED AND EXPLICIT. Only the people mentioned,
//     plus the parent's author on a reply. There is no watch, no subscribe and
//     no "everyone who can read it" — an undirected event is the
//     channel-unread-badge shape and creates the hot row plan 01 names.
//   - SUPPRESSION HAPPENS BEFORE Deliver. The author is removed here, not
//     inside the inbox: nobody is notified about their own comment.
//   - the kind is a TEXT string, no enum, no migration.
func (h *Handler) notify(ctx context.Context, c *Comment, mentioned []string, alreadyMentioned []string) {
	if h.notifier == nil || c == nil {
		return
	}
	actor := authctx.UserID(ctx)

	seen := map[string]bool{actor: true}
	for _, id := range alreadyMentioned {
		seen[id] = true
	}

	recipients := make([]string, 0, len(mentioned)+1)
	for _, id := range mentioned {
		if seen[id] {
			continue
		}
		seen[id] = true
		recipients = append(recipients, id)
	}

	// A reply notifies the parent's author, who did not have to mention
	// themselves to be part of the conversation they started.
	if c.ParentID != nil {
		if parent, err := h.repo.Get(ctx, *c.ParentID); err == nil && parent != nil &&
			parent.AuthorID != nil && !seen[*parent.AuthorID] {
			seen[*parent.AuthorID] = true
			recipients = append(recipients, *parent.AuthorID)
		}
	}
	if len(recipients) == 0 {
		return
	}

	title := c.ObjectType
	if h.titles != nil {
		if t, err := h.titles.Title(ctx, authz.ObjectRef{Type: c.ObjectType, ID: c.ObjectID}); err == nil && t != "" {
			title = t
		}
	}

	// Try, not fail: the comment is written and a lost notification must not
	// take a successful request with it.
	_ = h.notifier.Deliver(ctx, inbox.Request{
		WorkspaceID: c.WorkspaceID,
		Kind:        "comment.mentioned",
		ObjectType:  c.ObjectType,
		ObjectID:    c.ObjectID,
		// It coalesces under the OBJECT, not under the comment: five comments on
		// one issue is one badge that says five, not five badges.
		SubjectType: c.ObjectType,
		SubjectID:   c.ObjectID,
		ActorID:     actor,
		Title:       title,
		Body:        preview(c.Body),
		Data: map[string]string{
			"comment_id":  c.ID,
			"object_type": c.ObjectType,
			"object_id":   c.ObjectID,
		},
		Recipients: recipients,
	})
}

// preview trims a body to something a notification can carry. Rune-safe: a cut
// through a multi-byte character produces a replacement glyph in a push
// notification somebody actually reads.
func preview(body string) string {
	const max = 140
	runes := []rune(body)
	if len(runes) <= max {
		return body
	}
	return string(runes[:max]) + "…"
}

func (h *Handler) record(ctx context.Context, c *Comment, action string) {
	if h.audit == nil || c == nil {
		return
	}
	h.audit.Try(ctx, audit.Entry{
		WorkspaceID:  c.WorkspaceID,
		ActorID:      authctx.UserID(ctx),
		Action:       action,
		ResourceType: "comment",
		ResourceID:   c.ID,
		Metadata:     map[string]interface{}{"object_type": c.ObjectType, "object_id": c.ObjectID},
	})
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "comment not found")
	case errors.Is(err, ErrNotAuthor):
		httputil.JSONError(w, http.StatusForbidden, "NOT_AUTHOR", err.Error())
	case errors.Is(err, ErrDeleted):
		httputil.JSONError(w, http.StatusConflict, "COMMENT_DELETED", err.Error())
	case errors.Is(err, ErrNested):
		httputil.JSONError(w, http.StatusConflict, "COMMENT_NESTED", err.Error())
	case errors.Is(err, ErrEmpty):
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	default:
		httputil.HandleError(w, httputil.NewInternal(err))
	}
}
