package huddle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/rtc"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// tokenTTL bounds a join credential. Short, because it IS the grant: revoking a
// channel membership does not invalidate a token already minted, exactly as it
// does not invalidate a presigned download URL. Short enough that a revoked
// person cannot rejoin, long enough to survive a slow client start.
const tokenTTL = 5 * time.Minute

// Broadcaster is the WebSocket fan-out. An interface so huddle does not import
// internal/ws — the dependency runs huddle -> interface <- app, the same shape
// Drive uses for collab.
type Broadcaster interface {
	PublishHuddle(workspaceID, channelID, eventType string, data any)
}

type Handler struct {
	repo   *Repository
	authz  *authz.Checker
	media  rtc.Provider
	ice    rtc.ICEProvider
	hub    Broadcaster
	audit  *audit.Service
	logger *slog.Logger
	// webhookSecret verifies the SFU's callbacks. Empty means the webhook route
	// is NOT registered — an unauthenticated endpoint that writes the roster is
	// a stranger deciding who is in your calls.
	webhookSecret string
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, media rtc.Provider, ice rtc.ICEProvider,
	hub Broadcaster, auditor *audit.Service, webhookSecret string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if media == nil {
		media = rtc.Disabled{}
	}
	if ice == nil {
		ice = rtc.Disabled{}
	}
	return &Handler{
		repo: NewRepository(pool), authz: az, media: media, ice: ice,
		hub: hub, audit: auditor, webhookSecret: webhookSecret, logger: logger,
	}
}

// RegisterRoutes registers nothing when media is not deployed.
//
// That is the §3c contract made concrete: a capability the deployment does not
// have is ABSENT, not present-and-failing. A client asks GET /capabilities and
// hides the button; a client that did not still gets a 404 rather than a 500,
// which is the truthful answer to "start a call" on a server with no SFU.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	if !h.media.Enabled() {
		h.logger.Info("huddles disabled: no media server configured")
		return
	}
	mux.Handle("POST /api/v1/channels/{channel_id}/huddle", authMw(http.HandlerFunc(h.Start)))
	mux.Handle("GET /api/v1/channels/{channel_id}/huddle", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/huddle", authMw(http.HandlerFunc(h.End)))
}

// RegisterWebhookRoutes is separate because this route is UNAUTHENTICATED in
// the session sense — it is called by the media server, not by a person — and
// is verified by a signature instead. It is registered only when a secret
// exists, so a misconfigured deployment has no endpoint rather than an open one.
func (h *Handler) RegisterWebhookRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	if !h.media.Enabled() || h.webhookSecret == "" {
		return
	}
	mux.Handle("POST /api/v1/rtc/webhook", mw(http.HandlerFunc(h.Webhook)))
}

// scopeCapability is the whole authorization model: a huddle's capability IS
// its scope's. There is no huddle ACL to disagree with the channel.
func (h *Handler) scopeCapability(ctx context.Context, actor, channelID string) (authz.Capability, error) {
	return h.authz.Capability(ctx, authz.UserSubject(actor), authz.ChannelObject(channelID))
}

type startResponse struct {
	Huddle       *Huddle         `json:"huddle"`
	Token        string          `json:"token"`
	URL          string          `json:"url"`
	CanPublish   bool            `json:"can_publish"`
	ICEServers   []rtc.ICEServer `json:"ice_servers"`
	Participants []Participant   `json:"participants"`
}

