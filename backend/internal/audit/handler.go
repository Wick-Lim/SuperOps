package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

// exportLimit bounds one export response. The route streams NDJSON so the
// response is not held in memory, but an unbounded export is still the
// highest-value single request in the product and a cap is what turns "one
// request, every row" into "one request, a page".
const exportLimit = 50000

// WorkspaceScoper resolves the workspaces a caller administers. It is
// admin.Handler.scope's contract, lifted to an interface so the query handler
// can move into the package that owns the table without importing the admin
// package (which imports this one).
type WorkspaceScoper interface {
	AdminWorkspaceIDs(ctx context.Context, userID string) ([]string, error)
}

// Handler serves the audit read surface.
//
// It lives here rather than in internal/admin — where the query used to be —
// because the package that owns the table owns the query. It is mounted at the
// SAME paths behind the SAME adminMw, so nothing about the authorization
// changes: workspace scoping is still AdminWorkspaceIDs, and the caller still
// sees only the workspaces they administer, which is the invariant
// TestAdminEndpointsAreWorkspaceScoped covers.
//
// There is deliberately NO route here that mutates audit_logs. There never has
// been one and there never must be: disabling auditing is startup configuration,
// so turning it off lands in the deploy trail instead of in the product.
type Handler struct {
	pool  *pgxpool.Pool
	svc   *Service
	scope WorkspaceScoper
	sink  Sink
}

func NewHandler(pool *pgxpool.Pool, svc *Service, scope WorkspaceScoper, sink Sink) *Handler {
	return &Handler{pool: pool, svc: svc, scope: scope, sink: sink}
}

// RegisterRoutes mounts the read surface behind the caller's admin middleware.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, adminMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/admin/audit-logs", adminMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/admin/audit-logs/verify", adminMw(http.HandlerFunc(h.Verify)))
}

// RegisterExportRoutes mounts the routes that need their own rate limit: the
// export (one request, a lot of rows) and the sink test (one request, a real
// outbound call). The same treatment internal/app gives the mail test.
func (h *Handler) RegisterExportRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/admin/audit-logs/export", mw(http.HandlerFunc(h.Export)))
	mux.Handle("POST /api/v1/admin/audit-sink/test", mw(http.HandlerFunc(h.TestSink)))
}

// filter is the shared query shape.
type filter struct {
	workspaces   []string
	actorID      string
	action       string
	resourceType string
	resourceID   string
	from, to     *time.Time
}

func (h *Handler) parseFilter(w http.ResponseWriter, r *http.Request) (filter, string, bool) {
	actor := authctx.UserID(r.Context())
	ids, err := h.scope.AdminWorkspaceIDs(r.Context(), actor)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return filter{}, "", false
	}
	if len(ids) == 0 {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return filter{}, "", false
	}

	f := filter{
		workspaces:   ids,
		actorID:      httputil.QueryParam(r, "actor_id"),
		action:       httputil.QueryParam(r, "action"),
		resourceType: httputil.QueryParam(r, "resource_type"),
		resourceID:   httputil.QueryParam(r, "resource_id"),
	}
	// A caller may narrow to one of THEIR workspaces; they may not widen.
	if ws := httputil.QueryParam(r, "workspace_id"); ws != "" {
		if !contains(ids, ws) {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not an administrator of that workspace")
			return filter{}, "", false
		}
		f.workspaces = []string{ws}
	}
	if f.actorID != "" {
		if _, err := uuid.Parse(f.actorID); err != nil {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "actor_id must be a uuid")
			return filter{}, "", false
		}
	}
	for _, spec := range []struct {
		name string
		dst  **time.Time
	}{{"from", &f.from}, {"to", &f.to}} {
		raw := httputil.QueryParam(r, spec.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", spec.name+" must be RFC3339")
			return filter{}, "", false
		}
		*spec.dst = &t
	}
	return f, actor, true
}

