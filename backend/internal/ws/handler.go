package ws

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type WSHandler struct {
	hub      *Hub
	jwtMgr   *auth.JWTManager
	presence *presence.Service
	logger   *slog.Logger
}

func NewWSHandler(hub *Hub, jwtMgr *auth.JWTManager, presenceSvc *presence.Service, logger *slog.Logger) *WSHandler {
	return &WSHandler{hub: hub, jwtMgr: jwtMgr, presence: presenceSvc, logger: logger}
}

func (h *WSHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/ws", h.HandleWebSocket)
}

func (h *WSHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Authenticate via query param
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token required")
		return
	}

	claims, err := h.jwtMgr.Validate(tokenStr)
	if err != nil {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid token")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow all origins in dev; restrict in production via nginx
	})
	if err != nil {
		h.logger.Error("websocket accept failed", "error", err)
		return
	}

	client := NewClient(h.hub, conn, claims.UserID, h.presence, h.logger)
	h.hub.register <- client

	if h.presence != nil {
		// context.Background: presence writes must outlive the request context,
		// which may already be near cancellation after the hijacked upgrade.
		_ = h.presence.SetOnline(context.Background(), claims.UserID)
		defer h.presence.SetStatus(context.Background(), claims.UserID, presence.StatusOffline)
	}

	// Send hello
	client.SendMessage(TypeHello, map[string]string{
		"user_id": claims.UserID,
	})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go client.WritePump(ctx)
	client.ReadPump(ctx) // blocks until disconnect
}
