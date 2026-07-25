package mail

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// LogSender renders a message and logs it instead of delivering it.
//
// It is the default transport, and deliberately so. A fresh deployment must not
// be able to mail real people before an operator has chosen a transport on
// purpose — the failure mode of the other default (silently relaying through
// whatever the host's MTA is) is mail arriving from an address nobody controls.
//
// In development it replaces a mail-catcher: the whole text body is logged, and
// the links are pulled out separately so an invitation can be redeemed by
// copying one line out of `docker compose logs backend`.
type LogSender struct {
	logger *slog.Logger
}

func NewLogSender(logger *slog.Logger) *LogSender {
	return &LogSender{logger: logger}
}

func (s *LogSender) Name() string { return TransportLog }

// linkPattern finds the absolute URLs in a text body. Deliberately greedy about
// what ends a URL — trailing punctuation is trimmed below — because the whole
// point is that the operator can paste the result.
var linkPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

func (s *LogSender) Send(ctx context.Context, msg *Message) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mail: log transport: %w", err)
	}
	if err := msg.Validate(); err != nil {
		return permanent("message is not sendable", err)
	}

	to := make([]string, 0, len(msg.To))
	for _, a := range msg.To {
		to = append(to, a.String())
	}

	body := msg.Text
	if body == "" {
		body = msg.HTML
	}

	s.logger.Info("mail not sent: MAIL_TRANSPORT is 'log'",
		"to", strings.Join(to, ", "),
		"subject", msg.Subject,
		"idempotency_key", msg.IdempotencyKey,
		"links", extractLinks(body),
		"body", body,
	)
	return nil
}

func extractLinks(body string) []string {
	found := linkPattern.FindAllString(body, -1)
	links := make([]string, 0, len(found))
	seen := make(map[string]struct{}, len(found))
	for _, link := range found {
		link = strings.TrimRight(link, ".,;:)]}")
		if _, dup := seen[link]; dup {
			continue
		}
		seen[link] = struct{}{}
		links = append(links, link)
	}
	return links
}
