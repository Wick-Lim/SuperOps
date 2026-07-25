package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// storageTestTimeout bounds the round trip behind POST /admin/storage/test. It
// sits below the server's write timeout so a wedged bucket answers 502 rather
// than the client seeing a truncated response.
const storageTestTimeout = 15 * time.Second

// StorageValidator is the part of storage.Backend this handler uses.
//
// An interface, and a deliberately tiny one: the admin package has no business
// reading or writing objects, and a *storage.Backend field here would let a
// future handler do exactly that. It is also what lets a test assert the
// failure path without standing up MinIO.
type StorageValidator interface {
	// Validate proves the bucket is readable, writable and deletable. It is the
	// same check that gates boot.
	Validate(ctx context.Context) error
	Name() string
}

// StorageDeps is object storage as seen by the admin surface. The zero value
// means storage is not configured, and the endpoint answers 503 — which is a
// different answer from "configured and broken", and the operator needs to be
// able to tell those apart.
type StorageDeps struct {
	Backend StorageValidator

	// Secrets are scrubbed from any error shown to the caller. A misconfigured
	// endpoint quotes the request back more often than you would hope.
	Secrets []string
}

// RegisterStorageRoutes is separate from RegisterRoutes for the same reason
// RegisterMailRoutes is: this route performs real I/O against a third party, so
// the composition root gives it its own rate limiter.
func (h *Handler) RegisterStorageRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/admin/storage/test", mw(http.HandlerFunc(h.TestStorage)))
}

// TestStorage round trips a marker object through the configured backend and
// reports what happened.
//
// ROADMAP §3c rule 3. Object storage misconfiguration is quiet in a specific
// way: a bucket policy that grants read and not write passes every "is it
// reachable" check and then fails on somebody's first upload; one that grants
// read and write but not delete passes and then makes the object garbage
// collector a no-op that logs success forever. None of that is visible without
// actually doing all three, which is exactly what this does — and it is the
// same Validate that gates boot, so an operator who gets a 200 here knows the
// process would start.
//
// It writes only to a reserved key under `.superops/`, so it cannot be turned
// into an upload endpoint, and it is rate limited in the composition root.
func (h *Handler) TestStorage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	workspaces, actor, ok := h.scope(w, r)
	if !ok {
		return
	}

	if h.storage.Backend == nil {
		httputil.JSONError(w, http.StatusServiceUnavailable, "STORAGE_NOT_CONFIGURED",
			"object storage is not configured on this deployment")
		return
	}

	transport := h.storage.Backend.Name()

	testCtx, cancel := context.WithTimeout(ctx, storageTestTimeout)
	defer cancel()

	if err := h.storage.Backend.Validate(testCtx); err != nil {
		h.mail.logger().Warn("storage configuration test failed",
			"backend", transport, "actor", actor, "error", err)

		// The backend's own error, verbatim apart from the credentials. It is
		// admin-only and rate limited, and a diagnostic that hides the reason is
		// not a diagnostic — "AccessDenied" and "connection refused" call for
		// completely different actions.
		httputil.JSONError(w, http.StatusBadGateway, "STORAGE_UNAVAILABLE",
			mail.Redact(err.Error(), h.storage.Secrets...))
		return
	}

	if h.audit != nil {
		// Try, not Log: the check has already run, so losing the trail entry
		// must be visible in the logs without failing a request that succeeded.
		h.audit.Try(ctx, audit.Entry{
			WorkspaceID:  workspaces[0],
			ActorID:      actor,
			Action:       "storage.test_ran",
			ResourceType: "storage",
			Metadata:     map[string]interface{}{"backend": transport},
		})
	}

	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"backend": transport,
		"checked": []string{"put", "get", "delete"},
		"note": "the bucket accepted a write, returned the same bytes and allowed the delete; " +
			"this is the same check that gates startup",
	})
}
