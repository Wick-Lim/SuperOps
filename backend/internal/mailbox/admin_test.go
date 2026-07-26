package mailbox

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver is the reason SetResolver exists. Without it, verification could
// only be tested against a live DNS zone — which means it was not tested, which
// is how a security check becomes decorative.
type fakeResolver struct {
	records map[string][]string
	err     error
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	recs, ok := f.records[name]
	if !ok {
		return nil, errors.New("NXDOMAIN")
	}
	return recs, nil
}

// The host is a DEDICATED subdomain, not the apex.
//
// A TXT record on the apex shares a record set with SPF and DMARC, and some
// resolvers concatenate multiple strings in one record. Verifying there would
// mean parsing somebody else's mail configuration to find our token.
func TestVerifyHostIsADedicatedSubdomain(t *testing.T) {
	got := verifyHost("example.com")
	if got != "_superops.example.com" {
		t.Fatalf("verifyHost = %q", got)
	}
	if got == "example.com" {
		t.Fatal("verification would collide with SPF and DMARC on the apex")
	}
}

// The matcher is the whole check. It has to accept what registrars actually
// store and reject everything else.
func TestTXTMatching(t *testing.T) {
	const token = "abc123"

	for _, tt := range []struct {
		name    string
		records []string
		want    bool
	}{
		{"exact", []string{token}, true},
		// Some registrars store the value with the quotes included, and some
		// resolvers hand them back that way.
		{"quoted", []string{`"` + token + `"`}, true},
		{"padded", []string{"  " + token + "  "}, true},
		// The zone usually has other TXT records; ours only has to be present.
		{"among others", []string{"v=spf1 -all", token}, true},
		{"absent", []string{"v=spf1 -all"}, false},
		{"prefix only", []string{token + "extra"}, false},
		{"empty", []string{}, false},
		{"blank", []string{""}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesToken(tt.records, token)
			if got != tt.want {
				t.Errorf("matchesToken(%q, %q) = %v, want %v", tt.records, token, got, tt.want)
			}
		})
	}
}

// A resolver that could not answer must NOT verify. NXDOMAIN before the record
// is published is the normal first attempt, and treating a lookup failure as
// success would let anybody claim any domain by breaking their own DNS.
func TestALookupFailureDoesNotVerify(t *testing.T) {
	r := fakeResolver{err: errors.New("server misbehaving")}
	recs, err := r.LookupTXT(context.Background(), "_superops.example.com")
	if err == nil {
		t.Fatal("the fixture is wrong")
	}
	if matchesToken(recs, "abc123") {
		t.Fatal("a failed lookup matched the token")
	}
}
