package rtc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The disabled arm is the DEFAULT, and it must fail rather than succeed
// quietly. A provider that returned an empty token and no error would produce a
// client that connects to nothing and shows an empty call.
func TestDisabledFailsClosed(t *testing.T) {
	var p Provider = Disabled{}
	if p.Enabled() {
		t.Fatal("the disabled provider reports itself enabled")
	}
	if _, err := p.Token(Grant{Room: "r", Identity: "u"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Token = %v, want ErrDisabled", err)
	}
	if err := p.CreateRoom(context.Background(), "r", 10); !errors.Is(err, ErrDisabled) {
		t.Errorf("CreateRoom = %v, want ErrDisabled", err)
	}
	if _, err := p.Room(context.Background(), "r"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Room = %v, want ErrDisabled", err)
	}
	if p.URL() != "" {
		t.Error("a disabled provider reported a URL, which a client would try to connect to")
	}
}

// A media host with no credentials cannot mint a token, so it must report
// itself disabled rather than fail at the first join — the deployment is
// misconfigured and the honest state is "no calls here".
func TestPartialConfigurationIsNotEnabled(t *testing.T) {
	for _, tt := range []struct {
		name string
		lk   LiveKit
	}{
		{"host only", LiveKit{Host: "http://sfu"}},
		{"no secret", LiveKit{Host: "http://sfu", APIKey: "k"}},
		{"no key", LiveKit{Host: "http://sfu", APISecret: "s"}},
		{"nothing", LiveKit{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lk := tt.lk
			if lk.Enabled() {
				t.Error("reported enabled with an incomplete configuration")
			}
		})
	}
}

// THE TOKEN IS THE PERMISSION BOUNDARY. The media server has no idea what this
// product's permission model is, so a read-capability caller is kept off the
// microphone by the token and by nothing else.
func TestTokenCarriesTheGrantAndNothingMore(t *testing.T) {
	lk := &LiveKit{Host: "http://sfu", APIKey: "devkey", APISecret: "devsecret-at-least-32-characters!!"}

	t.Run("a listener cannot publish", func(t *testing.T) {
		tok, err := lk.Token(Grant{Room: "hd_1", Identity: "u1", CanSubscribe: true})
		if err != nil {
			t.Fatal(err)
		}
		v := decodeClaims(t, tok)
		if v.Video.CanPublish == nil || *v.Video.CanPublish {
			t.Fatal("a read-capability caller was given publish rights")
		}
		if v.Video.CanSubscribe == nil || !*v.Video.CanSubscribe {
			t.Error("a listener cannot subscribe, so they would join a silent call")
		}
	})

	t.Run("a publisher gets audio and screen and NOT camera", func(t *testing.T) {
		tok, err := lk.Token(Grant{
			Room: "hd_1", Identity: "u1", CanPublish: true, CanSubscribe: true,
			CanPublishSources: []string{"microphone", "screen_share", "screen_share_audio"},
		})
		if err != nil {
			t.Fatal(err)
		}
		v := decodeClaims(t, tok)
		for _, src := range v.Video.Sources {
			if strings.Contains(src, "camera") {
				t.Fatalf("camera is in the token sources (%v); it is a cut, and the token is "+
					"where the cut is enforced", v.Video.Sources)
			}
		}
		if len(v.Video.Sources) == 0 {
			t.Error("an empty source list means ANY source to the media server, including camera")
		}
	})

	t.Run("the token expires", func(t *testing.T) {
		tok, err := lk.Token(Grant{Room: "r", Identity: "u", TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		v := decodeClaims(t, tok)
		if v.NotAfter == 0 {
			t.Fatal("no expiry: the token IS the grant, and revoking a membership does not " +
				"invalidate one already minted")
		}
		if v.NotAfter-v.IssuedAt > 600 {
			t.Errorf("token lives %ds, which is long enough for a revoked person to rejoin",
				v.NotAfter-v.IssuedAt)
		}
	})

	t.Run("a token needs a room and an identity", func(t *testing.T) {
		if _, err := lk.Token(Grant{Identity: "u"}); err == nil {
			t.Error("minted a token with no room")
		}
		if _, err := lk.Token(Grant{Room: "r"}); err == nil {
			t.Error("minted a token with no identity, so no webhook could attribute it")
		}
	})
}

func decodeClaims(t *testing.T, token string) claims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TURN with no secret would be an OPEN RELAY: anybody on the internet could
// move traffic through the customer's network. Refusing is the only safe
// answer; serving the URL credential-less would look like it worked.
func TestTURNWithoutASecretIsRefused(t *testing.T) {
	_, err := StaticICE{URLs: []string{"turn:relay.example:3478"}}.Servers(context.Background(), "u1")
	if err == nil {
		t.Fatal("served a TURN URL with no credential, which is an open relay")
	}

	// STUN needs no credential and must still work — refusing it would break
	// the deployments that need no TURN at all.
	got, err := StaticICE{URLs: []string{"stun:stun.example:3478"}}.Servers(context.Background(), "u1")
	if err != nil || len(got) != 1 || got[0].Username != "" {
		t.Fatalf("STUN-only = %+v, %v", got, err)
	}
}

// The credential is time-limited and per-identity, so a leaked one expires on
// its own rather than becoming a permanent relay account.
func TestTURNCredentialsExpireAndDifferPerUser(t *testing.T) {
	ice := StaticICE{URLs: []string{"turn:relay.example:3478"}, Secret: "shared", TTL: time.Hour}
	a, err := ice.Servers(context.Background(), "user-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ice.Servers(context.Background(), "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Username == b[0].Username || a[0].Credential == b[0].Credential {
		t.Fatal("two users got the same relay credential, so revoking one revokes both")
	}
	if !strings.Contains(a[0].Username, "user-a") {
		t.Errorf("username %q does not identify the user, so nothing can be attributed", a[0].Username)
	}
	if !strings.HasPrefix(a[0].Username, "1") {
		t.Errorf("username %q carries no expiry timestamp", a[0].Username)
	}
}

// A room the media server has already forgotten is the normal case after a
// restart, and deleting it is a SUCCESS: the room is gone, which is what was
// asked for. Returning an error would make the end-call path fail for the most
// common reason it is ever called.
func TestDeletingAForgottenRoomSucceeds(t *testing.T) {
	lk := &LiveKit{
		Host: "http://sfu", APIKey: "k", APISecret: "s",
		HTTP: fakeDoer(func(*HTTPRequest) (*HTTPResponse, error) {
			return &HTTPResponse{Status: 404, Body: []byte(`{"code":"not_found"}`)}, nil
		}),
	}
	if err := lk.DeleteRoom(context.Background(), "hd_gone"); err != nil {
		t.Fatalf("DeleteRoom on a forgotten room = %v, want nil", err)
	}
	// But READING one is not: the reconciler needs to tell "forgotten" from
	// "the media server is down", and treating both as empty would end every
	// live call during an outage.
	if _, err := lk.Room(context.Background(), "hd_gone"); !errors.Is(err, ErrRoomNotFound) {
		t.Errorf("Room = %v, want ErrRoomNotFound", err)
	}
}

// Screen sharing is derived from the published tracks, never from anything a
// client asserted.
func TestScreenSharingComesFromTheTracks(t *testing.T) {
	lk := &LiveKit{
		Host: "http://sfu", APIKey: "k", APISecret: "s",
		HTTP: fakeDoer(func(*HTTPRequest) (*HTTPResponse, error) {
			return &HTTPResponse{Status: 200, Body: []byte(`{"participants":[
				{"sid":"s1","identity":"u1","joined_at":100,"tracks":[{"source":"MICROPHONE"}]},
				{"sid":"s2","identity":"u2","joined_at":200,"tracks":[{"source":"SCREEN_SHARE"}]}
			]}`)}, nil
		}),
	}
	room, err := lk.Room(context.Background(), "hd_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(room.Participants) != 2 {
		t.Fatalf("got %d participants", len(room.Participants))
	}
	if room.Participants[0].ScreenSharing {
		t.Error("a microphone track was read as a screen share")
	}
	if !room.Participants[1].ScreenSharing {
		t.Error("a screen share track was not recognised")
	}
}

type fakeDoer func(*HTTPRequest) (*HTTPResponse, error)

func (f fakeDoer) Do(r *HTTPRequest) (*HTTPResponse, error) { return f(r) }