// build renders the filter into SQL. `action` is a PREFIX match ("user."),
// which the (workspace_id, action, created_at DESC) index serves as a range
// scan; a suffix or contains match would not be indexable at all.
func (f filter) build() (string, []any) {
	sql := `SELECT id, workspace_id, actor_id, action, resource_type, resource_id,
	               metadata::text, COALESCE(host(ip_address),''), created_at,
	               event_count, last_at, chain_seq
	          FROM audit_logs
	         WHERE workspace_id = ANY($1)`
	args := []any{f.workspaces}

	add := func(clause string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(clause, len(args))
	}
	if f.actorID != "" {
		add(" AND actor_id = $%d", f.actorID)
	}
	if f.action != "" {
		if strings.HasSuffix(f.action, ".") {
			add(" AND action LIKE $%d", f.action+"%")
		} else {
			add(" AND action = $%d", f.action)
		}
	}
	if f.resourceType != "" {
		add(" AND resource_type = $%d", f.resourceType)
	}
	if f.resourceID != "" {
		add(" AND resource_id = $%d", f.resourceID)
	}
	if f.from != nil {
		add(" AND created_at >= $%d", *f.from)
	}
	if f.to != nil {
		add(" AND created_at < $%d", *f.to)
	}
	return sql, args
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	f, actor, ok := h.parseFilter(w, r)
	if !ok {
		return
	}

	sql, args := f.build()
	if !params.Cursor.IsZero() {
		args = append(args, params.Cursor.CreatedAt, params.Cursor.ID)
		sql += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, params.Limit+1)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := h.pool.Query(r.Context(), sql, args...)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	entries, err := scanEntries(rows)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(entries) > params.Limit
	if hasMore {
		entries = entries[:params.Limit]
	}
	var cursor string
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		cursor = httputil.EncodeCursor(last.CreatedAt, last.ID)
	}

	// A read of the audit log is itself audited, WITH THE FILTER RECORDED. That
	// is the row that catches an administrator going looking, and it is the one
	// audit event whose absence would be most convenient for the person causing
	// it. Coalesced, because an admin paging through a week of logs is one
	// investigation, not forty events.
	h.svc.Buffer(r.Context(), Entry{
		WorkspaceID:  f.workspaces[0],
		ActorID:      actor,
		Action:       ActionAuditRead,
		ResourceType: "audit_log",
		IPAddress:    clientIP(r),
		Metadata:     f.describe(),
		Coalesce:     true,
	})

	httputil.JSONList(w, http.StatusOK, entries, cursor, hasMore)
}

// Export streams the same query as NDJSON.
//
// Streaming rather than buffering because the alternative is holding an
// arbitrary number of rows in memory to serve the single request most likely to
// ask for all of them. It is capped, rate limited by its own middleware, and
// audited synchronously BEFORE the first byte goes out — an export that fails
// halfway has still exported everything up to that point, so recording it
// afterwards would lose exactly the interesting case.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	f, actor, ok := h.parseFilter(w, r)
	if !ok {
		return
	}

	h.svc.Try(r.Context(), Entry{
		WorkspaceID:  f.workspaces[0],
		ActorID:      actor,
		Action:       ActionAuditExported,
		ResourceType: "audit_log",
		IPAddress:    clientIP(r),
		Metadata:     f.describe(),
	})

	sql, args := f.build()
	args = append(args, exportLimit)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := h.pool.Query(r.Context(), sql, args...)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-logs.ndjson"`)
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			// The status line is already sent, so this cannot become a 500. The
			// truncated stream and the log line are what the caller and the
			// operator get instead.
			logFromRequest(r).Error("audit export failed mid-stream", "error", err, "actor_id", actor)
			return
		}
		if err := enc.Encode(e); err != nil {
			return // client hung up
		}
	}
	if err := rows.Err(); err != nil {
		logFromRequest(r).Error("audit export failed mid-stream", "error", err, "actor_id", actor)
	}
}

// Verify reports each administered workspace's chain status. Read-only: it does
// NOT advance the anchor, so an auditor can ask the question without changing
// the answer. The audit_verify worker job is what anchors.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	f, _, ok := h.parseFilter(w, r)
	if !ok {
		return
	}

	statuses, err := NewVerifier(h.pool, h.sink, logFromRequest(r)).Verify(r.Context(), f.workspaces)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	sinkName := "none"
	if h.sink != nil {
		sinkName = h.sink.Name()
	}
	allOK := true
	for _, st := range statuses {
		if !st.OK {
			allOK = false
		}
	}
	// A broken chain is reported as data with a 200, not as a 500. A corrupted
	// audit log must not be a denial of service — that would make corrupting it
	// an attack rather than a thing an attack leaves behind.
	httputil.JSON(w, http.StatusOK, map[string]any{
		"ok":     allOK,
		"sink":   sinkName,
		"chains": statuses,
		"note": "chain_seq is only assigned to immutable, workspace-scoped entries; " +
			"coalesced rows (dedupe_key IS NOT NULL) are coalescable and therefore unchained. " +
			"Everything at or below anchored_seq has been shipped off-box by the configured sink; " +
			"everything above it is protected only by a chain stored in the same database.",
	})
}

