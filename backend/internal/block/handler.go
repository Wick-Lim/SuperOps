package block

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	repo *Repository
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{repo: NewRepository(pool)}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/blocks", authMw(http.HandlerFunc(h.List)))
	mux.Handle("POST /api/v1/blocks", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("DELETE /api/v1/blocks/{blocked_id}", authMw(http.HandlerFunc(h.Delete)))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	blocks, err := h.repo.ListByBlocker(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if blocks == nil {
		blocks = []*Block{}
	}
	httputil.JSON(w, http.StatusOK, blocks)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		BlockedID string `json:"blocked_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.BlockedID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "blocked_id is required")
		return
	}

	// Without this the uuid type error surfaced from Postgres as a 500.
	if _, err := uuid.Parse(input.BlockedID); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "blocked_id must be a user id")
		return
	}

	// Also enforced by the user_blocks_no_self CHECK (migration 009); rejecting
	// here keeps it a 400 rather than a constraint violation.
	if input.BlockedID == userID {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "cannot block yourself")
		return
	}

	// Without this the foreign-key violation surfaced as a 500.
	exists, err := h.repo.UserExists(r.Context(), input.BlockedID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !exists {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}

	if err := h.repo.Create(r.Context(), userID, input.BlockedID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "user blocked"})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	blockedID := r.PathValue("blocked_id")

	if _, err := uuid.Parse(blockedID); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "blocked_id must be a user id")
		return
	}

	deleted, err := h.repo.Delete(r.Context(), userID, blockedID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !deleted {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "block not found")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "user unblocked"})
}
