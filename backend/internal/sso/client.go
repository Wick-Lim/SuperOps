package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// maxResponseBytes caps every document we read from a provider. The issuer
	// is operator/admin-supplied, so "the other end is well behaved" is not an
	// assumption available here.
	maxResponseBytes = 1 << 20

	// defaultJWKSMinRefresh throttles the refetch triggered by an unknown
	// `kid`. Rotation must be picked up in seconds, but an attacker minting
	// tokens with random kids must not be able to turn each one into an
	// outbound request.
	defaultJWKSMinRefresh = 30 * time.Second

	maxRedirects = 3
)

// Client talks to OIDC providers and caches what is cacheable.
//
// One instance is shared by every workspace: the caches are keyed by issuer and
// by JWKS URI, so two workspaces pointing at the same tenant share the fetch,
// and two pointing at different ones cannot see each other's keys.
type Client struct {
	http          *http.Client
	cacheTTL      time.Duration
	allowInsecure bool
	// minRefresh is the floor between two JWKS fetches for the same URI.
	minRefresh time.Duration

	mu       sync.Mutex
	metadata map[string]*metadataEntry
	keys     map[string]*keySet
}

type metadataEntry struct {
	doc       *Metadata
	fetchedAt time.Time
}

type keySet struct {
	keys      []parsedKey
	fetchedAt time.Time
}

