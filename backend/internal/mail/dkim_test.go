package mail

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()

	// 2048 bits, which is what a DKIM record should carry today.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, pem.EncodeToMemory(block)
}

func TestDKIMSignatureVerifiesWithTheMatchingPublicKey(t *testing.T) {
	key, pemKey := testRSAKey(t)

	signer, err := NewDKIMSigner("superops.example", "s1", pemKey)
	if err != nil {
		t.Fatalf("NewDKIMSigner: %v", err)
	}
	if signer.Domain() != "superops.example" {
		t.Errorf("Domain() = %q", signer.Domain())
	}

	rm, err := render(testFrom, testMessage(), time.Date(2026, 7, 25, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := signer.Sign(rm, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if rm.Headers[0].Name != "DKIM-Signature" {
		t.Fatalf("first header is %q, want DKIM-Signature", rm.Headers[0].Name)
	}

	// Verify the way a receiver would: parse the assembled message back, rebuild
	// the canonical input from the h= list in the signature, and check both the
	// body hash and the RSA signature.
	verifyDKIM(t, rm.Bytes(), &key.PublicKey)
}

func TestDKIMSignatureBreaksWhenTheBodyIsAltered(t *testing.T) {
	key, pemKey := testRSAKey(t)
	signer, _ := NewDKIMSigner("superops.example", "s1", pemKey)

	rm, err := render(testFrom, testMessage(), time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := signer.Sign(rm, time.Now()); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Rewrite the link in the body only — the same trick a phishing relay would
	// pull — leaving every header, including the signature, untouched.
	raw := rm.Bytes()
	sep := bytes.Index(raw, []byte(crlf+crlf))
	tampered := append([]byte(nil), raw[:sep+4]...)
	tampered = append(tampered, bytes.Replace(raw[sep+4:], []byte("chat.example.com"), []byte("phish.example.net"), 1)...)
	if bytes.Equal(tampered, raw) {
		t.Fatal("test did not actually alter the body")
	}

	if verifyDKIMBodyHash(t, tampered) {
		t.Error("the body hash still matched after the body was rewritten")
	}
	_ = key
}

func TestDKIMSignsTheHeadersThatMatter(t *testing.T) {
	_, pemKey := testRSAKey(t)
	signer, _ := NewDKIMSigner("superops.example", "s1", pemKey)

	rm, err := render(testFrom, testMessage(), time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := signer.Sign(rm, time.Now()); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tags := parseDKIMTags(rm.Headers[0].Value)
	signed := strings.Split(tags["h"], ":")
	for _, want := range []string{"from", "to", "subject", "date", "message-id", "content-type"} {
		if !contains(signed, want) {
			t.Errorf("h= is %q, missing %q — an intermediary could rewrite it undetected", tags["h"], want)
		}
	}
	if tags["a"] != "rsa-sha256" || tags["c"] != "relaxed/relaxed" || tags["v"] != "1" {
		t.Errorf("signature tags = %v, want v=1 a=rsa-sha256 c=relaxed/relaxed", tags)
	}
	if tags["d"] != "superops.example" || tags["s"] != "s1" {
		t.Errorf("d=%q s=%q", tags["d"], tags["s"])
	}
}

func TestCanonicalizeHeaderRelaxed(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"Subject", "  hello   world  ", "subject:hello world\r\n"},
		{"To", "a@x.test,\r\n\tb@y.test", "to:a@x.test, b@y.test\r\n"},
		{"MIME-Version", "1.0", "mime-version:1.0\r\n"},
	}
	for _, tc := range cases {
		if got := canonicalizeHeaderRelaxed(tc.name, tc.value); got != tc.want {
			t.Errorf("canonicalizeHeaderRelaxed(%q, %q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestCanonicalizeBodyRelaxed(t *testing.T) {
	// RFC 6376 §3.4.4: collapse internal whitespace, strip trailing whitespace,
	// drop trailing empty lines, end with exactly one CRLF.
	in := "line  one\t \r\nline two   \r\n\r\n\r\n"
	want := "line one\r\nline two\r\n"
	if got := string(canonicalizeBodyRelaxed([]byte(in))); got != want {
		t.Errorf("canonicalizeBodyRelaxed = %q, want %q", got, want)
	}

	if got := canonicalizeBodyRelaxed([]byte("\r\n\r\n")); len(got) != 0 {
		t.Errorf("an all-blank body canonicalised to %q, want empty", got)
	}
}

func TestNewDKIMSignerRejectsUnusableKeys(t *testing.T) {
	if _, err := NewDKIMSigner("d.test", "s", []byte("not pem at all")); err == nil {
		t.Error("accepted a non-PEM key")
	}

	_, pemKey := testRSAKey(t)
	if _, err := NewDKIMSigner("", "s", pemKey); err == nil {
		t.Error("accepted an empty domain")
	}
	if _, err := NewDKIMSigner("d.test", "", pemKey); err == nil {
		t.Error("accepted an empty selector")
	}

	// An EC key parses as PKCS#8 but is not RSA, and rsa-sha256 is the only
	// algorithm this signer implements.
	ecKey, err := x509.MarshalPKCS8PrivateKey(mustECKey(t))
	if err != nil {
		t.Fatalf("marshal EC key: %v", err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecKey})
	if _, err := NewDKIMSigner("d.test", "s", ecPEM); err == nil {
		t.Error("accepted a non-RSA key")
	}

	// The minimum key size guard cannot be exercised from here: crypto/rsa
	// refuses to generate anything under 1024 bits, which is the floor itself.
}

func mustECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return key
}

func TestNewDKIMSignerAcceptsPKCS8(t *testing.T) {
	key, _ := testRSAKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if _, err := NewDKIMSigner("d.test", "s", pkcs8); err != nil {
		t.Errorf("rejected a PKCS#8 key: %v", err)
	}
}

// --- receiver-side verification ----------------------------------------------

// verifyDKIM checks a signed message the way a receiving MTA would: it reads
// the DKIM-Signature out of the assembled bytes, recomputes the body hash,
// rebuilds the signed-header digest from the h= list with b= emptied, and
// verifies the RSA signature.
func verifyDKIM(t *testing.T, raw []byte, pub *rsa.PublicKey) {
	t.Helper()

	headers, body := splitMessage(t, raw)

	var sigValue string
	for _, h := range headers {
		if strings.EqualFold(h.Name, "DKIM-Signature") {
			sigValue = h.Value
		}
	}
	if sigValue == "" {
		t.Fatal("no DKIM-Signature header in the message")
	}

	tags := parseDKIMTags(sigValue)

	wantBH, err := base64.StdEncoding.DecodeString(tags["bh"])
	if err != nil {
		t.Fatalf("decode bh: %v", err)
	}
	gotBH := sha256.Sum256(canonicalizeBodyRelaxed(body))
	if !bytes.Equal(wantBH, gotBH[:]) {
		t.Fatal("bh= does not match the canonicalised body")
	}

	digest := sha256.New()
	for _, name := range strings.Split(tags["h"], ":") {
		var value string
		var found bool
		for _, h := range headers {
			if strings.EqualFold(h.Name, name) {
				value, found = h.Value, true
			}
		}
		if !found {
			t.Fatalf("h= names %q but the message has no such header", name)
		}
		digest.Write([]byte(canonicalizeHeaderRelaxed(name, value)))
	}

	parts := strings.Split(sigValue, "; ")
	for i, p := range parts {
		if strings.HasPrefix(p, "b=") {
			parts[i] = "b="
		}
	}
	digest.Write([]byte(strings.TrimSuffix(canonicalizeHeaderRelaxed("dkim-signature", strings.Join(parts, "; ")), crlf)))

	sig, err := base64.StdEncoding.DecodeString(tags["b"])
	if err != nil {
		t.Fatalf("decode b: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest.Sum(nil), sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func verifyDKIMBodyHash(t *testing.T, raw []byte) bool {
	t.Helper()

	headers, body := splitMessage(t, raw)
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "DKIM-Signature") {
			continue
		}
		want, err := base64.StdEncoding.DecodeString(parseDKIMTags(h.Value)["bh"])
		if err != nil {
			t.Fatalf("decode bh: %v", err)
		}
		got := sha256.Sum256(canonicalizeBodyRelaxed(body))
		return bytes.Equal(want, got[:])
	}
	t.Fatal("no DKIM-Signature header")
	return false
}

// splitMessage parses assembled bytes back into unfolded headers and a body.
func splitMessage(t *testing.T, raw []byte) ([]header, []byte) {
	t.Helper()

	idx := bytes.Index(raw, []byte(crlf+crlf))
	if idx < 0 {
		t.Fatal("message has no header/body separator")
	}
	body := raw[idx+4:]

	var headers []header
	for _, line := range strings.Split(string(raw[:idx]), crlf) {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(headers) > 0 {
			headers[len(headers)-1].Value += crlf + line
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("malformed header line %q", line)
		}
		headers = append(headers, header{Name: name, Value: strings.TrimPrefix(value, " ")})
	}
	return headers, body
}

func parseDKIMTags(value string) map[string]string {
	tags := map[string]string{}
	for _, part := range strings.Split(value, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return tags
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
