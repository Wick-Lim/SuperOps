package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/comment"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// The production adapters for the three action ports.
//
// EVERY ONE OF THEM AUTHORIZES AS THE WORKFLOW'S OWNER, before it does
// anything. That is the rule the whole engine rests on:
//
//	YOU CANNOT AUTOMATE WHAT YOU COULD NOT DO BY HAND.
//
// Without it a `post_message` step reaches every channel in the tenant,
// including private ones its author cannot read, and "save a workflow" becomes
// a privilege escalation available to any member. The check is here rather than
// in the executor because each action knows what object it is about to touch;
// a generic pre-check would have to guess.
//
// The owner is also why these are not free functions: each needs the checker.

// ErrUnauthorized is an action whose owner may not do the thing by hand. It is
// a run failure, not a retry: the capability will not appear on its own, and
// retrying would hammer the database on every redelivery.
type ErrUnauthorized struct {
	Owner  string
	Object authz.ObjectRef
	Want   authz.Capability
}

func (e *ErrUnauthorized) Error() string {
	return fmt.Sprintf("the workflow owner may not %s %s", e.Want, e.Object)
}

// authorize is the shared guard. It refuses on error as well as on
// insufficiency: a checker that could not answer must not be read as "yes".
func authorize(ctx context.Context, az *authz.Checker, owner string, obj authz.ObjectRef,
	want authz.Capability) error {
	got, err := az.Capability(ctx, authz.UserSubject(owner), obj)
	if err != nil {
		return fmt.Errorf("authorize workflow owner on %s: %w", obj, err)
	}
	if !got.Implies(want) {
		return &ErrUnauthorized{Owner: owner, Object: obj, Want: want}
	}
	return nil
}

// ---------------------------------------------------------------------------
// post_message
// ---------------------------------------------------------------------------

// MessageCreator is the slice of internal/message this needs. An interface so
// workflow does not import the message package's whole surface, and so the
// dependency runs workflow -> interface <- app.
type MessageCreator interface {
	// CreateFor posts a message attributed to userID and stamps the provenance
	// of the run that produced it, so the trigger consumer can refuse to let it
	// start an infinite cascade.
	CreateFor(ctx context.Context, workspaceID, channelID, userID, body string,
		depth int, rootRunID string) (string, error)
}

type messageAction struct {
	az      *authz.Checker
	creator MessageCreator
}

// NewMessageAction builds the production post_message action.
func NewMessageAction(az *authz.Checker, creator MessageCreator) Action {
	return &messageAction{az: az, creator: creator}
}

func (*messageAction) Kind() string { return StepPostMessage }

func (a *messageAction) Perform(ctx context.Context, run *Run, config map[string]any) (any, error) {
	channelID, _ := config["channel_id"].(string)
	body, _ := config["body"].(string)
	if channelID == "" || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("post_message needs a channel_id and a body")
	}
	// CapWrite on the channel: the same rung that lets the owner type in it.
	if err := authorize(ctx, a.az, run.OwnerID, authz.ChannelObject(channelID), authz.CapWrite); err != nil {
		return nil, err
	}
	id, err := a.creator.CreateFor(ctx, run.WorkspaceID, channelID, run.OwnerID, body,
		run.Depth, run.RootRunID)
	if err != nil {
		return nil, fmt.Errorf("post message: %w", err)
	}
	return map[string]any{"message_id": id, "channel_id": channelID}, nil
}

// ---------------------------------------------------------------------------
// notify
// ---------------------------------------------------------------------------

// InboxPublisher is the notification fan-out. Publishing rather than writing
// directly is deliberate: the inbox owns coalescing, preferences and the digest,
// and a workflow that inserted rows itself would bypass all three — including
// somebody's "never email me" setting.
type InboxPublisher interface {
	PublishNotification(ctx context.Context, workspaceID, recipientID, kind, title, body string) error
}

type notifyProductionAction struct {
	az        *authz.Checker
	publisher InboxPublisher
	pool      *pgxpool.Pool
}

func NewNotifyProductionAction(az *authz.Checker, pool *pgxpool.Pool, p InboxPublisher) Action {
	return &notifyProductionAction{az: az, publisher: p, pool: pool}
}

func (*notifyProductionAction) Kind() string { return StepNotify }

