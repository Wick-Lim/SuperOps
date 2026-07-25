// Package rtc is the real-time media capability.
//
// ROADMAP §3c: a DEPLOYMENT-DEPENDENT CAPABILITY. Audio and screen sharing need
// an SFU, an SFU is a second server somebody has to run, and a self-hosted
// product cannot assume one exists. So this package has two arms and the
// default is the honest one: no SFU configured means the feature is ABSENT —
// the routes are not registered, the client is told the capability is off, and
// nothing anywhere pretends a call is about to work.
//
// That is the same shape MinIO and Meilisearch already have in this codebase,
// and it is chosen for the same reason: a feature that half-works because a
// dependency is missing is worse than one that says so.
package rtc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors a caller must distinguish.
var (
	// ErrDisabled is every call into a provider that has no SFU. Callers map it
	// to 501, never to 500: nothing is broken, the capability is not deployed.
	ErrDisabled = errors.New("rtc: no media server is configured")
	// ErrRoomNotFound is a room the SFU does not know about, which after a
	// restart is most of them.
	ErrRoomNotFound = errors.New("rtc: room not found")
)

// Grant is what a token permits. Every field is a REFUSAL when false: the
// token is the only thing standing between a viewer and a microphone, because
// the SFU has no idea what our permission model is.
type Grant struct {
	Room string
	// Identity is the SFU's participant identity. It is the SuperOps user id,
	// so a webhook can be attributed without a second lookup.
	Identity string
	Name     string
	// CanPublish gates the microphone and the screen. A read-capability caller
	// joins with it false and can listen — which is a real product state, not a
	// degraded one.
	CanPublish   bool
	CanSubscribe bool
	// Sources the token allows. Camera is absent from v1 by decision, not by
	// omission: it is the phase's largest available cut, it removes the
	// tile-grid client work, and it takes a ten-person room from roughly
	// 135 Mbps of egress to single digits plus one screen stream. Re-enabling
	// it later is a token claim and a client component, not a re-architecture.
	CanPublishSources []string
	TTL               time.Duration
}

// Room is what the SFU knows about a room.
type Room struct {
	Name         string
	Participants []Participant
}

// Participant is one live session.
type Participant struct {
	SID      string
	Identity string
	Name     string
	// ScreenSharing is derived from the published tracks rather than from
	// anything a client asserted.
	ScreenSharing bool
	JoinedAt      time.Time
}

// Provider is the SFU. Small on purpose: everything above it is ours, and an
// interface that mirrored a vendor's API would make replacing them a rewrite.
type Provider interface {
	// Enabled reports whether media is deployed at all.
	Enabled() bool
	// CreateRoom is idempotent on name.
	CreateRoom(ctx context.Context, name string, maxParticipants int) error
	// Token mints a join credential.
	Token(g Grant) (string, error)
	// Room reads the live state, for the reconciler.
	Room(ctx context.Context, name string) (*Room, error)
	// DeleteRoom ends a call for everyone.
	DeleteRoom(ctx context.Context, name string) error
	// URL is the address the client connects to. Empty when disabled.
	URL() string
}

