package mailbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// maxInboundBody bounds one delivery. Larger than any sane email and far
// smaller than what an unbounded reader would let a provider push through this
// process's memory.
const maxInboundBody = 2 << 20

type Handler struct {
	repo   *Repository
	pool   *pgxpool.Pool
	authz  *authz.Checker
	logger *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{repo: NewRepository(pool, az), pool: pool, authz: az, logger: logger}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/mailboxes", authMw(http.HandlerFunc(h.CreateMailbox)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/mailboxes", authMw(http.HandlerFunc(h.ListMailboxes)))
	mux.Handle("GET /api/v1/mailboxes/{mailbox_id}/conversations", authMw(http.HandlerFunc(h.ListConversations)))
	mux.Handle("GET /api/v1/conversations/{conversation_id}", authMw(http.HandlerFunc(h.GetConversation)))
	mux.Handle("PATCH /api/v1/conversations/{conversation_id}", authMw(http.HandlerFunc(h.PatchConversation)))
	mux.Handle("POST /api/v1/conversations/{conversation_id}/reply", authMw(http.HandlerFunc(h.Reply)))
}

// RegisterIngestRoutes is separate because ingest is authenticated by a BEARER
// TOKEN belonging to the deployment, not by a user session. The provider is a
// machine; giving it a user account would mean a user whose password is in a
// webhook configuration.
func (h *Handler) RegisterIngestRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/mail/inbound", mw(http.HandlerFunc(h.Inbound)))
}

// ---------------------------------------------------------------------------
// Mailboxes
// ---------------------------------------------------------------------------

func (h *Handler) CreateMailbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Creating an addressable destination for the whole company is an admin
	// action, not a member one.
	if got, err := h.authz.Capability(ctx, authz.UserSubject(actor), authz.WorkspaceObject(workspaceID)); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	} else if !got.Implies(authz.CapAdmin) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only a workspace admin may create a mailbox")
		return
	}

	var req struct {
		Address     string `json:"address"`
		DisplayName string `json:"display_name"`
		Prefix      string `json:"prefix"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}

	mb, err := h.repo.CreateMailbox(ctx, workspaceID, req.Address, req.DisplayName, req.Prefix, actor)
	switch {
	case errors.Is(err, ErrAddressTaken):
		httputil.JSONError(w, http.StatusConflict, "ADDRESS_TAKEN", "that address already exists")
		return
	case err != nil:
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_MAILBOX", err.Error())
		return
	}
	httputil.JSON(w, http.StatusCreated, mb)
}

func (h *Handler) ListMailboxes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	// The LIST path filters on materialized keys, once. Never a Capability call
	// per row — that is the N+1 the whole permission design exists to prevent.
	keys, err := h.authz.KeysFor(ctx, authz.UserSubject(actor), workspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT m.id::text, m.workspace_id::text, m.address, m.display_name, m.prefix,
		       m.archived_at, m.created_at, m.auto_reply_enabled
		  FROM mailboxes m
		 WHERE m.workspace_id = $1 AND m.archived_at IS NULL
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'mailbox' AND k.object_id = m.id
		                  AND k.key = ANY($2))
		 ORDER BY m.address`, workspaceID, keys)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]Mailbox, 0, 8)
	for rows.Next() {
		var mb Mailbox
		if err := rows.Scan(&mb.ID, &mb.WorkspaceID, &mb.Address, &mb.DisplayName, &mb.Prefix,
			&mb.ArchivedAt, &mb.CreatedAt, &mb.AutoReply); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, mb)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Conversations
// ---------------------------------------------------------------------------

