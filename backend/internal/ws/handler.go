package ws

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// MemberChecker reports whether a user may subscribe to a channel's events.
// It returns (false, nil) for a non-member and a non-nil error only when the
// decision could not be made; the two must never be collapsed, or a database
// outage silently revokes everyone's subscriptions.
type MemberChecker func(ctx context.Context, channelID, userID string) (bool, error)

// WorkspaceLister returns every workspace a user belongs to. It is resolved
// once per connection so workspace-scoped events (channel.created,
// presence.changed) can be routed without a query per event.
type WorkspaceLister func(ctx context.Context, userID string) ([]string, error)

type WSHandler struct {
	hub        *Hub
	jwtMgr     *auth.JWTManager
	presence   *presence.Service
	isMember   MemberChecker
	workspaces WorkspaceLister
	acceptOpts *websocket.AcceptOptions
	logger     *slog.Logger
}

func NewWSHandler(
	hub *Hub,
	jwtMgr *auth.JWTManager,
	presenceSvc *presence.Service,
	isMember MemberChecker,
	workspaces WorkspaceLister,
	allowedOrigins []string,
	logger *slog.Logger,
) *WSHandler {
	return &WSHandler{
		hub:        hub,
		jwtMgr:     jwtMgr,
		presence:   presenceSvc,
		isMember:   isMember,
		workspaces: workspaces,
		acceptOpts: acceptOptions(allowedOrigins, logger),
		logger:     logger,
	}
}

// acceptOptions turns the CORS allowlist into WebSocket origin patterns. The
// upgrade previously set InsecureSkipVerify with a comment deferring the check
// to nginx, which no nginx config in the repo implements.
func acceptOptions(allowedOrigins []string, logger *slog.Logger) *websocket.AcceptOptions {
	patterns := make([]string, 0, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			logger.Warn("websocket origin verification disabled: CORS allowlist contains \"*\"")
			return &websocket.AcceptOptions{InsecureSkipVerify: true}
		}
		patterns = append(patterns, origin)
	}
	// With no patterns the request Host is still always authorised, so
	// same-origin browsers work and cross-origin ones are rejected. Non-browser
	// clients send no Origin header at all and are unaffected either way.
	return &websocket.AcceptOptions{OriginPatterns: patterns}
}

func (h *WSHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/ws", h.HandleWebSocket)
}

func (h *WSHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Authenticate via query param: the browser/RN WebSocket constructor cannot
	// set an Authorization header.
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

	// Resolve workspace membership before the upgrade: once Accept has hijacked
	// the connection there is no ResponseWriter left to report an error on.
	var workspaceIDs []string
	if h.workspaces != nil {
		workspaceIDs, err = h.workspaces(r.Context(), claims.UserID)
		if err != nil {
			h.logger.Error("websocket: list workspaces", "user_id", claims.UserID, "error", err)
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
	}

	conn, err := websocket.Accept(w, r, h.acceptOpts)
	if err != nil {
		// Accept has already written the failure response.
		h.logger.Warn("websocket accept failed", "error", err)
		return
	}

	client := NewClient(r.Context(), h.hub, conn, claims.UserID, workspaceIDs, h.presence, h.isMember, h.logger)
	defer client.close(websocket.StatusNormalClosure, "")

	select {
	case h.hub.register <- client:
	case <-h.hub.done:
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}

	if h.presence != nil {
		// context.Background: presence writes must outlive the request context,
		// which may already be near cancellation after the hijacked upgrade.
		ctx, cancel := context.WithTimeout(context.Background(), presenceOpTimeout)
		conns, err := h.presence.Connect(ctx, claims.UserID)
		cancel()
		if err != nil {
			h.logger.Warn("presence connect failed", "user_id", claims.UserID, "error", err)
		} else if conns == 1 {
			h.hub.PublishPresenceChanged(claims.UserID, string(presence.StatusOnline), workspaceIDs)
		}

		// Refcounted: the previous unconditional DEL marked a user offline when
		// any one of their devices disconnected, and a reconnect raced the old
		// connection's deferred delete.
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), presenceOpTimeout)
			defer cancel()
			last, err := h.presence.Disconnect(ctx, claims.UserID)
			if err != nil {
				h.logger.Warn("presence disconnect failed", "user_id", claims.UserID, "error", err)
				return
			}
			if last {
				h.hub.PublishPresenceChanged(claims.UserID, string(presence.StatusOffline), workspaceIDs)
			}
		}()
	}

	client.SendMessage(TypeHello, map[string]string{
		"user_id":       claims.UserID,
		"connection_id": client.connID,
	})

	go client.WritePump()
	go client.KeepAlive()
	client.ReadPump() // blocks until disconnect
}
