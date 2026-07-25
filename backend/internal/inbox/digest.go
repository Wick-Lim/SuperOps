package inbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
)

// Digest batching defaults. Both are configurable (INBOX_DIGEST_QUIET_PERIOD,
// INBOX_DIGEST_MIN_INTERVAL) because they are deployment policy: a support team
// wants a tighter loop than a company that mails once a day.
const (
	// DefaultQuietPeriod is what collapses a burst. An item still receiving
	// events is not yet mailed, and any new event resets notified_at to NULL and
	// last_at to now, pushing it back out of the window.
	DefaultQuietPeriod = 10 * time.Minute
	// DefaultMinInterval is the hard ceiling on mail volume per person per
	// workspace. email='immediate' is gated by the same counter, so choosing it
	// cannot exceed the digest's budget.
	DefaultMinInterval = time.Hour
	// digestScanLimit bounds one tick. The job runs every five minutes; a
	// backlog is drained over several ticks rather than in one transaction.
	digestScanLimit = 2000
	// digestItemsPerMail bounds one message. Beyond this the mail says "and N
	// more" rather than becoming a wall of text.
	digestItemsPerMail = 25
)

// MailQueue is the send path. mail.Publisher satisfies it; the interface exists
// so the digest job can be driven in a test without NATS.
type MailQueue interface {
	Queue(ctx context.Context, workspaceID, kind string, msg *mail.Message) error
}

// DigestRenderer renders one digest. *mail.Renderer satisfies it.
type DigestRenderer interface {
	Digest(to mail.Address, d mail.DigestData) (*mail.Message, error)
}

// DigestConfig is the job's tuning.
type DigestConfig struct {
	QuietPeriod time.Duration
	MinInterval time.Duration
}

// Digester is the body of the inbox_digest worker job.
type Digester struct {
	repo     *Repository
	renderer DigestRenderer
	queue    MailQueue
	cfg      DigestConfig
	logger   *slog.Logger
}

func NewDigester(repo *Repository, renderer DigestRenderer, queue MailQueue, cfg DigestConfig, logger *slog.Logger) *Digester {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.QuietPeriod <= 0 {
		cfg.QuietPeriod = DefaultQuietPeriod
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = DefaultMinInterval
	}
	return &Digester{repo: repo, renderer: renderer, queue: queue, cfg: cfg, logger: logger}
}

// Run selects candidates, groups them per (user, workspace), claims them and
// queues one message each. It reports how many digests it queued.
//
// # Claim-then-send, not send-then-claim
//
// If the publish fails after the claim commits, that digest is lost and logged.
// The alternative loses the crash window in the other direction and mails the
// same digest twice. Losing one digest beats mailing a company twice — that is
// the trade, and the ordering looks arbitrary unless it is written down.
//
// # The candidate set is filtered by preference AFTER selection, not in SQL
//
// Resolution is exact kind > '<resource>.*' > '*' > default, which is an ordered
// ladder over a small set of rows; expressing it in the candidate query would
// mean either a correlated subquery per row or a LATERAL join that no index
// serves. Instead the prefs for the whole candidate set are loaded per workspace
// with the same one-query-per-set rule the fan-out uses.
func (d *Digester) Run(ctx context.Context) (int, error) {
	if d.queue == nil || d.renderer == nil {
		return 0, nil
	}

	candidates, err := d.repo.DigestCandidates(ctx, d.cfg.QuietPeriod, d.cfg.MinInterval, digestScanLimit)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// Group by (user, workspace); one message per group.
	type group struct {
		user    DigestCandidate
		itemIDs []string
		items   []mail.DigestItem
		unread  int
	}
	groups := map[string]*group{}
	order := []string{}
	byWorkspace := map[string][]string{}

	for _, c := range candidates {
		key := c.UserID + "|" + c.WorkspaceID
		if _, ok := groups[key]; !ok {
			groups[key] = &group{user: c}
			order = append(order, key)
			byWorkspace[c.WorkspaceID] = append(byWorkspace[c.WorkspaceID], c.UserID)
		}
		groups[key].itemIDs = append(groups[key].itemIDs, c.ItemID)
	}

	// One preference query per workspace, not per candidate.
	prefsByWorkspace := map[string]PrefSet{}
	for ws, users := range byWorkspace {
		set, err := d.repo.LoadPrefs(ctx, ws, users)
		if err != nil {
			return 0, err
		}
		prefsByWorkspace[ws] = set
	}

	// Second pass, now that preferences are known.
	for _, c := range candidates {
		g := groups[c.UserID+"|"+c.WorkspaceID]
		p := prefsByWorkspace[c.WorkspaceID].Resolve(c.UserID, c.Kind)
		if p.Email == EmailNever {
			continue
		}
		g.unread += c.UnreadCount
		if len(g.items) >= digestItemsPerMail {
			continue
		}
		preview := c.Preview
		if c.PrivateSubject {
			// A digest quoting content the recipient may no longer be allowed to
			// read is the one failure this message must not have. A deep link
			// that 404s is fine; a quoted private message is not.
			preview = ""
		}
		g.items = append(g.items, mail.DigestItem{
			Title:       c.Title,
			Preview:     preview,
			UnreadCount: c.UnreadCount,
			Link:        deepLink(c),
		})
	}

	sent := 0
	for _, key := range order {
		g := groups[key]
		// Every item in the group resolved to email='never'. The items are still
		// claimed, so they are not reconsidered on every tick forever — the user
		// asked not to be mailed about them, not to be asked about them again in
		// five minutes.
		if err := d.repo.ClaimDigest(ctx, g.user.UserID, g.user.WorkspaceID, g.itemIDs); err != nil {
			d.logger.Error("inbox digest: claim failed, skipping group",
				"error", err, "user_id", g.user.UserID, "workspace_id", g.user.WorkspaceID)
			continue
		}
		if len(g.items) == 0 {
			continue
		}

		msg, err := d.renderer.Digest(mail.Address{Email: g.user.UserEmail, Name: g.user.UserName},
			mail.DigestData{
				UserName:      g.user.UserName,
				WorkspaceName: g.user.WorkspaceNm,
				TotalUnread:   g.unread,
				Items:         g.items,
			})
		if err != nil {
			d.logger.Error("inbox digest: render failed", "error", err, "user_id", g.user.UserID)
			continue
		}
		// The idempotency key collapses a retried publish into JetStream's
		// duplicate window. It is derived from the claim, so a redelivery of the
		// same claimed set is one message.
		msg.IdempotencyKey = "digest:" + EventID("inbox.digest", g.user.UserID, g.itemIDs[0])

		if err := d.queue.Queue(ctx, g.user.WorkspaceID, mail.KindDigest, msg); err != nil {
			// Claimed but not queued: this digest is lost. Logged at ERROR
			// because it is the failure mode the claim-then-send ordering
			// deliberately chose, and the only way anybody learns about it.
			d.logger.Error("inbox digest: claimed but not queued; this digest is lost",
				"error", err, "user_id", g.user.UserID, "workspace_id", g.user.WorkspaceID,
				"items", len(g.itemIDs))
			continue
		}
		sent++
	}
	return sent, nil
}

// deepLink is the path the digest links an item to. It is a path rather than a
// URL: mail's templateData.URL makes it absolute against the deployment's
// configured public origin, which is the single place that knows it.
func deepLink(c DigestCandidate) string {
	if c.SubjectType == SubjectChannel {
		return fmt.Sprintf("/channels/%s", c.SubjectID)
	}
	return fmt.Sprintf("/inbox?item=%s", c.ItemID)
}