// Start creates the huddle for a channel, or joins the one already running.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	channelID, err := httputil.RequirePathParam(r, "channel_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	capability, err := h.scopeCapability(ctx, actor, channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	// Read is enough to be IN a call — listening is a real product state.
	// Publishing needs write, which is the same rung that lets you post in the
	// channel, and it is enforced in the TOKEN rather than here: the SFU has no
	// idea what our permission model is, so the token is the only thing between
	// a reader and a microphone.
	if !capability.Implies(authz.CapRead) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	workspaceID, err := h.channelWorkspace(ctx, channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hud, created, err := h.repo.StartOrJoin(ctx, workspaceID, ScopeChannel, channelID, actor, 10)
	if err != nil {
		if errors.Is(err, ErrBadScope) {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_SCOPE", "unsupported huddle scope")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if created {
		if err := h.media.CreateRoom(ctx, hud.RoomName, hud.MaxParticipants); err != nil {
			// COMPENSATE. The row is committed and the room is not, so the
			// huddle is unjoinable — and leaving it live would block every
			// future attempt on this channel through the partial unique index.
			if _, endErr := h.repo.End(ctx, hud.ID, "failed"); endErr != nil {
				h.logger.Error("could not end a huddle whose room was never created",
					"huddle_id", hud.ID, "error", endErr)
			}
			h.logger.Error("create media room", "huddle_id", hud.ID, "error", err)
			httputil.JSONError(w, http.StatusBadGateway, "MEDIA_UNAVAILABLE",
				"the media server could not create the room")
			return
		}
		h.record(ctx, workspaceID, "huddle.started", hud.ID, map[string]any{"channel_id": channelID})
	}

	h.writeJoin(w, r, hud, capability, actor, created, channelID, workspaceID)
}

func (h *Handler) writeJoin(w http.ResponseWriter, r *http.Request, hud *Huddle,
	capability authz.Capability, actor string, created bool, channelID, workspaceID string) {

	ctx := r.Context()
	canPublish := capability.Implies(authz.CapWrite)

	token, err := h.media.Token(rtc.Grant{
		Room:         hud.RoomName,
		Identity:     actor,
		Name:         actor,
		CanPublish:   canPublish,
		CanSubscribe: true,
		// Audio and screen only. Camera is a cut, and the token is where the
		// cut is enforced: a client that asked to publish video would be
		// refused by the SFU rather than trusted.
		CanPublishSources: publishSources(canPublish),
		TTL:               tokenTTL,
	})
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	ice, err := h.ice.Servers(ctx, actor)
	if err != nil {
		// A relay list that could not be built is a call that may fail to
		// connect for some people. Logged, not fatal: STUN works for most.
		h.logger.Warn("could not build the ICE server list", "error", err)
		ice = nil
	}

	participants, err := h.repo.Participants(ctx, hud.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if created && h.hub != nil {
		// One frame, to the channel. NOT an inbox notification: a 500-member
		// channel would cost 500 inbox items and 500 pushes per click, and
		// migration 020's header forbids exactly this fan-out.
		h.hub.PublishHuddle(workspaceID, channelID, "huddle.started", map[string]any{
			"channel_id": channelID,
			"huddle_id":  hud.ID,
			"started_by": actor,
		})
	}

	httputil.JSON(w, http.StatusOK, startResponse{
		Huddle:       hud,
		Token:        token,
		URL:          h.media.URL(),
		CanPublish:   canPublish,
		ICEServers:   ice,
		Participants: participants,
	})
}

func publishSources(canPublish bool) []string {
	if !canPublish {
		return nil
	}
	return []string{"microphone", "screen_share", "screen_share_audio"}
}

// Get answers "is there a call in this channel", and joins it if so.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	channelID, err := httputil.RequirePathParam(r, "channel_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability, err := h.scopeCapability(ctx, actor, channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !capability.Implies(authz.CapRead) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	hud, err := h.repo.Live(ctx, ScopeChannel, channelID)
	if errors.Is(err, ErrNotFound) {
		// No call. 200 with a null huddle, not 404: "there is no call" is a
		// successful answer to "is there a call", and a 404 would be
		// indistinguishable from "no such channel".
		httputil.JSON(w, http.StatusOK, map[string]any{"huddle": nil})
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	workspaceID, err := h.channelWorkspace(ctx, channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	h.writeJoin(w, r, hud, capability, actor, false, channelID, workspaceID)
}

// End stops the call for everyone. Write capability, because ending a call
// somebody else is in is not a read.
func (h *Handler) End(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	channelID, err := httputil.RequirePathParam(r, "channel_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability, err := h.scopeCapability(ctx, actor, channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	switch {
	case !capability.Implies(authz.CapRead):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	case !capability.Implies(authz.CapWrite):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient capability to end the call")
		return
	}

	hud, err := h.repo.Live(ctx, ScopeChannel, channelID)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// The database FIRST, then the media server. If the order were reversed a
	// failure between them would leave a row claiming a live call whose room is
	// gone, and the partial unique index would block every new call on the
	// channel until the reconciler noticed.
	if _, err := h.repo.End(ctx, hud.ID, "ended_by_user"); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if err := h.media.DeleteRoom(ctx, hud.RoomName); err != nil {
		h.logger.Warn("could not delete the media room", "room", hud.RoomName, "error", err)
	}
	if h.hub != nil {
		h.hub.PublishHuddle(hud.WorkspaceID, channelID, "huddle.ended", map[string]any{
			"channel_id": channelID, "huddle_id": hud.ID, "reason": "ended_by_user",
		})
	}
	h.record(ctx, hud.WorkspaceID, "huddle.ended", hud.ID, map[string]any{"channel_id": channelID})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Webhook
// ---------------------------------------------------------------------------

// maxWebhookBody bounds the callback. The media server is a trusted peer, but a
// trusted peer that has been compromised should not be able to exhaust this
// process's memory.
const maxWebhookBody = 256 << 10

// Webhook applies one media-server event.
//
// VERIFIED BEFORE PARSED. The signature covers the raw body, so it is computed
// over the bytes rather than over a re-serialisation of the decoded struct —
// the classic way a signature check becomes decorative.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil || len(body) > maxWebhookBody {
		httputil.JSONError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", "webhook body too large")
		return
	}
	if !h.verify(r.Header.Get("Authorization"), body) {
		// 401 with no detail. Telling an unauthenticated caller WHY their
		// signature failed is telling them how to make one that works.
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid signature")
		return
	}

	var payload struct {
		ID    string `json:"id"`
		Event string `json:"event"`
		Room  struct {
			Name string `json:"name"`
		} `json:"room"`
		Participant struct {
			SID      string `json:"sid"`
			Identity string `json:"identity"`
		} `json:"participant"`
		Track struct {
			Source string `json:"source"`
		} `json:"track"`
		CreatedAt int64 `json:"createdAt"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_BODY", "malformed webhook payload")
		return
	}

	at := time.Time{}
	if payload.CreatedAt > 0 {
		at = time.Unix(payload.CreatedAt, 0).UTC()
	}
	ev := Event{
		ID:       payload.ID,
		Type:     payload.Event,
		Room:     payload.Room.Name,
		SID:      payload.Participant.SID,
		Identity: payload.Participant.Identity,
		Screen:   payload.Track.Source == "SCREEN_SHARE",
		At:       at,
	}

	if err := h.repo.Apply(r.Context(), ev); err != nil {
		// 500 so the media server RETRIES. Acking an event we failed to apply
		// is how a roster silently goes wrong: at-least-once only helps if the
		// receiver refuses what it could not process.
		h.logger.Error("apply media webhook", "event", ev.Type, "id", ev.ID, "error", err)
		httputil.JSONError(w, http.StatusInternalServerError, "INTERNAL", "could not apply event")
		return
	}

	// Fan the roster change out to the channel, best effort and after the
	// commit.
	if h.hub != nil && ev.Room != "" {
		if hud, err := h.repo.ByRoom(r.Context(), ev.Room); err == nil && hud.ScopeType == ScopeChannel {
			h.hub.PublishHuddle(hud.WorkspaceID, hud.ScopeID, "huddle.roster", map[string]any{
				"channel_id": hud.ScopeID,
				"huddle_id":  hud.ID,
			})
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// verify checks LiveKit's Authorization header, which is a JWT whose `sha256`
// claim is the base64 digest of the body.
func (h *Handler) verify(header string, body []byte) bool {
	if h.webhookSecret == "" || header == "" {
		return false
	}
	token := header
	if len(header) > 7 && (header[:7] == "Bearer " || header[:7] == "bearer ") {
		token = header[7:]
	}
	parts := splitN(token, '.', 3)
	if len(parts) != 3 {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.webhookSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(mac.Sum(nil), want) != 1 {
		return false
	}

	claimJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		SHA256 string `json:"sha256"`
		Exp    int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimJSON, &claims); err != nil {
		return false
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return false
	}
	// THE BODY DIGEST IS THE POINT. Without it a valid token replays any body
	// at all, and the signature check only proves somebody once held the
	// secret.
	sum := sha256.Sum256(body)
	return claims.SHA256 != "" &&
		subtle.ConstantTimeCompare([]byte(claims.SHA256), []byte(base64.StdEncoding.EncodeToString(sum[:]))) == 1
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// ---------------------------------------------------------------------------

func (h *Handler) channelWorkspace(ctx context.Context, channelID string) (string, error) {
	var ws string
	err := h.repo.pool.QueryRow(ctx, `SELECT workspace_id::text FROM channels WHERE id = $1`, channelID).Scan(&ws)
	return ws, err
}

func (h *Handler) record(ctx context.Context, workspaceID, action, id string, meta map[string]any) {
	if h.audit == nil {
		return
	}
	// Try, not Record: an audit write must not fail a call that already
	// started. The service's own buffer is what makes that safe.
	h.audit.Try(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      authctx.UserID(ctx),
		Action:       action,
		ResourceType: "huddle",
		ResourceID:   id,
		Metadata:     meta,
	})
}