// ICEProvider is TURN, and it is SEPARATE from the SFU on purpose.
//
// They are different deployment decisions: an SFU on a public address needs no
// TURN, and a deployment behind a corporate firewall needs TURN even with a
// perfectly healthy SFU. Folding them into one interface would make "I have an
// SFU" imply "I have relays", which is the assumption that produces a call
// that connects for everybody except the people who most need it to.
type ICEProvider interface {
	// Servers returns the ICE configuration for one user. Empty is legitimate:
	// STUN-only works on most networks.
	Servers(ctx context.Context, identity string) ([]ICEServer, error)
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ---------------------------------------------------------------------------
// The disabled arm
// ---------------------------------------------------------------------------

// Disabled is the default provider. Every method fails with ErrDisabled rather
// than returning a zero value, so a caller that forgot to check Enabled gets an
// error it can map instead of a room that silently does nothing.
type Disabled struct{}

func (Disabled) Enabled() bool                                 { return false }
func (Disabled) CreateRoom(context.Context, string, int) error { return ErrDisabled }
func (Disabled) Token(Grant) (string, error)                   { return "", ErrDisabled }
func (Disabled) Room(context.Context, string) (*Room, error)   { return nil, ErrDisabled }
func (Disabled) DeleteRoom(context.Context, string) error      { return ErrDisabled }
func (Disabled) URL() string                                   { return "" }
func (Disabled) Servers(context.Context, string) ([]ICEServer, error) {
	return nil, nil
}

var (
	_ Provider    = Disabled{}
	_ ICEProvider = Disabled{}
)

// ---------------------------------------------------------------------------
// Static TURN
// ---------------------------------------------------------------------------

// StaticICE serves a fixed relay list with time-limited credentials.
//
// The credential scheme is coturn's `use-auth-secret`: username is
// "<expiry>:<identity>" and password is base64(HMAC-SHA1(secret, username)) —
// except that this uses SHA-256, which coturn also accepts and which is not
// worth being bug-compatible about. The point is that a leaked credential
// expires on its own; a static username and password in a config file is a
// relay anybody on the internet can use to move traffic through the customer's
// network.
type StaticICE struct {
	URLs   []string
	Secret string
	TTL    time.Duration
}

func (s StaticICE) Servers(_ context.Context, identity string) ([]ICEServer, error) {
	if len(s.URLs) == 0 {
		return nil, nil
	}
	if s.Secret == "" {
		// STUN needs no credential; TURN with a blank secret would be an open
		// relay, so it is refused rather than served credential-less.
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				return nil, errors.New("rtc: TURN is configured without a shared secret, " +
					"which would be an open relay")
			}
		}
		return []ICEServer{{URLs: s.URLs}}, nil
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	username := fmt.Sprintf("%d:%s", time.Now().Add(ttl).Unix(), identity)
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write([]byte(username))
	return []ICEServer{{
		URLs:       s.URLs,
		Username:   username,
		Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}}, nil
}

var _ ICEProvider = StaticICE{}

// ---------------------------------------------------------------------------
// LiveKit
// ---------------------------------------------------------------------------

// LiveKit talks to a LiveKit SFU.
//
// The token is minted here rather than through the vendor SDK: it is a plain
// JWT with a documented claim set, and minting it directly keeps this package's
// dependency list empty. The SDK would also pull a large transitive tree into a
// binary that mostly does not use it.
type LiveKit struct {
	Host      string
	APIKey    string
	APISecret string
	HTTP      HTTPDoer
}

