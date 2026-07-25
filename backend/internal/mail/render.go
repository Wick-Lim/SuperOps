package mail

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"net/url"
	"path"
	"strings"
	texttemplate "text/template"
	"time"
)

// Templates live in the repository rather than in the database. A mail template
// is code — it is reviewed, diffed and rolled back with the binary that renders
// it, and an operator editing it in production is a defect, not a feature.
//
// A message kind is three files sharing a prefix:
//
//	<kind>.subject.tmpl   text/template, one line
//	<kind>.txt.tmpl       text/template
//	<kind>.html.tmpl      html/template, defines "content", invokes "layout"
//
// Adding password reset or a digest means dropping in three files; NewRenderer
// discovers them from the embedded FS and nothing here changes.
//
//go:embed templates/*.tmpl
var templateFS embed.FS

// Message kinds. The constants exist so callers and metrics agree on a spelling.
const (
	KindInvitation = "invitation"
	KindConfigTest = "config_test"
)

// layoutFile is parsed alongside every HTML template and supplies "layout".
const layoutFile = "templates/_layout.html.tmpl"

// RendererConfig configures link generation.
type RendererConfig struct {
	// BaseURL is the absolute, publicly reachable root of the web client. Every
	// link in every message is built from it, because a relative
	// "/invite/<token>" in an email is not a link at all.
	BaseURL string

	// ProductName appears in subjects and in the layout. Empty means "SuperOps".
	ProductName string
}

// Renderer turns typed data into a Message. It parses every template once at
// construction, so a broken template fails the boot rather than the first
// invitation, and is safe for concurrent use afterwards.
type Renderer struct {
	baseURL string
	product string

	subject map[string]*texttemplate.Template
	text    map[string]*texttemplate.Template
	html    map[string]*htmltemplate.Template
}

func NewRenderer(cfg RendererConfig) (*Renderer, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	product := strings.TrimSpace(cfg.ProductName)
	if product == "" {
		product = "SuperOps"
	}

	r := &Renderer{
		baseURL: base,
		product: product,
		subject: map[string]*texttemplate.Template{},
		text:    map[string]*texttemplate.Template{},
		html:    map[string]*htmltemplate.Template{},
	}

	kinds, err := fs.Glob(templateFS, "templates/*.subject.tmpl")
	if err != nil {
		return nil, fmt.Errorf("mail: enumerate templates: %w", err)
	}
	if len(kinds) == 0 {
		return nil, errors.New("mail: no message templates were embedded")
	}

	for _, subjectPath := range kinds {
		kind := strings.TrimSuffix(path.Base(subjectPath), ".subject.tmpl")

		subject, err := texttemplate.ParseFS(templateFS, subjectPath)
		if err != nil {
			return nil, fmt.Errorf("mail: parse %s subject template: %w", kind, err)
		}
		text, err := texttemplate.ParseFS(templateFS, "templates/"+kind+".txt.tmpl")
		if err != nil {
			return nil, fmt.Errorf("mail: parse %s text template: %w", kind, err)
		}
		html, err := htmltemplate.ParseFS(templateFS, layoutFile, "templates/"+kind+".html.tmpl")
		if err != nil {
			return nil, fmt.Errorf("mail: parse %s html template: %w", kind, err)
		}

		r.subject[kind] = subject
		r.text[kind] = text
		r.html[kind] = html
	}

	return r, nil
}

// BaseURL is the absolute root every link is built from.
func (r *Renderer) BaseURL() string { return r.baseURL }

// templateData is what every template sees. Shared chrome at the top level,
// message-specific fields under .Data, so a template reads
// `{{.Product}}` / `{{.Data.WorkspaceName}}` with no naming collisions.
type templateData struct {
	Product string
	BaseURL string
	Year    int
	Data    any
}

// URL makes a path absolute. Templates must call it for every link: it is the
// single place that knows the deployment's public origin.
func (t templateData) URL(p string) string {
	switch {
	case p == "":
		return t.BaseURL
	case strings.HasPrefix(p, "http://"), strings.HasPrefix(p, "https://"):
		return p
	default:
		return t.BaseURL + "/" + strings.TrimPrefix(p, "/")
	}
}

// InvitationData is the invitation template's view model.
type InvitationData struct {
	WorkspaceName string
	InviterName   string
	Role          string
	// AcceptPath is the single-use redemption path, e.g. "/invite/<token>". The
	// renderer makes it absolute; a caller must not pre-join it itself.
	AcceptPath string
	ExpiresAt  time.Time
}

// ConfigTestData is the admin verification message's view model.
type ConfigTestData struct {
	Transport   string
	RequestedBy string
	SentAt      time.Time
}

func (r *Renderer) Invitation(to Address, d InvitationData) (*Message, error) {
	return r.Render(KindInvitation, []Address{to}, d)
}

func (r *Renderer) ConfigTest(to Address, d ConfigTestData) (*Message, error) {
	return r.Render(KindConfigTest, []Address{to}, d)
}

// Render executes a kind's three templates into a Message.
//
// Every failure is permanent: a template that does not execute against its data
// will not execute against the same data a minute later.
func (r *Renderer) Render(kind string, to []Address, data any) (*Message, error) {
	subjectTmpl, ok := r.subject[kind]
	if !ok {
		return nil, permanent(fmt.Sprintf("no template for message kind %q", kind), nil)
	}

	td := templateData{Product: r.product, BaseURL: r.baseURL, Year: time.Now().Year(), Data: data}

	var subjectBuf, textBuf, htmlBuf bytes.Buffer
	if err := subjectTmpl.Execute(&subjectBuf, td); err != nil {
		return nil, permanent("render "+kind+" subject", err)
	}
	if err := r.text[kind].Execute(&textBuf, td); err != nil {
		return nil, permanent("render "+kind+" text body", err)
	}
	if err := r.html[kind].ExecuteTemplate(&htmlBuf, "layout", td); err != nil {
		return nil, permanent("render "+kind+" html body", err)
	}

	// A subject is a single header field: a template that emitted a newline
	// would otherwise inject a header.
	subject := collapseWhitespace(strings.ReplaceAll(strings.TrimSpace(subjectBuf.String()), "\n", " "))

	msg := &Message{
		To:      to,
		Subject: subject,
		Text:    strings.TrimLeft(textBuf.String(), "\n"),
		HTML:    strings.TrimLeft(htmlBuf.String(), "\n"),
	}
	if err := msg.Validate(); err != nil {
		return nil, permanent("rendered "+kind+" message is not sendable", err)
	}
	return msg, nil
}

// normalizeBaseURL requires an absolute http(s) origin and strips the trailing
// slash so path joining stays predictable.
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("mail: a public base URL is required to build links in messages")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mail: public base URL %q is not a URL: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("mail: public base URL must be an absolute http(s) URL (got %q)", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}
