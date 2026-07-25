package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/push"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// SubjectRequested is the NATS subject a pillar publishes an inbox request on.
// One durable consumer (DurableFanout) binds FilterRequested and calls Deliver,
// so a new pillar adds zero durables and zero worker wiring.
const (
	EventRequested  = "inbox.requested"
	FilterRequested = "superops.*.inbox.requested"
	DurableFanout   = "inbox-fanout"
)

// SubjectFor builds the publish subject for a workspace.
func SubjectFor(workspaceID string) string {
	return "superops." + workspaceID + ".inbox.requested"
}

// DeviceTokenLister resolves the push tokens registered for a user. It is
// satisfied by user.Repository; declared here so this package does not depend on
// the user package for one query.
type DeviceTokenLister interface {
	PushTokensForUser(ctx context.Context, userID string) ([]string, error)
}

// Pusher accepts push messages for asynchronous delivery. Enqueue must not
// block: it is called from inside a JetStream consumer callback whose latency is
// the consumer's ack latency. push.Dispatcher is the implementation.
type Pusher interface {
	Enqueue(msgs []push.Message)
}

// Notifier is the fan-out. It is the ONLY way rows enter this package, whether
// the caller is in-process (internal/notification) or a pillar publishing on
// SubjectRequested.
type Notifier struct {
	repo   *Repository
	nats   *natspkg.Client
	logger *slog.Logger

	// devices and pusher are both nil when PUSH_ENABLED is off, which is the
	// default. Every push path checks for that rather than the config, so
	// "push is disabled" and "push is misconfigured" collapse to the same no-op
	// instead of a nil dereference in the fan-out.
	devices DeviceTokenLister
	pusher  Pusher
}

func NewNotifier(
	repo *Repository,
	nc *natspkg.Client,
	devices DeviceTokenLister,
	pusher Pusher,
	logger *slog.Logger,
) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{repo: repo, nats: nc, devices: devices, pusher: pusher, logger: logger}
}