// HTTPDoer is the http.Client surface this needs, so a test can answer without
// a server.
type HTTPDoer interface {
	Do(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest and HTTPResponse are deliberately minimal: the room API is three
// JSON POSTs, and modelling them on net/http would make the seam harder to fake
// than the thing it replaces.
type HTTPRequest struct {
	Ctx   context.Context
	Path  string
	Token string
	Body  []byte
}

type HTTPResponse struct {
	Status int
	Body   []byte
}

func (l *LiveKit) Enabled() bool {
	return l != nil && l.Host != "" && l.APIKey != "" && l.APISecret != ""
}

func (l *LiveKit) URL() string {
	if !l.Enabled() {
		return ""
	}
	return l.Host
}

// claims is the LiveKit JWT body.
type claims struct {
	Issuer   string     `json:"iss"`
	Subject  string     `json:"sub"`
	Name     string     `json:"name,omitempty"`
	NotAfter int64      `json:"exp"`
	IssuedAt int64      `json:"iat"`
	Video    videoGrant `json:"video"`
}

type videoGrant struct {
	Room         string   `json:"room,omitempty"`
	RoomJoin     bool     `json:"roomJoin,omitempty"`
	RoomCreate   bool     `json:"roomCreate,omitempty"`
	RoomAdmin    bool     `json:"roomAdmin,omitempty"`
	RoomList     bool     `json:"roomList,omitempty"`
	CanPublish   *bool    `json:"canPublish,omitempty"`
	CanSubscribe *bool    `json:"canSubscribe,omitempty"`
	Sources      []string `json:"canPublishSources,omitempty"`
}

func (l *LiveKit) Token(g Grant) (string, error) {
	if !l.Enabled() {
		return "", ErrDisabled
	}
	if g.Room == "" || g.Identity == "" {
		return "", errors.New("rtc: a token needs a room and an identity")
	}
	ttl := g.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	now := time.Now()
	canPublish := g.CanPublish
	canSubscribe := g.CanSubscribe
	return l.sign(claims{
		Issuer:   l.APIKey,
		Subject:  g.Identity,
		Name:     g.Name,
		IssuedAt: now.Unix(),
		NotAfter: now.Add(ttl).Unix(),
		Video: videoGrant{
			Room:     g.Room,
			RoomJoin: true,
			// Sources is what actually keeps a camera off the wire. An empty
			// list would mean "anything", so a caller with no publish rights
			// gets CanPublish false rather than an empty source list.
			CanPublish:   &canPublish,
			CanSubscribe: &canSubscribe,
			Sources:      g.CanPublishSources,
		},
	})
}

// adminToken is the server's own credential for the room API.
func (l *LiveKit) adminToken() (string, error) {
	now := time.Now()
	return l.sign(claims{
		Issuer:   l.APIKey,
		Subject:  l.APIKey,
		IssuedAt: now.Unix(),
		NotAfter: now.Add(1 * time.Minute).Unix(),
		Video:    videoGrant{RoomCreate: true, RoomAdmin: true, RoomList: true},
	})
}

func (l *LiveKit) sign(c claims) (string, error) {
	header := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("rtc: encode claims: %w", err)
	}
	unsigned := header + "." + base64url(body)
	mac := hmac.New(sha256.New, []byte(l.APISecret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64url(mac.Sum(nil)), nil
}

func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func (l *LiveKit) CreateRoom(ctx context.Context, name string, maxParticipants int) error {
	if !l.Enabled() {
		return ErrDisabled
	}
	body, _ := json.Marshal(map[string]any{
		"name":             name,
		"max_participants": maxParticipants,
		// The room outlives the last participant by this much, so a brief
		// reconnect does not destroy the call. Longer than a reconnect,
		// shorter than a coffee.
		"empty_timeout": 120,
	})
	_, err := l.call(ctx, "/twirp/livekit.RoomService/CreateRoom", body)
	return err
}

func (l *LiveKit) Room(ctx context.Context, name string) (*Room, error) {
	if !l.Enabled() {
		return nil, ErrDisabled
	}
	body, _ := json.Marshal(map[string]any{"room": name})
	raw, err := l.call(ctx, "/twirp/livekit.RoomService/ListParticipants", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Participants []struct {
			SID      string `json:"sid"`
			Identity string `json:"identity"`
			Name     string `json:"name"`
			JoinedAt int64  `json:"joined_at"`
			Tracks   []struct {
				Source string `json:"source"`
			} `json:"tracks"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("rtc: decode participants: %w", err)
	}
	room := &Room{Name: name}
	for _, p := range out.Participants {
		sharing := false
		for _, t := range p.Tracks {
			if strings.EqualFold(t.Source, "SCREEN_SHARE") {
				sharing = true
			}
		}
		room.Participants = append(room.Participants, Participant{
			SID:           p.SID,
			Identity:      p.Identity,
			Name:          p.Name,
			ScreenSharing: sharing,
			JoinedAt:      time.Unix(p.JoinedAt, 0),
		})
	}
	return room, nil
}

func (l *LiveKit) DeleteRoom(ctx context.Context, name string) error {
	if !l.Enabled() {
		return ErrDisabled
	}
	body, _ := json.Marshal(map[string]any{"room": name})
	_, err := l.call(ctx, "/twirp/livekit.RoomService/DeleteRoom", body)
	// Deleting a room the SFU has already forgotten is the normal case after a
	// restart, and it is a success from the caller's point of view: the room is
	// gone, which is what was asked for.
	if errors.Is(err, ErrRoomNotFound) {
		return nil
	}
	return err
}

func (l *LiveKit) call(ctx context.Context, path string, body []byte) ([]byte, error) {
	token, err := l.adminToken()
	if err != nil {
		return nil, err
	}
	if l.HTTP == nil {
		return nil, errors.New("rtc: no HTTP transport configured")
	}
	res, err := l.HTTP.Do(&HTTPRequest{Ctx: ctx, Path: strings.TrimRight(l.Host, "/") + path, Token: token, Body: body})
	if err != nil {
		return nil, fmt.Errorf("rtc: %s: %w", path, err)
	}
	switch {
	case res.Status == 404, res.Status == 200 && len(res.Body) == 0:
		return nil, ErrRoomNotFound
	case res.Status >= 400:
		return nil, fmt.Errorf("rtc: %s: status %d: %s", path, res.Status, truncate(res.Body, 256))
	}
	return res.Body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

var _ Provider = (*LiveKit)(nil)
