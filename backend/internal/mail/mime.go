package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
)

// crlf is the only line ending that appears in a rendered message. SMTP is
// defined in terms of CRLF and DKIM canonicalisation is defined in terms of
// CRLF-delimited lines, so a stray bare LF from a template would change the
// bytes an MTA sees relative to the bytes that were signed.
const crlf = "\r\n"

// maxHeaderLineOctets is RFC 5322's hard limit for one line, excluding CRLF.
// Nothing this product sends comes close, so exceeding it means a display name
// or a URL grew unexpectedly — a message defect, not a network problem.
const maxHeaderLineOctets = 998

// addressFoldColumn is where an address list is wrapped onto a continuation
// line. RFC 5322 recommends 78 octets per line; the header name and the ": "
// eat a few of those, so wrap a little earlier.
const addressFoldColumn = 70

// header is one rendered header field. A slice of these rather than a
// textproto.MIMEHeader map because DKIM signs headers in a specific order and
// a map has none.
type header struct {
	Name  string
	Value string
}

// renderedMessage is a message in its on-the-wire form, split at the blank
// line so DKIM can canonicalise the two halves separately and prepend its own
// header afterwards.
type renderedMessage struct {
	Headers []header
	Body    []byte
}

// prepend puts h first. DKIM-Signature conventionally leads the message, and
// (more importantly) a signature must not be inserted among the headers it
// covers in a way that changes their order.
func (r *renderedMessage) prepend(h header) {
	r.Headers = append([]header{h}, r.Headers...)
}

// Bytes serialises headers, the separating blank line, and the body.
//
// No dot-stuffing and no terminating dot: that is SMTP's transfer encoding,
// applied after signing by textproto.DotWriter inside net/smtp.
func (r *renderedMessage) Bytes() []byte {
	var buf bytes.Buffer
	for _, h := range r.Headers {
		buf.WriteString(h.Name)
		buf.WriteString(": ")
		buf.WriteString(h.Value)
		buf.WriteString(crlf)
	}
	buf.WriteString(crlf)
	buf.Write(r.Body)
	return buf.Bytes()
}

// render turns a Message into RFC 5322 bytes.
//
// Every failure here is permanent: a malformed address or an oversized header
// is a defect in the message, and redelivering it produces exactly the same
// result while burning the retry budget.
func render(from Address, msg *Message, now time.Time) (*renderedMessage, error) {
	if err := msg.Validate(); err != nil {
		return nil, permanent("message is not sendable", err)
	}
	if err := checkAddress("sender", from); err != nil {
		return nil, err
	}

	tos := make([]string, 0, len(msg.To))
	for _, a := range msg.To {
		if err := checkAddress("recipient", a); err != nil {
			return nil, err
		}
		tos = append(tos, formatAddress(a))
	}
	if strings.ContainsAny(msg.ReplyTo, "\r\n") {
		return nil, permanent("reply-to contains a line break", nil)
	}

	at := strings.LastIndex(from.Email, "@")
	domain := from.Email[at+1:]
	if domain == "" {
		return nil, permanent(fmt.Sprintf("sender address %q has no domain", from.Email), nil)
	}

	msgID, err := messageID(domain)
	if err != nil {
		return nil, err
	}

	bodyHeaders, body, err := buildBody(msg)
	if err != nil {
		return nil, err
	}

	headers := []header{
		{"From", formatAddress(from)},
		{"To", foldAddressList(tos)},
		{"Subject", mime.QEncoding.Encode("utf-8", msg.Subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", msgID},
		{"MIME-Version", "1.0"},
	}
	if msg.ReplyTo != "" {
		headers = append(headers, header{"Reply-To", msg.ReplyTo})
	}
	headers = append(headers, bodyHeaders...)
	// Everything this product sends is machine-generated. Saying so stops
	// out-of-office autoresponders from replying to a no-reply address and
	// stops some mailing lists from treating the message as human traffic.
	headers = append(headers, header{"Auto-Submitted", "auto-generated"})

	for _, h := range headers {
		for _, line := range strings.Split(h.Name+": "+h.Value, crlf) {
			if len(line) > maxHeaderLineOctets {
				return nil, permanent(fmt.Sprintf("header %s exceeds %d octets on one line", h.Name, maxHeaderLineOctets), nil)
			}
		}
	}

	return &renderedMessage{Headers: headers, Body: body}, nil
}

// buildBody renders the MIME body and the Content-* headers that describe it.
//
// Both bodies present is the normal case and produces multipart/alternative
// with text first: a client that understands both picks the last part it can
// render, and a client (or spam filter) that only reads text still finds the
// links. Quoted-printable rather than 8bit because a relay that has not
// advertised 8BITMIME is entitled to mangle raw UTF-8.
func buildBody(msg *Message) ([]header, []byte, error) {
	text := normalizeCRLF(msg.Text)
	html := normalizeCRLF(msg.HTML)

	switch {
	case text != "" && html != "":
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := writeQuotedPrintablePart(mw, "text/plain; charset=utf-8", text); err != nil {
			return nil, nil, err
		}
		if err := writeQuotedPrintablePart(mw, "text/html; charset=utf-8", html); err != nil {
			return nil, nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, nil, permanent("close multipart body", err)
		}
		return []header{{"Content-Type", "multipart/alternative; boundary=" + mw.Boundary()}}, buf.Bytes(), nil

	case html != "":
		return singlePartBody("text/html; charset=utf-8", html)

	default:
		return singlePartBody("text/plain; charset=utf-8", text)
	}
}

func singlePartBody(contentType, body string) ([]header, []byte, error) {
	var buf bytes.Buffer
	qp := quotedprintable.NewWriter(&buf)
	if _, err := io.WriteString(qp, body); err != nil {
		return nil, nil, permanent("encode body", err)
	}
	if err := qp.Close(); err != nil {
		return nil, nil, permanent("encode body", err)
	}
	return []header{
		{"Content-Type", contentType},
		{"Content-Transfer-Encoding", "quoted-printable"},
	}, buf.Bytes(), nil
}

func writeQuotedPrintablePart(mw *multipart.Writer, contentType, body string) error {
	h := make(textproto.MIMEHeader, 2)
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "quoted-printable")

	part, err := mw.CreatePart(h)
	if err != nil {
		return permanent("create MIME part", err)
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(qp, body); err != nil {
		return permanent("encode MIME part", err)
	}
	if err := qp.Close(); err != nil {
		return permanent("encode MIME part", err)
	}
	return nil
}

// normalizeCRLF rewrites every line ending as CRLF exactly once, whatever the
// template produced.
func normalizeCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, crlf, "\n"), "\n", crlf)
}