// Metadata is the subset of the OpenID Provider Metadata document (OIDC
// Discovery §3) this package uses.
type Metadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	UserinfoEndpoint                  string   `json:"userinfo_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

func NewClient(cfg Config) *Client {
	cfg = cfg.Defaults()
	return &Client{
		http:          newHTTPClient(cfg.HTTPTimeout, cfg.AllowInsecureIssuer),
		cacheTTL:      cfg.JWKSCacheTTL,
		allowInsecure: cfg.AllowInsecureIssuer,
		minRefresh:    defaultJWKSMinRefresh,
		metadata:      map[string]*metadataEntry{},
		keys:          map[string]*keySet{},
	}
}

// Metadata returns the provider's discovery document, cached.
//
// The document's own `issuer` must equal the configured issuer (OIDC Discovery
// §4.3). Skipping that check is how a provider — or anything that can answer
// for its hostname — redirects the token exchange somewhere else.
func (c *Client) Metadata(ctx context.Context, issuer string) (*Metadata, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if err := c.validateIssuer(issuer); err != nil {
		return nil, err
	}

	c.mu.Lock()
	entry, ok := c.metadata[issuer]
	c.mu.Unlock()
	if ok && time.Since(entry.fetchedAt) < c.cacheTTL {
		return entry.doc, nil
	}

	body, err := c.get(ctx, issuer+"/.well-known/openid-configuration")
	if err != nil {
		return nil, fmt.Errorf("fetch openid configuration: %w", err)
	}

	var doc Metadata
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode openid configuration: %w", err)
	}
	if strings.TrimRight(doc.Issuer, "/") != issuer {
		return nil, fmt.Errorf("openid configuration declares issuer %q, expected %q", doc.Issuer, issuer)
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"jwks_uri":               doc.JWKSURI,
	} {
		if endpoint == "" {
			return nil, fmt.Errorf("openid configuration has no %s", name)
		}
		if err := c.validateEndpoint(endpoint); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	c.mu.Lock()
	c.metadata[issuer] = &metadataEntry{doc: &doc, fetchedAt: time.Now()}
	c.mu.Unlock()

	return &doc, nil
}

// Key resolves the verification key for one ID token header.
//
// Rotation is handled here rather than by a timer: a provider rotates by
// publishing a new key and signing with it immediately, so the first token
// carrying an unknown `kid` is the notification. The refetch it triggers is
// throttled, and a cached set older than the TTL is revalidated regardless.
func (c *Client) Key(ctx context.Context, jwksURI, kid, alg string) (parsedKey, error) {
	c.mu.Lock()
	set := c.keys[jwksURI]
	c.mu.Unlock()

	if set != nil && time.Since(set.fetchedAt) < c.cacheTTL {
		if k, ok := selectKey(set.keys, kid, alg); ok {
			return k, nil
		}
		if time.Since(set.fetchedAt) < c.minRefresh {
			return parsedKey{}, fmt.Errorf("no key %q in the cached key set", kid)
		}
	}

	fresh, err := c.fetchKeys(ctx, jwksURI)
	if err != nil {
		return parsedKey{}, err
	}
	k, ok := selectKey(fresh, kid, alg)
	if !ok {
		return parsedKey{}, fmt.Errorf("the provider's key set has no key %q for algorithm %q", kid, alg)
	}
	return k, nil
}

func (c *Client) fetchKeys(ctx context.Context, jwksURI string) ([]parsedKey, error) {
	if err := c.validateEndpoint(jwksURI); err != nil {
		return nil, fmt.Errorf("jwks_uri: %w", err)
	}
	body, err := c.get(ctx, jwksURI)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.keys[jwksURI] = &keySet{keys: keys, fetchedAt: time.Now()}
	c.mu.Unlock()
	return keys, nil
}

// selectKey picks the key a token header names.
//
// A key set with exactly one key and a token with no `kid` is the common
// small-provider case and is allowed. With several keys and no `kid` there is
// no way to choose that is not "try them all", which is how algorithm-confusion
// bugs are built, so it is refused.
func selectKey(keys []parsedKey, kid, alg string) (parsedKey, bool) {
	if kid == "" {
		if len(keys) == 1 && algMatches(keys[0], alg) {
			return keys[0], true
		}
		return parsedKey{}, false
	}
	for _, k := range keys {
		if k.kid == kid && algMatches(k, alg) {
			return k, true
		}
	}
	return parsedKey{}, false
}

// algMatches enforces the provider's own pinning: a key published as RS256 must
// not be used to verify an ES256 (or HS256) signature.
func algMatches(k parsedKey, alg string) bool {
	return k.alg == "" || k.alg == alg
}

// ExchangeResponse is the token endpoint's success body (RFC 6749 §5.1).
type ExchangeResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// ExchangeInput is one authorization-code redemption.
type ExchangeInput struct {
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	Code          string
	RedirectURI   string
	CodeVerifier  string
	// AuthMethods is token_endpoint_auth_methods_supported from discovery.
	AuthMethods []string
}

// Exchange redeems the authorization code for an ID token.
//
// Client authentication follows the provider's advertised methods, defaulting
// to client_secret_basic (which RFC 6749 §2.3.1 requires every server to
// support). A provider that advertises only client_secret_post gets the
// credentials in the form instead. With no secret at all — a public client,
// which is legitimate here because PKCE is mandatory — only the client_id goes.
func (c *Client) Exchange(ctx context.Context, in ExchangeInput) (*ExchangeResponse, error) {
	if err := c.validateEndpoint(in.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("token_endpoint: %w", err)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {in.Code},
		"redirect_uri":  {in.RedirectURI},
		"code_verifier": {in.CodeVerifier},
		"client_id":     {in.ClientID},
	}

	useBasic := in.ClientSecret != "" && !onlyPostAuth(in.AuthMethods)
	if in.ClientSecret != "" && !useBasic {
		form.Set("client_secret", in.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if useBasic {
		// RFC 6749 §2.3.1: the two halves are form-urlencoded before base64.
		req.SetBasicAuth(url.QueryEscape(in.ClientID), url.QueryEscape(in.ClientSecret))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The OAuth error body is the only useful diagnostic a provider gives;
		// it names the misconfiguration (invalid_client, invalid_grant). It goes
		// to the log, never to the client.
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr) // best effort: not every provider sends JSON
		return nil, fmt.Errorf("token endpoint returned %d: %s %s", resp.StatusCode, oauthErr.Error, oauthErr.Description)
	}

	var out ExchangeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.IDToken == "" {
		return nil, errors.New("token response contains no id_token")
	}
	return &out, nil
}

func onlyPostAuth(methods []string) bool {
	if len(methods) == 0 {
		return false
	}
	for _, m := range methods {
		if m == "client_secret_basic" {
			return false
		}
	}
	// The provider advertised methods and basic was not among them.
	for _, m := range methods {
		if m == "client_secret_post" {
			return true
		}
	}
	return false
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
	}
	return body, nil
}

// validateIssuer rejects an issuer that cannot be an OIDC issuer, before any
// request is made. The value comes from a workspace administrator, who is not
// the operator of the deployment.
func (c *Client) validateIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("issuer %q is not a URL: %w", issuer, err)
	}
	if u.Host == "" {
		return fmt.Errorf("issuer %q has no host", issuer)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer %q must not carry a query or fragment", issuer)
	}
	return c.checkScheme(u)
}

func (c *Client) validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", endpoint, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", endpoint)
	}
	return c.checkScheme(u)
}

func (c *Client) checkScheme(u *url.URL) error {
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && c.allowInsecure {
		return nil
	}
	return fmt.Errorf("%q must use https (set SSO_ALLOW_INSECURE_ISSUER=true only for a local test provider)", u.Redacted())
}

// newHTTPClient builds the only outbound client this package uses.
//
// The Control hook runs after DNS resolution with the address actually about to
// be dialled, which is the only place a check survives DNS rebinding: a
// hostname that resolves public on the first lookup and to 169.254.169.254 on
// the second is checked both times.
func newHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("refusing to dial %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("refusing to dial %q: not an IP address", address)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s; an identity provider must be reachable on the public internet", ip)
			}
			return nil
		}
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   2,
			DisableCompression:    false,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// isPublicIP reports whether an address is one a real identity provider could
// live on. Everything private, local or reserved is refused: a workspace admin
// must not be able to aim this process at the cloud metadata service, at a
// database on the pod network, or at localhost.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return false
	}
	// 100.64.0.0/10 (RFC 6598 carrier-grade NAT) is neither "private" nor
	// public by Go's classification, and is where a lot of internal
	// infrastructure lives.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}