// ListConversations authorizes ONCE, on the mailbox.
//
// That is not a shortcut, it is the model: a conversation's acl_object hangs
// off its mailbox, so a caller who may read the mailbox may read every
// conversation in it — there is no per-thread grant that could disagree.
//
// It is also what lets this ship without widening the access-key alphabet. A
// per-row `acl_key` filter would need an `m-<mailbox>` container prefix, and
// internal/search's validator is a CLOSED set: a key it rejects is DROPPED from
// the filter, and a dropped narrowing term widens the query. Adding a prefix
// belongs with plan 00 alongside the held `p-` decision, and this endpoint does
// not need it.
func (h *Handler) ListConversations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	mailboxID, err := httputil.RequirePathParam(r, "mailbox_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.mayRead(w, ctx, actor, authz.ObjectRef{Type: "mailbox", ID: mailboxID}) {
		return
	}

	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}
	state := r.URL.Query().Get("state")

	hasCur := !page.Cursor.IsZero()
	cursorID := page.Cursor.ID
	if !hasCur {
		cursorID = "00000000-0000-0000-0000-000000000000"
	}

	rows, err := h.pool.Query(ctx, `
		SELECT c.id::text, c.mailbox_id::text, c.workspace_id::text, c.number,
		       m.prefix || '-' || c.number::text, c.subject, c.state, c.priority,
		       c.assignee_id::text, c.customer_email, c.customer_name,
		       c.last_message_at, c.awaiting_reply, c.created_at
		  FROM mail_conversations c
		  JOIN mailboxes m ON m.id = c.mailbox_id
		 WHERE c.mailbox_id = $1
		   AND ($2 = '' OR c.state = $2)
		   AND (NOT $3::bool OR (c.last_message_at, c.id) < ($4::timestamptz, $5::uuid))
		 ORDER BY c.last_message_at DESC, c.id DESC
		 LIMIT $6`,
		mailboxID, state, hasCur, page.Cursor.CreatedAt, cursorID, page.Limit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]Conversation, 0, page.Limit)
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.MailboxID, &c.WorkspaceID, &c.Number, &c.Reference,
			&c.Subject, &c.State, &c.Priority, &c.AssigneeID, &c.CustomerEmail,
			&c.CustomerName, &c.LastMessageAt, &c.AwaitingReply, &c.CreatedAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var next string
	hasMore := page.Limit > 0 && len(out) == page.Limit
	if hasMore {
		last := out[len(out)-1]
		next = httputil.EncodeCursor(last.LastMessageAt, last.ID)
	}
	httputil.JSONList(w, http.StatusOK, out, next, hasMore)
}

func (h *Handler) GetConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	id, err := httputil.RequirePathParam(r, "conversation_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.mayRead(w, ctx, actor, authz.ObjectRef{Type: "conversation", ID: id}) {
		return
	}

	conv, err := h.conversation(ctx, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	msgs, err := h.messages(ctx, id)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"conversation": conv, "messages": msgs})
}