// checkAddress rejects the header-injection shapes before they reach a header.
// Message.Validate only checks the subject, and an attacker-supplied recipient
// (an invitation is created from a form field) is the other obvious vector.
func checkAddress(role string, a Address) error {
	if strings.ContainsAny(a.Name, "\r\n") {
		return permanent(fmt.Sprintf("%s display name contains a line break", role), nil)
	}
	if !strings.Contains(a.Email, "@") {
		return permanent(fmt.Sprintf("%s address %q has no domain", role, a.Email), nil)
	}
	if strings.ContainsAny(a.Email, "\r\n <>,;\"") || strings.TrimSpace(a.Email) != a.Email {
		return permanent(fmt.Sprintf("%s address %q contains characters that cannot appear in a header", role, a.Email), nil)
	}
	return nil
}

// formatAddress renders `Display Name <local@domain>`.
//
// A non-ASCII name is RFC 2047 encoded; an ASCII name containing an address
// special (a comma, most commonly) is quoted, because otherwise "Doe, Jane"
// splits one recipient into two.
func formatAddress(a Address) string {
	if a.Name == "" {
		return a.Email
	}
	enc := mime.QEncoding.Encode("utf-8", a.Name)
	if enc == a.Name && strings.ContainsAny(a.Name, "()<>@,;:\\\".[]") {
		enc = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.Name) + `"`
	}
	return enc + " <" + a.Email + ">"
}

// foldAddressList joins rendered addresses, wrapping onto continuation lines so
// a long list cannot push the header past the RFC 5322 line limit. Folding is
// invisible to DKIM: relaxed header canonicalisation unfolds first.
func foldAddressList(addrs []string) string {
	var b strings.Builder
	col := 0
	for i, a := range addrs {
		if i > 0 {
			b.WriteString(",")
			col++
			if col+len(a) > addressFoldColumn {
				b.WriteString(crlf + "\t")
				col = 1
			} else {
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(a)
		col += len(a)
	}
	return b.String()
}

func messageID(domain string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not something a redelivery fixes quickly, but
		// it is not a property of the message either — treat it as transient.
		return "", fmt.Errorf("mail: generate message id: %w", err)
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">", nil
}
