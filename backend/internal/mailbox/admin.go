package mailbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Administration: sending domains and ingest credentials.
//
// Both were missing entirely, and each absence disabled a half of the product:
//
//   - NO INGEST TOKEN COULD BE MINTED, so POST /mail/inbound was a permanent
//     401 on every real deployment. The table was only ever SELECTed; the
//     integration suite hand-inserted rows. Inbound mail could not be turned on
//     at all.
//   - NO DOMAIN COULD BE VERIFIED, so any workspace admin could claim any
//     address — support@yourcompetitor.example — with no proof of ownership,
//     and mailboxes.domain_id was never written. Migration 055's own header
//     says why that matters: sending as a domain you do not control is how a
//     deployment's IP gets burned.

// DNSResolver looks up TXT records. An interface so verification can be tested
// without a live zone, and so a deployment behind a split-horizon resolver can
// substitute its own.
type DNSResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type netResolver struct{}

func (netResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// matchesToken reports whether the zone carries our verification value.
//
// Extracted so it can be tested: it is the entire ownership check, and a
// matcher that accepted a prefix — or that treated a failed lookup as a match —
// would let anybody claim any domain. The zone almost always has other TXT
// records, so ours only has to be present among them.
func matchesToken(records []string, token string) bool {
	if token == "" {
		return false
	}
	for _, rec := range records {
		// Trimmed, because some registrars store the value with surrounding
		// quotes and some resolvers hand them back that way.
		if strings.Trim(strings.TrimSpace(rec), `"`) == token {
			return true
		}
	}
	return false
}

// verifyHost is where the operator publishes the token. A dedicated subdomain
// rather than the apex, so verification does not collide with SPF, DMARC or
// anything else already living in the zone's TXT records.
func verifyHost(domain string) string { return "_superops." + domain }

func (h *Handler) RegisterAdminRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/mail/domains", authMw(http.HandlerFunc(h.CreateDomain)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/mail/domains", authMw(http.HandlerFunc(h.ListDomains)))
	mux.Handle("POST /api/v1/mail/domains/{domain_id}/verify", authMw(http.HandlerFunc(h.VerifyDomain)))
	mux.Handle("DELETE /api/v1/mail/domains/{domain_id}", authMw(http.HandlerFunc(h.DeleteDomain)))

	mux.Handle("POST /api/v1/workspaces/{workspace_id}/mail/ingest-tokens", authMw(http.HandlerFunc(h.CreateIngestToken)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/mail/ingest-tokens", authMw(http.HandlerFunc(h.ListIngestTokens)))
	mux.Handle("DELETE /api/v1/mail/ingest-tokens/{token_id}", authMw(http.HandlerFunc(h.RevokeIngestToken)))
}

func (h *Handler) requireWorkspaceAdmin(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	got, err := h.authz.Capability(r.Context(), authz.UserSubject(authctx.UserID(r.Context())),
		authz.WorkspaceObject(workspaceID))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	if !got.Implies(authz.CapAdmin) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN",
			"only a workspace admin may administer mail")
		return false
	}
	return true
}

// Domain is a sending domain and its verification state.
type Domain struct {
	ID          string     `json:"id"`
	Domain      string     `json:"domain"`
	VerifiedAt  *time.Time `json:"verified_at"`
	CreatedAt   time.Time  `json:"created_at"`
	VerifyHost  string     `json:"verify_host"`
	VerifyValue string     `json:"verify_value"`
}

func (h *Handler) CreateDomain(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}

	var req struct {
		Domain string `json:"domain"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))

	// A random token, never derived from the domain: a derived one could be
	// computed by anybody for a domain they do not own, which would make the
	// whole check theatre.
	token, err := crypto.GenerateRandomToken(24)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var d Domain
	err = h.pool.QueryRow(r.Context(), `
		INSERT INTO mail_domains (workspace_id, domain, verify_token)
		VALUES ($1, $2, $3)
		RETURNING id::text, domain, verified_at, created_at, verify_token`,
		workspaceID, domain, token,
	).Scan(&d.ID, &d.Domain, &d.VerifiedAt, &d.CreatedAt, &d.VerifyValue)
	if err != nil {
		if strings.Contains(err.Error(), "mail_domains_domain_key") {
			// Globally unique. Two tenants claiming one domain is an
			// unresolvable routing question, better answered here than at the
			// first inbound message.
			httputil.JSONError(w, http.StatusConflict, "DOMAIN_TAKEN",
				"that domain is already registered")
			return
		}
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_DOMAIN", err.Error())
		return
	}
	d.VerifyHost = verifyHost(d.Domain)
	httputil.JSON(w, http.StatusCreated, d)
}

func (h *Handler) ListDomains(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT id::text, domain, verified_at, created_at, verify_token
		   FROM mail_domains WHERE workspace_id = $1 ORDER BY domain`, workspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]Domain, 0, 8)
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.Domain, &d.VerifiedAt, &d.CreatedAt, &d.VerifyValue); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		d.VerifyHost = verifyHost(d.Domain)
		out = append(out, d)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// VerifyDomain checks the DNS TXT record and, on success, marks the domain
