package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultDirectPort is SMTP's delivery port. Note that almost every cloud
// provider blocks outbound 25 — see the package comment — which is why this
// transport is not, and must never become, the default.
const defaultDirectPort = 25

// defaultDirectTimeout bounds delivery to one domain, across all of its MX
// hosts. Receiving MTAs are markedly slower than a submission relay: tarpitting
// and greylisting are normal, not pathological.
const defaultDirectTimeout = 60 * time.Second

// nullMX is the target of an RFC 7505 "this domain accepts no mail" record.
const nullMX = "."

// DirectConfig configures MX delivery.
type DirectConfig struct {
	From Address

	// HELO is the name announced in EHLO and it is load-bearing here, unlike on
	// an authenticated relay: receiving MTAs check that it is a FQDN, and many
	// check it against the connecting IP's PTR record. Getting it wrong is one
	// of the cheapest ways to be rejected outright.
	HELO string

	// Port overrides 25, for testing against a listener.
	Port int

	Timeout time.Duration

	// DKIM signs outgoing messages. Optional, and the transport shouts about it
	// when absent — nobody else signs on our behalf when we deliver directly.
	DKIM *DKIMSigner

	// Resolver overrides the default DNS resolver, for testing.
	Resolver *net.Resolver

	Logger *slog.Logger
}

// DirectSender delivers straight to each recipient domain's MX.
//
// For on-premises deployments on a host that allows outbound 25. It keeps
// company mail off a third party's infrastructure, and in exchange it inherits
// every deliverability problem that a provider would otherwise have solved:
// SPF, DKIM, DMARC, PTR records, IP reputation and warm-up.
type DirectSender struct {
	from     Address
	helo     string
	port     int
	timeout  time.Duration
	signer   *DKIMSigner
	resolver *net.Resolver
	logger   *slog.Logger

	// hostsFor resolves a domain to an ordered list of MX hosts. It is a field
	// rather than a direct call so a test can deliver to a loopback listener
	// without standing up a DNS server; production always uses mxHosts.
	hostsFor func(context.Context, string) ([]string, error)
}