func (h *Handler) PatchConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	id, err := httputil.RequirePathParam(r, "conversation_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Changing state or assignment is a write, not a comment: it decides
	// whether anybody else picks the conversation up.
	if !h.may(w, ctx, actor, authz.ObjectRef{Type: "conversation", ID: id}, authz.CapWrite) {
		return
	}

	var req struct {
		State      *string `json:"state"`
		Priority   *string `json:"priority"`
		AssigneeID *string `json:"assignee_id"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if req.State != nil && !validState(*req.State) {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_STATE",
			"state must be one of open, pending, closed, spam")
		return
	}
	if req.Priority != nil && !validPriority(*req.Priority) {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_PRIORITY",
			"priority must be one of low, normal, high, urgent")
		return
	}

	// COALESCE keeps an omitted field untouched, so two agents changing
	// different fields do not overwrite each other's work.
	if _, err := h.pool.Exec(ctx, `
		UPDATE mail_conversations
		   SET state       = COALESCE($2, state),
		       priority    = COALESCE($3, priority),
		       assignee_id = CASE WHEN $4::text IS NULL THEN assignee_id
		                          WHEN $4 = '' THEN NULL
		                          ELSE $4::uuid END,
		       updated_at  = NOW()
		 WHERE id = $1`, id, req.State, req.Priority, req.AssigneeID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	conv, err := h.conversation(ctx, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, conv)
}

// Reply writes an outbound message into the thread.
//
// It stores the message and marks the conversation as answered; the actual
// delivery is internal/mail's job, reached through the existing outbound queue.
// Storing first is deliberate: an agent's reply must survive an SMTP outage,
// and a reply that was sent but not recorded is a conversation whose history
// lies.
func (h *Handler) Reply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	id, err := httputil.RequirePathParam(r, "conversation_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.may(w, ctx, actor, authz.ObjectRef{Type: "conversation", ID: id}, authz.CapWrite) {
		return
	}

	var req struct {
		BodyText string `json:"body_text"`
		BodyHTML string `json:"body_html"`
		Subject  string `json:"subject"`
	}
	if err := httputil.DecodeJSONLimit(r, &req, maxInboundBody); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if strings.TrimSpace(req.BodyText) == "" && strings.TrimSpace(req.BodyHTML) == "" {
		httputil.JSONError(w, http.StatusBadRequest, "EMPTY_REPLY", "a reply needs a body")
		return
	}

	var msg Message
	err = h.pool.QueryRow(ctx, `
		WITH parent AS (
			SELECT m.message_id, m.references_ids, c.subject, c.customer_email
			  FROM mail_conversations c
			  LEFT JOIN mail_messages m ON m.conversation_id = c.id
			 WHERE c.id = $1
			 ORDER BY m.created_at DESC NULLS LAST
			 LIMIT 1
		)
		INSERT INTO mail_messages
		       (conversation_id, direction, message_id, in_reply_to, references_ids,
		        from_address, subject, body_text, body_html, author_id, sent_at)
		SELECT $1, 'outbound', $2,
		       COALESCE(p.message_id, ''),
		       -- The reply's References is the parent's plus the parent itself,
		       -- which is what keeps the customer's mail client threading it.
		       CASE WHEN p.message_id IS NULL THEN '{}'::text[]
		            ELSE array_append(p.references_ids, p.message_id) END,
		       '',
		       COALESCE(NULLIF($3, ''), 'Re: ' || COALESCE(p.subject, '')),
		       $4, $5, $6, NOW()
		  FROM parent p
		RETURNING id::text, conversation_id::text, direction, message_id, in_reply_to,
		          subject, body_text, body_html, author_id::text, sent_at, created_at`,
		id, newMessageID(), req.Subject, req.BodyText, req.BodyHTML, actor,
	).Scan(&msg.ID, &msg.ConversationID, &msg.Direction, &msg.MessageID, &msg.InReplyTo,
		&msg.Subject, &msg.BodyText, &msg.BodyHTML, &msg.AuthorID, &msg.SentAt, &msg.CreatedAt)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// awaiting_reply goes false: the ball is with the customer now, and that is
	// the column every "needs attention" view reads.
	if _, err := h.pool.Exec(ctx, `
		UPDATE mail_conversations
		   SET last_message_at = NOW(), awaiting_reply = FALSE,
		       state = CASE WHEN state = 'open' THEN 'pending' ELSE state END,
		       updated_at = NOW()
		 WHERE id = $1`, id); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusCreated, msg)
}

// ---------------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------------

// Inbound receives one email from the mail provider.
//
// Authenticated by a bearer token matched against an indexed sha256 — never by
// a shared secret compared in plaintext, and never by source IP, which a
// provider changes without telling anybody.
func (h *Handler) Inbound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, ok := h.authenticateIngest(ctx, r.Header.Get("Authorization"))
	if !ok {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid ingest token")
		return
	}

	var req struct {
		EventID    string   `json:"event_id"`
		Recipient  string   `json:"recipient"`
		MessageID  string   `json:"message_id"`
		InReplyTo  string   `json:"in_reply_to"`
		References []string `json:"references"`
		From       string   `json:"from"`
		FromName   string   `json:"from_name"`
		To         []string `json:"to"`
		Cc         []string `json:"cc"`
		Subject    string   `json:"subject"`
		BodyText   string   `json:"body_text"`
		BodyHTML   string   `json:"body_html"`
		RawKey     string   `json:"raw_key"`
		ReceivedAt string   `json:"received_at"`
	}
	if err := httputil.DecodeJSONLimit(r, &req, maxInboundBody); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	received := time.Time{}
	if req.ReceivedAt != "" {
		if t, err := time.Parse(time.RFC3339, req.ReceivedAt); err == nil {
			received = t
		}
	}

	conv, msg, err := h.repo.Ingest(ctx, Inbound{
		ProviderEventID: req.EventID,
		Recipient:       req.Recipient,
		MessageID:       req.MessageID,
		InReplyTo:       req.InReplyTo,
		References:      req.References,
		FromAddress:     req.From,
		FromName:        req.FromName,
		To:              req.To,
		Cc:              req.Cc,
		Subject:         req.Subject,
		BodyText:        req.BodyText,
		BodyHTML:        req.BodyHTML,
		RawKey:          req.RawKey,
		ReceivedAt:      received,
	})
	switch {
	case errors.Is(err, ErrDuplicate):
		// 200, not an error. The provider retried and there is nothing wrong;
		// answering non-2xx would make it retry forever.
		httputil.JSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	case errors.Is(err, ErrNoMailbox):
		// 202: accepted and dropped. A 4xx makes a provider retry a message
		// that will never be deliverable, and a 5xx does the same and pages
		// somebody.
		httputil.JSON(w, http.StatusAccepted, map[string]any{"status": "no_mailbox"})
		return
	case err != nil:
		h.logger.Error("ingest inbound mail", "event_id", req.EventID, "error", err)
		httputil.JSONError(w, http.StatusInternalServerError, "INTERNAL", "could not file the message")
		return
	}

	// The token's workspace must own the mailbox. A token for tenant A that
	// could file into tenant B's mailbox would be a cross-tenant write with a
	// legitimate credential.
	if workspaceID != "" && conv.WorkspaceID != workspaceID {
		h.logger.Error("an ingest token filed into another workspace",
			"token_workspace", workspaceID, "conversation_workspace", conv.WorkspaceID)
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "that mailbox belongs to another workspace")
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]any{
		"status": "filed", "conversation": conv, "message_id": msg.ID,
	})
}

func (h *Handler) authenticateIngest(ctx context.Context, header string) (string, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(header, "Bearer "), "bearer "))
	if token == "" {
		return "", false
	}
	var workspaceID string
	err := h.pool.QueryRow(ctx,
		`SELECT workspace_id::text FROM mail_ingest_tokens
		  WHERE token_hash = $1 AND revoked_at IS NULL`, crypto.HashToken(token)).Scan(&workspaceID)
	if err != nil {
		return "", false
	}
	return workspaceID, true
}

// ---------------------------------------------------------------------------

func (h *Handler) may(w http.ResponseWriter, ctx context.Context, actor string,
	obj authz.ObjectRef, want authz.Capability) bool {
	got, err := h.authz.Capability(ctx, authz.UserSubject(actor), obj)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	switch {
	case !got.Implies(authz.CapRead):
		// 404, not 403: a 403 on something you cannot see confirms it exists.
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return false
	case !got.Implies(want):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient capability")
		return false
	}
	return true
}

func (h *Handler) mayRead(w http.ResponseWriter, ctx context.Context, actor string, obj authz.ObjectRef) bool {
	return h.may(w, ctx, actor, obj, authz.CapRead)
}

func (h *Handler) conversation(ctx context.Context, id string) (*Conversation, error) {
	var c Conversation
	err := h.pool.QueryRow(ctx, `
		SELECT c.id::text, c.mailbox_id::text, c.workspace_id::text, c.number,
		       m.prefix || '-' || c.number::text, c.subject, c.state, c.priority,
		       c.assignee_id::text, c.customer_email, c.customer_name,
		       c.last_message_at, c.awaiting_reply, c.created_at
		  FROM mail_conversations c JOIN mailboxes m ON m.id = c.mailbox_id
		 WHERE c.id = $1`, id,
	).Scan(&c.ID, &c.MailboxID, &c.WorkspaceID, &c.Number, &c.Reference, &c.Subject,
		&c.State, &c.Priority, &c.AssigneeID, &c.CustomerEmail, &c.CustomerName,
		&c.LastMessageAt, &c.AwaitingReply, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (h *Handler) messages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT id::text, conversation_id::text, direction, message_id, in_reply_to,
		       from_address, from_name, to_addresses, cc_addresses, subject,
		       body_text, body_html, author_id::text, sent_at, created_at
		  FROM mail_messages WHERE conversation_id = $1 ORDER BY created_at`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	out := make([]Message, 0, 16)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Direction, &m.MessageID, &m.InReplyTo,
			&m.FromAddress, &m.FromName, &m.ToAddresses, &m.CcAddresses, &m.Subject,
			&m.BodyText, &m.BodyHTML, &m.AuthorID, &m.SentAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	httputil.HandleError(w, httputil.NewInternal(err))
}

func validState(s string) bool {
	switch s {
	case StateOpen, StatePending, StateClosed, StateSpam:
		return true
	}
	return false
}

func validPriority(s string) bool {
	switch s {
	case "low", "normal", "high", "urgent":
		return true
	}
	return false
}

// newMessageID mints an RFC 5322 identifier for an outbound message. The
// customer's mail client threads on it, and this deployment must be able to
// recognise it coming back in a reply.
func newMessageID() string {
	// A failure here would mean no entropy source at all, at which point a
	// predictable id is the least of the problems — but a predictable one lets
	// somebody forge an In-Reply-To that files their email into a conversation
	// they cannot read, so it falls back to a uuid rather than to a counter.
	tok, err := crypto.GenerateRandomToken(24)
	if err != nil {
		tok = uuid.NewString()
	}
	return fmt.Sprintf("<%s@superops>", tok)
}
