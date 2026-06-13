package block

import (
	"net/http"

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

	if input.BlockedID == userID {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "cannot block yourself")
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

	if err := h.repo.Delete(r.Context(), userID, blockedID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "user unblocked"})
}
