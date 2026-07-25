// DKIM signing (RFC 6376), relaxed/relaxed, rsa-sha256.
//
// This exists for the smtp-direct transport. A relay operated by a provider
// signs on our behalf; delivering straight to a recipient's MX means nobody
// else will, and an unsigned message from an unfamiliar IP is spam as far as
// every large mailbox provider is concerned.
//
// Signing is necessary but nowhere near sufficient. The receiver validates the
// signature against a TXT record at <selector>._domainkey.<domain>, which this
// code cannot publish, and then weighs it against SPF, DMARC alignment, the
// sending IP's reputation and its PTR record. None of that is assertable from
// inside this process.
package mail

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dkimSignedHeaders is the h= list, in signing order.
//
// From is mandatory. The rest are the headers a receiver would otherwise let an
// intermediary rewrite without breaking the signature — which is the whole
// point of signing them. Headers absent from the message are skipped rather
// than signed as empty, and each is signed at most once, so no "oversigning"
// entries are emitted.
var dkimSignedHeaders = []string{
	"from", "to", "subject", "date", "message-id", "mime-version", "content-type",
}

// minDKIMKeyBits rejects a key too short to be worth signing with. 1024 is the
// floor still accepted by receivers; 2048 is what should be generated today.
const minDKIMKeyBits = 1024

// DKIMSigner signs rendered messages. It holds no per-message state and is safe
// for concurrent use.
type DKIMSigner struct {
	domain   string
	selector string
	key      *rsa.PrivateKey
}

// NewDKIMSigner parses a PEM-encoded RSA private key (PKCS#1 or PKCS#8) and
// binds it to the d=/s= pair whose TXT record carries the matching public key.
func NewDKIMSigner(domain, selector string, pemKey []byte) (*DKIMSigner, error) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	selector = strings.TrimSpace(strings.ToLower(selector))
	if domain == "" || selector == "" {
		return nil, errors.New("mail: DKIM requires both a domain and a selector")
	}

	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("mail: DKIM private key is not PEM encoded")
	}

	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mail: parse DKIM PKCS#1 key: %w", err)
		}
		key = parsed
	default:
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mail: parse DKIM private key: %w", err)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("mail: DKIM private key is %T, want an RSA key", parsed)
		}
		key = rsaKey
	}

	if bits := key.N.BitLen(); bits < minDKIMKeyBits {
		return nil, fmt.Errorf("mail: DKIM key is %d bits, want at least %d", bits, minDKIMKeyBits)
	}

	return &DKIMSigner{domain: domain, selector: selector, key: key}, nil
}

// Domain reports the d= value, for logging and for the startup warning that
// compares it against the From address.
func (s *DKIMSigner) Domain() string { return s.domain }

// Sign prepends a DKIM-Signature header covering rm.
//
// Order matters and is fixed by RFC 6376: hash the canonicalised body into bh=,
// build the signature header with an empty b=, hash the signed headers followed
// by that header (canonicalised, without its trailing CRLF), sign the digest,
// then fill b= in.
func (s *DKIMSigner) Sign(rm *renderedMessage, now time.Time) error {
	bodyHash := sha256.Sum256(canonicalizeBodyRelaxed(rm.Body))

	present := make(map[string][]string, len(rm.Headers))
	for _, h := range rm.Headers {
		lower := strings.ToLower(h.Name)
		present[lower] = append(present[lower], h.Value)
	}

	signed := make([]string, 0, len(dkimSignedHeaders))
	for _, name := range dkimSignedHeaders {
		if len(present[name]) > 0 {
			signed = append(signed, name)
		}
	}

	value := strings.Join([]string{
		"v=1",
		"a=rsa-sha256",
		"c=relaxed/relaxed",
		"d=" + s.domain,
		"s=" + s.selector,
		"t=" + strconv.FormatInt(now.Unix(), 10),
		"h=" + strings.Join(signed, ":"),
		"bh=" + base64.StdEncoding.EncodeToString(bodyHash[:]),
		"b=",
	}, "; ")

	h := sha256.New()
	for _, name := range signed {
		// The last instance of a header is the one signed when a name repeats;
		// nothing this package renders repeats, but the rule is cheap to honour.
		values := present[name]
		h.Write([]byte(canonicalizeHeaderRelaxed(name, values[len(values)-1])))
	}
	// The signature header itself is canonicalised with an empty b= and, per
	// RFC 6376 §3.7, without the trailing CRLF.
	h.Write([]byte(strings.TrimSuffix(canonicalizeHeaderRelaxed("dkim-signature", value), crlf)))

	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return fmt.Errorf("mail: DKIM sign: %w", err)
	}

	rm.prepend(header{Name: "DKIM-Signature", Value: value + base64.StdEncoding.EncodeToString(sig)})
	return nil
}

// canonicalizeHeaderRelaxed implements RFC 6376 §3.4.2: lowercase the name,
// unfold the value, collapse runs of whitespace to a single space, strip
// leading and trailing whitespace, and emit "name:value" with no space after
// the colon.
func canonicalizeHeaderRelaxed(name, value string) string {
	unfolded := strings.NewReplacer("\r\n", "", "\r", "", "\n", "").Replace(value)
	return strings.ToLower(strings.TrimSpace(name)) + ":" + collapseWhitespace(unfolded) + crlf
}

// canonicalizeBodyRelaxed implements RFC 6376 §3.4.4: collapse runs of
// whitespace within each line, strip trailing whitespace from each line, drop
// trailing empty lines, and terminate a non-empty body with exactly one CRLF.
func canonicalizeBodyRelaxed(body []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(body), crlf, "\n"), "\n")
	for i, line := range lines {
		lines[i] = collapseWhitespace(line)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, crlf) + crlf)
}

// collapseWhitespace reduces every run of SP/TAB to one SP and trims the ends.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}