func NewDirectSender(cfg DirectConfig) (*DirectSender, error) {
	if err := checkAddress("sender", cfg.From); err != nil {
		return nil, fmt.Errorf("mail: MAIL_FROM is unusable: %w", err)
	}
	helo := strings.TrimSpace(cfg.HELO)
	if helo == "" {
		return nil, errors.New("mail: the smtp-direct transport requires MAIL_DIRECT_HELO, " +
			"the public FQDN of this host; receiving MTAs check it against the connecting IP's PTR record")
	}
	if !strings.Contains(helo, ".") {
		return nil, fmt.Errorf("mail: MAIL_DIRECT_HELO must be a fully qualified domain name (got %q)", helo)
	}

	port := cfg.Port
	if port == 0 {
		port = defaultDirectPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("mail: MAIL_DIRECT_PORT must be between 1 and 65535 (got %d)", port)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultDirectTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	s := &DirectSender{
		from: cfg.From, helo: helo, port: port, timeout: timeout,
		signer: cfg.DKIM, resolver: resolver, logger: logger,
	}
	s.hostsFor = s.mxHosts

	fromDomain := cfg.From.Email[strings.LastIndex(cfg.From.Email, "@")+1:]
	switch {
	case cfg.DKIM == nil:
		logger.Warn("MAIL_TRANSPORT=smtp-direct with no DKIM key: outbound mail is UNSIGNED and will very likely be "+
			"classified as spam or rejected outright by Gmail, Outlook and every provider that enforces DMARC. "+
			"Set MAIL_DKIM_DOMAIN, MAIL_DKIM_SELECTOR and MAIL_DKIM_PRIVATE_KEY, and publish the matching TXT record",
			"from", cfg.From.Email)
	case !strings.EqualFold(cfg.DKIM.Domain(), fromDomain):
		logger.Warn("DKIM domain does not match the From domain: DMARC requires the two to align, "+
			"so a valid signature will still fail DMARC evaluation",
			"dkim_domain", cfg.DKIM.Domain(), "from_domain", fromDomain)
	default:
		logger.Info("smtp-direct will DKIM-sign outbound mail",
			"domain", cfg.DKIM.Domain(),
			"note", "delivery also requires an SPF record, a matching PTR record on the sending IP, and a warmed IP")
	}

	return s, nil
}

func (s *DirectSender) Name() string { return TransportSMTPDirect }

// Send groups recipients by domain and delivers each group to that domain's MX.
//
// One message is rendered per domain, not one per recipient: the To header then
// matches what was actually delivered, and a signature computed over it stays
// valid. Message.IdempotencyKey has no SMTP equivalent and is ignored — RFC
// 5321 gives a client no way to say "this is a retry of that", which is exactly
// why a provider's HTTP API can deduplicate and this cannot.
func (s *DirectSender) Send(ctx context.Context, msg *Message) error {
	if err := msg.Validate(); err != nil {
		return permanent("message is not sendable", err)
	}

	groups, err := groupByDomain(msg.To)
	if err != nil {
		return err
	}

	var (
		errs         []error
		allPermanent = true
	)
	for _, domain := range sortedKeys(groups) {
		if err := s.sendToDomain(ctx, domain, groups[domain], msg); err != nil {
			if !IsPermanent(err) {
				allPermanent = false
			}
			errs = append(errs, fmt.Errorf("%s: %w", domain, err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	joined := errors.Join(errs...)
	if allPermanent {
		return permanent("every recipient domain rejected the message", joined)
	}
	// A partial failure is redelivered in full, so a recipient whose domain
	// already accepted the message may receive it twice. That is the right
	// trade: the alternative is silently never delivering to the domains that
	// were still queued behind the failing one.
	//
	// %v, not %w, and that is load-bearing. errors.As walks into an
	// errors.Join tree, so wrapping a mixed result would let one domain's
	// *PermanentError make the whole send look permanent — and the domain that
	// was merely unreachable would never be retried. The aggregate's
	// classification has to be the aggregate's own.
	return fmt.Errorf("smtp-direct: %v", joined)
}

func (s *DirectSender) sendToDomain(ctx context.Context, domain string, to []Address, msg *Message) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// Re-render per domain so the To header names only this group.
	scoped := *msg
	scoped.To = to
	rendered, err := render(s.from, &scoped, time.Now())
	if err != nil {
		return err
	}
	if s.signer != nil {
		if err := s.signer.Sign(rendered, time.Now()); err != nil {
			return err
		}
	}
	raw := rendered.Bytes()

	rcpts := make([]string, 0, len(to))
	for _, a := range to {
		rcpts = append(rcpts, a.Email)
	}

	hosts, err := s.hostsFor(ctx, domain)
	if err != nil {
		return err
	}

	var lastErr error
	for _, host := range hosts {
		target := smtpTarget{
			addr:       net.JoinHostPort(host, strconv.Itoa(s.port)),
			serverName: host,
			helo:       s.helo,
			// Opportunistic: encrypt when the MX offers STARTTLS, deliver in the
			// clear when it does not, and do not verify the certificate. Without
			// DANE or MTA-STS there is nothing to verify a receiving MX's
			// certificate against, and refusing an unverifiable one would mean
			// falling back to plaintext for the same message anyway.
			tlsMode:     tlsOpportunistic,
			tlsInsecure: true,
			logger:      s.logger,
		}

		err := deliverSMTP(ctx, target, s.from.Email, rcpts, raw)
		if err == nil {
			s.logger.Info("mail delivered", "transport", TransportSMTPDirect,
				"domain", domain, "mx", host, "recipients", len(rcpts))
			return nil
		}
		if IsPermanent(err) {
			// A 5xx from an MX is the domain's answer, not this host's mood.
			// Trying the backup MX would produce the same rejection.
			return err
		}
		s.logger.Warn("mail: MX attempt failed, trying the next one",
			"domain", domain, "mx", host, "error", err)
		lastErr = err
	}
	return lastErr
}

// mxHosts resolves a domain's mail exchangers, most-preferred first.
func (s *DirectSender) mxHosts(ctx context.Context, domain string) ([]string, error) {
	records, err := s.resolver.LookupMX(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && (dnsErr.IsNotFound || dnsErr.IsTemporary) {
			if dnsErr.IsTemporary {
				return nil, fmt.Errorf("smtp-direct: MX lookup for %s: %w", domain, err)
			}
			// No MX record is not the same as no mail: RFC 5321 §5.1 says fall
			// back to the address record.
			return s.implicitMX(ctx, domain)
		}
		return nil, fmt.Errorf("smtp-direct: MX lookup for %s: %w", domain, err)
	}

	if len(records) == 0 {
		return s.implicitMX(ctx, domain)
	}
	if len(records) == 1 && strings.TrimSuffix(records[0].Host, ".") == "" {
		// RFC 7505 null MX: the domain states that it accepts no mail at all.
		return nil, permanent(fmt.Sprintf("%s publishes a null MX record and accepts no mail", domain), nil)
	}

	sorted := append([]*net.MX(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Pref < sorted[j].Pref })

	hosts := make([]string, 0, len(sorted))
	for _, r := range sorted {
		if host := strings.TrimSuffix(r.Host, "."); host != "" && host != nullMX {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return nil, permanent(fmt.Sprintf("%s has no usable MX record", domain), nil)
	}
	return hosts, nil
}

func (s *DirectSender) implicitMX(ctx context.Context, domain string) ([]string, error) {
	if _, err := s.resolver.LookupIPAddr(ctx, domain); err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, permanent(fmt.Sprintf("%s has neither an MX nor an address record", domain), err)
		}
		return nil, fmt.Errorf("smtp-direct: address lookup for %s: %w", domain, err)
	}
	return []string{domain}, nil
}

// groupByDomain buckets recipients by the domain part of their address.
func groupByDomain(to []Address) (map[string][]Address, error) {
	groups := make(map[string][]Address, len(to))
	for _, a := range to {
		at := strings.LastIndex(a.Email, "@")
		if at < 0 || at == len(a.Email)-1 {
			return nil, permanent(fmt.Sprintf("recipient %q has no domain", a.Email), nil)
		}
		domain := strings.ToLower(a.Email[at+1:])
		groups[domain] = append(groups[domain], a)
	}
	return groups, nil
}

// sortedKeys keeps delivery order deterministic, which makes a failure log
// reproducible.
func sortedKeys(m map[string][]Address) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