// Deliver fans one event out to its recipients.
//
// Ordering is the whole contract, and it mirrors what the message fan-out
// already did (createAndPush): persist first, then relay, then push, and push
// ONLY for the recipients whose event row was genuinely new.
//
//   - A failed write is RETURNED. The caller is a durable consumer and the
//     return value is its ack decision; swallowing it acks an event nobody was
//     told about, and nothing in this system replays these.
//   - A failed relay or push is only LOGGED. The row is committed, the recipient
//     sees it on their next fetch, and naking a whole fan-out to retry a
//     fire-and-forget publish is a worse trade.
//   - A redelivery does neither. It produces no item projection at all (the
//     upsert only touches the recipients whose event row was new), so there is
//     nothing to relay and nothing to push — which is the correct answer for
//     both: the client already has the frame, and a duplicate push is a second
//     buzz in the recipient's pocket for something they have already read.
func (n *Notifier) Deliver(ctx context.Context, req Request) ([]Delivered, error) {
	recipients, err := n.validate(&req)
	if err != nil {
		return nil, err
	}
	if len(recipients) == 0 {
		return nil, nil
	}

	// One query for the whole recipient set. See PrefSet.
	prefs, err := n.repo.LoadPrefs(ctx, req.WorkspaceID, recipients)
	if err != nil {
		return nil, err
	}

	// in_app=false means NO ITEM IS CREATED AT ALL, not a hidden item. A
	// suppressed notification that still counts toward a badge is the worst of
	// both: the user turned it off and the bell still says 1.
	wanted := make([]string, 0, len(recipients))
	pushWanted := make(map[string]bool, len(recipients))
	for _, uid := range recipients {
		p := prefs.Resolve(uid, req.Kind)
		if !p.InApp {
			continue
		}
		wanted = append(wanted, uid)
		pushWanted[uid] = p.Push
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	delivered, err := n.repo.Deliver(ctx, req, wanted, time.Now())
	if err != nil {
		return nil, err
	}

	for i := range delivered {
		delivered[i].Push = pushWanted[delivered[i].UserID]
		n.relay(req.WorkspaceID, &delivered[i])
	}
	n.enqueuePush(ctx, req, delivered)
	return delivered, nil
}

// validate rejects a request that no redelivery can fix, and normalises the
// recipient list.
//
// Everything here is a *PermanentError on purpose: a malformed kind, an
// unparseable id or an over-wide audience are all properties of the event, and
// naking them five times before terminating just delays the same outcome while
// occupying a consumer slot.
func (n *Notifier) validate(req *Request) ([]string, error) {
	if _, err := uuid.Parse(req.WorkspaceID); err != nil {
		return nil, &PermanentError{Reason: "inbox: workspace_id is not a uuid: " + req.WorkspaceID}
	}
	if !ValidKind(req.Kind) {
		return nil, &PermanentError{Reason: "inbox: kind must be '<resource>.<verb>', got " + req.Kind}
	}
	if !validObjectType(req.ObjectType) || !validObjectType(req.SubjectType) {
		return nil, &PermanentError{Reason: "inbox: object_type/subject_type must match ^[a-z][a-z0-9_]{0,31}$"}
	}
	if _, err := uuid.Parse(req.ObjectID); err != nil {
		return nil, &PermanentError{Reason: "inbox: object_id is not a uuid: " + req.ObjectID}
	}
	if _, err := uuid.Parse(req.SubjectID); err != nil {
		return nil, &PermanentError{Reason: "inbox: subject_id is not a uuid: " + req.SubjectID}
	}
	if req.Title == "" {
		return nil, &PermanentError{Reason: "inbox: title is required"}
	}
	if req.ActorID != "" {
		if _, err := uuid.Parse(req.ActorID); err != nil {
			return nil, &PermanentError{Reason: "inbox: actor_id is not a uuid: " + req.ActorID}
		}
	}
	if len(req.Recipients) > MaxRecipients {
		return nil, &PermanentError{
			Reason: fmt.Sprintf("inbox: %d recipients is past the %d cap for one event",
				len(req.Recipients), MaxRecipients),
		}
	}

	// Deduplicate, preserving order. The item upsert is one multi-row statement,
	// and Postgres refuses an ON CONFLICT DO UPDATE that would touch one row
	// twice — so a producer that lists the same person under two headings (a
	// mention inside a DM) would otherwise fail the whole fan-out.
	seen := make(map[string]bool, len(req.Recipients))
	out := make([]string, 0, len(req.Recipients))
	for _, uid := range req.Recipients {
		if uid == "" || seen[uid] {
			continue
		}
		if _, err := uuid.Parse(uid); err != nil {
			return nil, &PermanentError{Reason: "inbox: recipient is not a uuid: " + uid}
		}
		seen[uid] = true
		out = append(out, uid)
	}
	return out, nil
}

// relay publishes the realtime frame for one recipient.
//
// d.Item is nil on a redelivery — Repository.Deliver only projects the
// recipients whose event row was genuinely new — so this is a no-op for one
// without needing to test Created separately.
//
// The payload is the LEGACY notification shape, and the subject is the legacy
// one, because internal/ws's relay routes `notification.new` by the `user_id`
// field and the shipped React Native client renders exactly those keys
// (app/src/lib/types AppNotification). Changing either would be a wire break for
// a client on its own release schedule; the inbox item rides inside the same
// frame under `item` for clients that know about it.
func (n *Notifier) relay(workspaceID string, d *Delivered) {
	if n.nats == nil || d.Item == nil {
		return
	}
	payload := legacyProjection(d.Item)
	payload["item"] = d.Item
	if err := n.nats.Publish("superops."+workspaceID+".notification.created",
		natspkg.Event{Type: "notification.new", Data: payload}); err != nil {
		n.logger.Warn("inbox: relay", "error", err, "user_id", d.UserID, "kind", d.Item.TopKind)
	}
}

// enqueuePush addresses committed items to their recipients' devices.
//
// # On who gets a push
//
// Exactly the audience that got an item, minus those whose resolved preference
// says otherwise, minus every recipient for whom this was a redelivery. There is
// no second audience query here that could drift away from the first one.
//
// # On not checking whether the user is already connected
//
// The tempting optimisation is to skip the push when the recipient has a live
// WebSocket. internal/presence keeps a per-user refcount in Redis, so the worker
// could consult it. It is deliberately not done: the refcount is per USER, not
// per device, so a desktop tab left open in an office would silence the push to
// the phone in the user's pocket — the one device push exists for. And "has a
// socket" is not "is looking at this channel"; the only process with the facts is
// the client, which makes that call in app/src/lib/push.ts where being wrong
// costs a redundant banner rather than a message nobody ever learns about.
func (n *Notifier) enqueuePush(ctx context.Context, req Request, delivered []Delivered) {
	if n.pusher == nil || n.devices == nil {
		return
	}

	var msgs []push.Message
	for i := range delivered {
		d := &delivered[i]
		if !d.Created || !d.Push || d.Item == nil {
			continue
		}
		tokens, err := n.devices.PushTokensForUser(ctx, d.UserID)
		if err != nil {
			n.logger.Warn("inbox: list device tokens", "error", err, "user_id", d.UserID)
			continue
		}
		if len(tokens) == 0 {
			continue
		}

		// The badge is the app-icon count: unread ITEMS, resolved server-side so
		// it stays right while the app is not running at all. A failure leaves
		// the existing badge alone rather than clearing it to zero.
		var badge *int
		if count, err := n.repo.UnreadItems(ctx, d.UserID); err != nil {
			n.logger.Warn("inbox: unread count for badge", "error", err, "user_id", d.UserID)
		} else {
			badge = &count
		}

		data := pushData(req, d.Item)
		for _, token := range tokens {
			msgs = append(msgs, push.Message{
				Token: token,
				Title: d.Item.Title,
				Body:  d.Item.Preview,
				Data:  data,
				Badge: badge,
			})
		}
	}
	if len(msgs) > 0 {
		n.pusher.Enqueue(msgs)
	}
}

// pushData is the payload the client receives on tap: enough to route, and
// enough to mark the item read without a round trip.
func pushData(req Request, item *Item) map[string]string {
	data := map[string]string{
		"notification_id": item.ID, // legacy key the shipped client reads
		"item_id":         item.ID,
		"type":            legacyType(item.TopKind),
		"kind":            item.TopKind,
		"subject_type":    item.SubjectType,
		"subject_id":      item.SubjectID,
	}
	for k, v := range req.Data {
		// The synthesised keys win: a channel_id from the payload is what we
		// want, a "type" from it would shadow the notification type.
		if _, taken := data[k]; !taken {
			data[k] = v
		}
	}
	return data
}

// HandleRequested is the durable consumer callback behind FilterRequested — the
// out-of-process half of the entry point.
//
// See internal/notification for the error contract this shares with every other
// consumer in the tree: a plain error means "retry me", a *PermanentError means
// the event is unprocessable and must be terminated rather than redelivered five
// times. Retrying is safe because every event id is derived (EventID) and the
// item upsert is gated on the event insert.
func (n *Notifier) HandleRequested(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	if envelope.Type != EventRequested {
		return nil
	}

	var req Request
	if err := json.Unmarshal(envelope.Data, &req); err != nil {
		return &PermanentError{Reason: "malformed inbox.requested payload", Err: err}
	}
	// The subject is authoritative for tenancy. A payload that names a different
	// workspace from the one it was published on is either a bug or an attempt
	// to file into another tenant's inbox; either way it is not retryable.
	ws, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}
	if req.WorkspaceID == "" {
		req.WorkspaceID = ws
	} else if req.WorkspaceID != ws {
		return &PermanentError{
			Reason: "inbox.requested payload workspace " + req.WorkspaceID + " does not match subject " + msg.Subject,
		}
	}

	if _, err := n.Deliver(ctx, req); err != nil {
		return fmt.Errorf("inbox fan-out for %s: %w", req.Kind, err)
	}
	return nil
}