// verified and adopts every mailbox already on it.
func (h *Handler) VerifyDomain(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "domain_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	var workspaceID, domain, token string
	var verifiedAt *time.Time
	err = h.pool.QueryRow(r.Context(),
		`SELECT workspace_id::text, domain, verify_token, verified_at
		   FROM mail_domains WHERE id = $1`, id,
	).Scan(&workspaceID, &domain, &token, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	records, err := h.dns.LookupTXT(ctx, verifyHost(domain))
	if err != nil {
		// A lookup failure is NOT a verification failure. NXDOMAIN before the
		// operator has published the record is the normal first attempt, and
		// answering 500 would make it look like the product is broken.
		httputil.JSON(w, http.StatusOK, map[string]any{
			"verified": false,
			"reason":   "no TXT record found at " + verifyHost(domain),
		})
		return
	}

	if !matchesToken(records, token) {
		httputil.JSON(w, http.StatusOK, map[string]any{
			"verified": false,
			"reason":   "the TXT record does not match the expected value",
		})
		return
	}

	// Verified. Adopt the mailboxes already on this domain in the same
	// statement — without it a mailbox created before verification keeps a NULL
	// domain_id and the outbound consumer keeps refusing to send from it, which
	// would present as "I verified the domain and mail still does not go out".
	if _, err := h.pool.Exec(r.Context(), `
		WITH verified AS (
			UPDATE mail_domains SET verified_at = COALESCE(verified_at, NOW())
			 WHERE id = $1 RETURNING id, domain, workspace_id
		)
		UPDATE mailboxes m SET domain_id = v.id
		  FROM verified v
		 WHERE m.workspace_id = v.workspace_id
		   AND m.domain_id IS NULL
		   AND lower(split_part(m.address, '@', 2)) = v.domain`, id); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"verified": true})
}

func (h *Handler) DeleteDomain(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "domain_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var workspaceID string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT workspace_id::text FROM mail_domains WHERE id = $1`, id).Scan(&workspaceID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}
	if _, err := h.pool.Exec(r.Context(), `DELETE FROM mail_domains WHERE id = $1`, id); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Ingest tokens
// ---------------------------------------------------------------------------

// IngestToken is the metadata. The secret is returned EXACTLY ONCE, at
// creation, and stored only as its sha256 — the same shape every other token in
// this product has, and the reason a leaked database is not a leaked
// credential.
type IngestToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	// Token is populated only by the create response.
	Token string `json:"token,omitempty"`
}

func (h *Handler) CreateIngestToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	_ = httputil.DecodeJSON(r, &req)

	secret, err := crypto.GenerateRandomToken(32)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var t IngestToken
	if err := h.pool.QueryRow(r.Context(), `
		INSERT INTO mail_ingest_tokens (workspace_id, name, token_hash, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, name, created_at, revoked_at`,
		workspaceID, strings.TrimSpace(req.Name), crypto.HashToken(secret),
		authctx.UserID(r.Context()),
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.RevokedAt); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	// The ONLY time the plaintext exists outside the caller's request. Said in
	// the response so a client can tell the operator to copy it now.
	t.Token = secret
	httputil.JSON(w, http.StatusCreated, map[string]any{
		"token": t,
		"note":  "This is the only time the token is shown. Store it in your mail provider's webhook configuration.",
	})
}

func (h *Handler) ListIngestTokens(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT id::text, name, created_at, revoked_at FROM mail_ingest_tokens
		  WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]IngestToken, 0, 8)
	for rows.Next() {
		var t IngestToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.RevokedAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, t)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// RevokeIngestToken stops a credential without deleting its row, so an audit
// can still answer "what was configured, and when did it stop".
func (h *Handler) RevokeIngestToken(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "token_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var workspaceID string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT workspace_id::text FROM mail_ingest_tokens WHERE id = $1`, id).Scan(&workspaceID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if !h.requireWorkspaceAdmin(w, r, workspaceID) {
		return
	}
	if _, err := h.pool.Exec(r.Context(),
		`UPDATE mail_ingest_tokens SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`,
		id); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