// TestSink ships a REAL anchor through the configured transport and reports the
// transport's REAL error. Same shape as POST /api/v1/admin/mail/test, and for
// the same reason: a diagnostic that hides the reason is not a diagnostic.
func (h *Handler) TestSink(w http.ResponseWriter, r *http.Request) {
	_, actor, ok := h.parseFilter(w, r)
	if !ok {
		return
	}
	if h.sink == nil {
		httputil.JSONError(w, http.StatusServiceUnavailable, "AUDIT_SINK_DISABLED",
			"no audit sink is configured")
		return
	}

	// A synthetic workspace id, so a test anchor can never be mistaken for a
	// real one by whatever is on the other end.
	anchor := Anchor{WorkspaceID: uuid.Nil.String(), HeadSeq: 0, HeadHash: "", At: time.Now()}
	if err := h.sink.Ship(r.Context(), []Anchor{anchor}); err != nil {
		httputil.JSONError(w, http.StatusBadGateway, "AUDIT_SINK_UNAVAILABLE", err.Error())
		return
	}

	h.svc.Try(r.Context(), Entry{
		ActorID:      actor,
		Action:       ActionAuditSinkTest,
		ResourceType: "audit_sink",
		IPAddress:    clientIP(r),
		Metadata:     map[string]interface{}{"transport": h.sink.Name()},
	})
	httputil.JSON(w, http.StatusOK, map[string]any{"shipped": true, "transport": h.sink.Name()})
}

// describe renders the filter for the audit record. It is what makes
// `audit.read` useful: "who looked" without "what they looked for" is half a
// row.
func (f filter) describe() map[string]interface{} {
	m := map[string]interface{}{"workspaces": len(f.workspaces)}
	if f.actorID != "" {
		m["actor_id"] = f.actorID
	}
	if f.action != "" {
		m["action"] = f.action
	}
	if f.resourceType != "" {
		m["resource_type"] = f.resourceType
	}
	if f.resourceID != "" {
		m["resource_id"] = f.resourceID
	}
	if f.from != nil {
		m["from"] = f.from.UTC().Format(time.RFC3339)
	}
	if f.to != nil {
		m["to"] = f.to.UTC().Format(time.RFC3339)
	}
	return m
}

// LogEntry is one row as the API renders it.
type LogEntry struct {
	ID           string     `json:"id"`
	WorkspaceID  *string    `json:"workspace_id"`
	ActorID      *string    `json:"actor_id"`
	Action       string     `json:"action"`
	ResourceType string     `json:"resource_type"`
	ResourceID   *string    `json:"resource_id"`
	Metadata     string     `json:"metadata"`
	IPAddress    string     `json:"ip_address"`
	CreatedAt    time.Time  `json:"created_at"`
	EventCount   int        `json:"event_count"`
	LastAt       *time.Time `json:"last_at"`
	ChainSeq     *int64     `json:"chain_seq"`
}

func scanEntries(rows pgx.Rows) ([]LogEntry, error) {
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEntry(rows pgx.Rows) (LogEntry, error) {
	var e LogEntry
	if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.ActorID, &e.Action, &e.ResourceType,
		&e.ResourceID, &e.Metadata, &e.IPAddress, &e.CreatedAt,
		&e.EventCount, &e.LastAt, &e.ChainSeq); err != nil {
		return e, fmt.Errorf("scan audit log: %w", err)
	}
	return e, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// clientIP is the caller's address, without the port. nilIfNotIP stores
// anything unparseable as NULL, so this only has to be best effort — and it is
// deliberately NOT proxy-aware: internal/ratelimit's TrustProxy setting exists
// because a forwarded header is caller-controlled, and an audit row is the last
// place to record an address the caller chose for themselves.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func logFromRequest(r *http.Request) *slog.Logger { return logger.FromContext(r.Context()) }
