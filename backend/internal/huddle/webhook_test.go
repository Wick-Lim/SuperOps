package huddle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

const secret = "webhook-shared-secret-at-least-32-chars"

// sign builds the header the media server sends: a JWT whose `sha256` claim is
// the digest of the body.
func sign(t *testing.T, body []byte, tamper func(claims map[string]any)) string {
	t.Helper()
	sum := sha256.Sum256(body)
	c := map[string]any{
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
		"exp":    time.Now().Add(time.Minute).Unix(),
	}
	if tamper != nil {
		tamper(c)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return "Bearer " + unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// THE WEBHOOK IS THE ONLY WRITER OF PARTICIPANT ROWS, and it is unauthenticated
// in the session sense. Everything about who is in a call rests on this check.
func TestWebhookSignature(t *testing.T) {
	h := &Handler{webhookSecret: secret}
	body := []byte(`{"id":"ev1","event":"participant_joined","room":{"name":"hd_1"}}`)

	t.Run("a correct signature is accepted", func(t *testing.T) {
		if !h.verify(sign(t, body, nil), body) {
			t.Fatal("a valid signature was rejected")
		}
	})

	// THE ONE THAT MATTERS. The signature covers the body digest, so a valid
	// token cannot be replayed against a DIFFERENT payload — otherwise anybody
	// who once saw one webhook could write any roster they liked.
	t.Run("a valid token does not authenticate a different body", func(t *testing.T) {
		header := sign(t, body, nil)
		forged := []byte(`{"id":"ev2","event":"participant_joined","room":{"name":"hd_1"},` +
			`"participant":{"sid":"attacker","identity":"someone-else"}}`)
		if h.verify(header, forged) {
			t.Fatal("a token signed for one body authenticated another; the digest claim is " +
				"not being checked and the signature is decorative")
		}
	})

	t.Run("a body digest that does not match is rejected", func(t *testing.T) {
		header := sign(t, body, func(c map[string]any) { c["sha256"] = "AAAA" })
		if h.verify(header, body) {
			t.Fatal("accepted a mismatched body digest")
		}
	})

	t.Run("a missing body digest is rejected", func(t *testing.T) {
		header := sign(t, body, func(c map[string]any) { delete(c, "sha256") })
		if h.verify(header, body) {
			t.Fatal("accepted a token with no body digest, which replays against any payload")
		}
	})

	t.Run("an expired token is rejected", func(t *testing.T) {
		header := sign(t, body, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() })
		if h.verify(header, body) {
			t.Fatal("accepted an expired token")
		}
	})

	t.Run("a wrong secret is rejected", func(t *testing.T) {
		other := &Handler{webhookSecret: "a-completely-different-secret-value!!"}
		if other.verify(sign(t, body, nil), body) {
			t.Fatal("a signature from another secret was accepted")
		}
	})

	t.Run("garbage is rejected without panicking", func(t *testing.T) {
		for _, header := range []string{"", "Bearer ", "Bearer a", "Bearer a.b", "not-a-token",
			"Bearer !!!.???.###", "Bearer ..", "Bearer a.b.c.d"} {
			if h.verify(header, body) {
				t.Errorf("accepted %q", header)
			}
		}
	})

	// A handler with no secret must refuse EVERYTHING, not accept everything.
	// The route is not registered in that state, so this is the second line of
	// defence for a wiring mistake.
	t.Run("no secret means no signature is valid", func(t *testing.T) {
		open := &Handler{}
		if open.verify(sign(t, body, nil), body) {
			t.Fatal("a handler with no configured secret accepted a webhook")
		}
	})
}

// splitN is hand-rolled because strings.SplitN would allocate on a hot path
// that runs per webhook; it has to behave the same way.
func TestSplitN(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"a.b.c", []string{"a", "b", "c"}},
		{"a.b.c.d", []string{"a", "b", "c.d"}},
		{"a.b", []string{"a", "b"}},
		{"a", []string{"a"}},
		{"", []string{""}},
		{"..", []string{"", "", ""}},
	} {
		got := splitN(tt.in, '.', 3)
		if len(got) != len(tt.want) {
			t.Errorf("splitN(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitN(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// Camera is a cut, and this is where the cut is enforced: a caller with no
// publish rights gets a nil source list AND CanPublish false, because an empty
// list means "any source" to the media server.
func TestPublishSources(t *testing.T) {
	if got := publishSources(false); got != nil {
		t.Errorf("a non-publisher got sources %v", got)
	}
	got := publishSources(true)
	if len(got) == 0 {
		t.Fatal("a publisher got no sources, which the media server reads as ANY source")
	}
	for _, s := range got {
		if s == "camera" {
			t.Fatal("camera is in the publishable sources; it is cut from v1")
		}
	}
}