func (a *notifyProductionAction) Perform(ctx context.Context, run *Run, config map[string]any) (any, error) {
	userID, _ := config["user_id"].(string)
	title, _ := config["title"].(string)
	body, _ := config["body"].(string)
	if userID == "" || strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("notify needs a user_id and a title")
	}

	// A user is not an acl_object, so the check is co-membership — the same
	// rule the document mention resolver uses, and the narrowest one available.
	// Without it a workflow could notify every user on the deployment, which is
	// a spam primitive with a legitimate credential.
	var shared bool
	if err := a.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workspace_members m
			 WHERE m.workspace_id = $1 AND m.user_id = $2
		) AND EXISTS (
			SELECT 1 FROM workspace_members m
			 WHERE m.workspace_id = $1 AND m.user_id = $3
		)`, run.WorkspaceID, userID, run.OwnerID).Scan(&shared); err != nil {
		return nil, fmt.Errorf("check notify recipient: %w", err)
	}
	if !shared {
		return nil, &ErrUnauthorized{
			Owner:  run.OwnerID,
			Object: authz.ObjectRef{Type: "user", ID: userID},
			Want:   authz.CapRead,
		}
	}

	if err := a.publisher.PublishNotification(ctx, run.WorkspaceID, userID,
		"workflow.notified", title, body); err != nil {
		return nil, fmt.Errorf("publish notification: %w", err)
	}
	return map[string]any{"notified": userID}, nil
}

// ---------------------------------------------------------------------------
// add_comment
// ---------------------------------------------------------------------------

// CommentCreator is the slice of internal/comment this needs.
type CommentCreator interface {
	Create(ctx context.Context, obj authz.ObjectRef, authorID, parentID, body string) (*comment.Comment, comment.Mentions, error)
}

type commentAction struct {
	az      *authz.Checker
	creator CommentCreator
}

func NewCommentAction(az *authz.Checker, creator CommentCreator) Action {
	return &commentAction{az: az, creator: creator}
}

func (*commentAction) Kind() string { return StepAddComment }

func (a *commentAction) Perform(ctx context.Context, run *Run, config map[string]any) (any, error) {
	objectType, _ := config["object_type"].(string)
	objectID, _ := config["object_id"].(string)
	body, _ := config["body"].(string)
	if objectType == "" || objectID == "" || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("add_comment needs an object_type, an object_id and a body")
	}
	obj := authz.ObjectRef{Type: objectType, ID: objectID}
	// CapComment — the rung that exists for exactly this: discussing a thing
	// you may not change.
	if err := authorize(ctx, a.az, run.OwnerID, obj, authz.CapComment); err != nil {
		return nil, err
	}
	c, _, err := a.creator.Create(ctx, obj, run.OwnerID, "", body)
	if err != nil {
		return nil, fmt.Errorf("add comment: %w", err)
	}
	return map[string]any{"comment_id": c.ID}, nil
}

// ---------------------------------------------------------------------------
// The inbox publisher
// ---------------------------------------------------------------------------

// NATSInbox publishes through inbox.Publish rather than writing rows.
//
// That is deliberate: the inbox owns coalescing, per-user preferences and the
// digest, and a workflow that inserted notification rows directly would bypass
// all three — including somebody's "never email me" setting, which is the one
// nobody notices is broken until it has been broken for a week.
//
// The dedupe id is derived by inbox.Publish from (kind, subject, object), so a
// redelivered trigger that legitimately reaches a second run coalesces onto the
// same item instead of producing a second badge. The effects table already
// covers a repeat WITHIN one run; this covers across runs.
type NATSInbox struct {
	Client *natspkg.Client
}

func (n NATSInbox) PublishNotification(ctx context.Context, workspaceID, recipientID, kind, title, body string) error {
	if n.Client == nil {
		return fmt.Errorf("workflow: no message bus configured, so nothing can be notified")
	}
	return inbox.Publish(ctx, n.Client, inbox.Request{
		WorkspaceID: workspaceID,
		Kind:        "workflow.notified",
		// The object is the notification itself; there is no richer object to
		// point at, and inventing one would make the inbox link somewhere the
		// user cannot go.
		ObjectType:  "workflow",
		ObjectID:    workspaceID,
		SubjectType: "workflow",
		SubjectID:   workspaceID,
		Title:       title,
		Body:        body,
		// Distinct per recipient and per title, or two different workflow
		// notifications to the same person would collapse into one.
		Discriminator: recipientID + ":" + title,
		Recipients:    []string{recipientID},
	})
}
