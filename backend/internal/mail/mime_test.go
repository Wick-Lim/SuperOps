package mail

import (
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
	"time"
)

var testFrom = Address{Name: "SuperOps", Email: "no-reply@superops.example"}

func testMessage() *Message {
	return &Message{
		To:      []Address{{Name: "Dana", Email: "dana@example.org"}},
		Subject: "You have been invited",
		Text:    "Open https://chat.example.com/invite/abc\n",
		HTML:    "<p>Open <a href=\"https://chat.example.com/invite/abc\">here</a></p>\n",
	}
}

func TestValidateRejectsUnsendableMessages(t *testing.T) {
	base := testMessage()

	cases := []struct {
		name string
		mut  func(*Message)
	}{
		{"no recipients", func(m *Message) { m.To = nil }},
		{"recipient without a domain", func(m *Message) { m.To = []Address{{Email: "dana"}} }},
		{"empty subject", func(m *Message) { m.Subject = "   " }},
		{"no body at all", func(m *Message) { m.Text, m.HTML = "", "" }},
		{"header injection through the subject", func(m *Message) {
			m.Subject = "hi\r\nBcc: victim@example.org"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := *base
			tc.mut(&msg)
			if err := msg.Validate(); err == nil {
				t.Fatal("Validate accepted a message it must reject")
			}
		})
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("Validate rejected a well-formed message: %v", err)
	}
}

func TestRenderProducesAMultipartAlternativeMessage(t *testing.T) {
	rm, err := render(testFrom, testMessage(), time.Date(2026, 7, 25, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	got := headerMap(rm)
	if got["From"] != `SuperOps <no-reply@superops.example>` {
		t.Errorf("From = %q", got["From"])
	}
	if got["To"] != `Dana <dana@example.org>` {
		t.Errorf("To = %q", got["To"])
	}
	if got["Subject"] != "You have been invited" {
		t.Errorf("Subject = %q", got["Subject"])
	}
	if got["MIME-Version"] != "1.0" {
		t.Errorf("MIME-Version = %q", got["MIME-Version"])
	}
	if !strings.HasSuffix(got["Message-ID"], "@superops.example>") {
		t.Errorf("Message-ID = %q, want it scoped to the sender domain", got["Message-ID"])
	}
	if got["Auto-Submitted"] != "auto-generated" {
		t.Errorf("Auto-Submitted = %q", got["Auto-Submitted"])
	}

	mediaType, params, err := mime.ParseMediaType(got["Content-Type"])
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", got["Content-Type"], err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}

	// Both bodies must be present: an HTML-only message scores worse with spam
	// filters, and a text-only one loses the button.
	// multipart.Part transparently decodes quoted-printable (and removes the
	// Content-Transfer-Encoding header while doing so), so reading the parts back
	// checks the encoding round-trips rather than just that a header was set.
	if !strings.Contains(string(rm.Body), "Content-Transfer-Encoding: quoted-printable") {
		t.Error("parts are not quoted-printable encoded")
	}

	mr := multipart.NewReader(strings.NewReader(string(rm.Body)), params["boundary"])
	var types, bodies []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		decoded, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		types = append(types, part.Header.Get("Content-Type"))
		bodies = append(bodies, string(decoded))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("parts = %v, want text/plain then text/html", types)
	}
	for i, body := range bodies {
		if !strings.Contains(body, "https://chat.example.com/invite/abc") {
			t.Errorf("part %d lost the link after encoding: %q", i, body)
		}
	}
}

func TestRenderedMessageUsesCRLFThroughout(t *testing.T) {
	rm, err := render(testFrom, testMessage(), time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	raw := string(rm.Bytes())
	// A bare LF would mean the bytes an MTA sees differ from the bytes DKIM
	// signed, and would be rewritten by some relays.
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' && (i == 0 || raw[i-1] != '\r') {
			t.Fatalf("bare LF at offset %d", i)
		}
	}
	if !strings.Contains(raw, "\r\n\r\n") {
		t.Fatal("no blank line separating headers from body")
	}
}

func TestRenderRejectsHeaderInjectionThroughARecipient(t *testing.T) {
	msg := testMessage()
	msg.To = []Address{{Name: "Dana", Email: "dana@example.org\r\nBcc: victim@example.org"}}

	_, err := render(testFrom, msg, time.Now())
	if err == nil {
		t.Fatal("render accepted a recipient address containing a line break")
	}
	if !IsPermanent(err) {
		t.Errorf("error is %v, want a permanent one: redelivering a malformed address cannot help", err)
	}
}

func TestFormatAddressQuotesNamesThatWouldSplitTheList(t *testing.T) {
	got := formatAddress(Address{Name: "Doe, Jane", Email: "jane@example.org"})
	want := `"Doe, Jane" <jane@example.org>`
	if got != want {
		t.Errorf("formatAddress = %q, want %q", got, want)
	}

	// Non-ASCII must be RFC 2047 encoded, not passed through raw.
	got = formatAddress(Address{Name: "Zoë", Email: "zoe@example.org"})
	if strings.Contains(got, "Zoë") {
		t.Errorf("formatAddress = %q, want the display name encoded", got)
	}
	if decoded, err := new(mime.WordDecoder).DecodeHeader(strings.TrimSuffix(got, " <zoe@example.org>")); err != nil || decoded != "Zoë" {
		t.Errorf("encoded name %q did not decode back (got %q, err %v)", got, decoded, err)
	}
}

func TestFoldAddressListStaysWithinTheLineLimit(t *testing.T) {
	addrs := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		addrs = append(addrs, "person-with-a-long-name-"+strings.Repeat("x", 10)+"@example.org")
	}
	folded := foldAddressList(addrs)
	for _, line := range strings.Split(folded, crlf) {
		if len(line) > maxHeaderLineOctets {
			t.Fatalf("folded line is %d octets, over the RFC 5322 limit", len(line))
		}
	}
	if !strings.Contains(folded, crlf+"\t") {
		t.Fatal("a 12-address list was not folded at all")
	}
}

func headerMap(rm *renderedMessage) map[string]string {
	m := make(map[string]string, len(rm.Headers))
	for _, h := range rm.Headers {
		m[h.Name] = h.Value
	}
	return m
}
