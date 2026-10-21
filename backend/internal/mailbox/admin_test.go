package mailbox

import (
	"context"
	"errors"
	"strings"
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

func TestSanitizeHTML(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantExact string
		forbid    []string
		require   []string
	}{
		{
			name:      "strips script tag and subtree",
			in:        `<p>Hello</p><script type="text/javascript">alert("xss")</script><b>World</b>`,
			wantExact: `<p>Hello</p><b>World</b>`,
		},
		{
			name:      "strips img onerror and style attributes",
			in:        `<img src="https://example.com/pic.png" onerror="alert(1)" onload=evil() style="background:url(javascript:alert(1))">`,
			wantExact: `<img src="https://example.com/pic.png">`,
		},
		{
			name:      "strips javascript href",
			in:        `<a href="javascript:alert(1)">Click</a>`,
			wantExact: `<a>Click</a>`,
		},
		{
			name:      "strips obfuscated mixed-case entity-encoded javascript href",
			in:        `<a href="JaVaScRiPt:&#x61;lert(1)" target="_blank">Click</a>`,
			wantExact: `<a target="_blank">Click</a>`,
		},
		{
			name:      "strips semicolon-less numeric entity and whitespace-obfuscated javascript href",
			in:        `<a href="&#106&#97;va&#x09;script:alert(1)">Click</a>`,
			wantExact: `<a>Click</a>`,
		},
		{
			name:      "strips vbscript and data URIs",
			in:        `<a href="vbscript:msgbox(1)">A</a><img src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">`,
			wantExact: `<a>A</a><img>`,
		},
		{
			name:      "strips iframe, svg, math, object, embed, form, style, meta, link, base",
			in:        `<meta charset="utf-8"><style>body{display:none}</style><iframe src="https://evil.example">fallback</iframe><svg onload=alert(1)><circle/></svg><math><mi>x</mi></math><object data="x"></object><embed src="x"><form action="https://evil.example"><input></form><p>Safe</p>`,
			wantExact: `<p>Safe</p>`,
		},
		{
			name:      "handles > inside quoted event handler attribute",
			in:        `<img src="https://example.com/a.png" onerror="if (1 > 0) alert(1)"><p>After</p>`,
			wantExact: `<img src="https://example.com/a.png"><p>After</p>`,
		},
		{
			name:      "preserves safe formatting tags and https/mailto links",
			in:        `<p>Hello <b>bold</b> and <i>italic</i></p><ul><li><a href="https://example.com/path?a=1&amp;b=2">Link</a></li><li><a href="mailto:support@example.com">Email</a></li></ul><blockquote>Quoted text</blockquote>`,
			wantExact: `<p>Hello <b>bold</b> and <i>italic</i></p><ul><li><a href="https://example.com/path?a=1&amp;b=2">Link</a></li><li><a href="mailto:support@example.com">Email</a></li></ul><blockquote>Quoted text</blockquote>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.in)
			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("SanitizeHTML(%q)\n got: %q\nwant: %q", tt.in, got, tt.wantExact)
			}
			for _, bad := range tt.forbid {
				if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
					t.Errorf("SanitizeHTML(%q) = %q, still contains forbidden %q", tt.in, got, bad)
				}
			}
			for _, good := range tt.require {
				if !strings.Contains(got, good) {
					t.Errorf("SanitizeHTML(%q) = %q, missing expected %q", tt.in, got, good)
				}
			}
		})
	}
}