// Publish is the in-process helper a pillar uses when it is on the API side and
// wants the worker to do the fan-out. It is the same Request the consumer above
// decodes.
func Publish(ctx context.Context, nc *natspkg.Client, req Request) error {
	if nc == nil {
		return fmt.Errorf("inbox: no NATS client")
	}
	if req.WorkspaceID == "" {
		return fmt.Errorf("inbox: workspace_id is required for the subject")
	}
	// The dedupe id is the event's own identity, so a producer that retries the
	// publish collapses into JetStream's duplicate window instead of relying on
	// the database gate alone.
	dedupe := EventID(req.Kind, req.SubjectID, eventKey(req.ObjectID, req.Discriminator))
	return nc.PublishDurable(ctx, SubjectFor(req.WorkspaceID), dedupe,
		natspkg.Event{Type: EventRequested, Data: req})
}

// workspaceFromSubject pulls the workspace id out of superops.{workspace_id}.*.
//
// A subject without one cannot have come from any publisher in this tree, and
// the realtime frame would be addressed to a subject nothing subscribes to. That
// is a permanently broken event, not a transient fault.
func workspaceFromSubject(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) < 2 || parts[1] == "" {
		return "", &PermanentError{Reason: "event subject carries no workspace id: " + subject}
	}
	return parts[1], nil
}

// truncate caps a preview at maxRunes RUNES.
//
// It must not slice by bytes: this deployment is Korean-language, so almost
// every message over 140 bytes would be cut mid-rune, producing invalid UTF-8
// that Postgres rejects on INSERT — and the fan-out would then fail the whole
// event rather than the one field.
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + "..."
		}
		count++
	}
	return s
}
