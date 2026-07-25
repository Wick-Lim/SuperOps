// Package comment is the product-wide comment surface.
//
// ONE comment system, and it is the first consumer that decides nothing.
// docs/plans/02-drive.md §13 cut per-file comments explicitly — "Phase 2 builds
// the comment surface once and Drive adopts it" — because two comment systems is
// the failure the whole all-in-one thesis is written against. So this package
// knows about acl_object and nothing else: it has never heard of an issue, a
// file or a document.
//
// A COMMENT IS AUTHORIZED BY ITS TARGET, with no rule of its own. "May I read
// this comment" is "may I read the thing it is about", which internal/authz
// already answers, and the capability ladder already has the rung the write side
// needs: CapComment sits between read and write precisely so a person can be
// given the ability to discuss something without the ability to change it.
package comment

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

var (
	// ErrNotFound is a comment that does not exist, or one whose target the
	// caller cannot read. The handler answers 404 for both: a 403 on something
	// you cannot see confirms it exists.
	ErrNotFound = errors.New("comment: not found")

	// ErrNotAuthor is an edit or delete by somebody who did not write it and
	// does not administer the target.
	ErrNotAuthor = errors.New("comment: only the author or an administrator can change this")

	// ErrDeleted is an edit of a comment that has been deleted. Its body is
	// genuinely gone, so there is nothing to edit.
	ErrDeleted = errors.New("comment: this comment was deleted")

	// ErrNested is a reply to a reply. Threading is one level deep.
	ErrNested = errors.New("comment: replies cannot themselves be replied to")

	// ErrEmpty is a body of nothing but whitespace.
	ErrEmpty = errors.New("comment: a comment needs some text")
)

// MaxBody mirrors the CHECK constraint. Stated here so the handler can refuse
// with a message rather than surfacing a constraint violation as a 500.
const MaxBody = 16000

// Comment is one comment.
type Comment struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	ObjectType  string  `json:"object_type"`
	ObjectID    string  `json:"object_id"`
	AuthorID    *string `json:"author_id"`
	ParentID    *string `json:"parent_id"`

	// Body is empty for a deleted comment, and Deleted says why. The row
	// survives because the replies under it still make sense; a thread that
	// silently loses its first message reads as corruption.
	Body    string `json:"body"`
	Deleted bool   `json:"deleted"`

	EditedAt  *time.Time `json:"edited_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// ReplyCount is filled for top-level comments in a thread listing so a
	// client can render "3 replies" without a second query per row.
	ReplyCount int `json:"reply_count"`
}

// Mentions is what a producer needs to fan out to the inbox.
//
// It is returned by Create rather than computed by the caller, because the
// extraction has to agree with what the client renders as a mention — two
// implementations of "what is a mention" is two different sets of people
// notified.
type Mentions struct {
	UserIDs []string
}

// mentionPattern matches <@uuid>, the same encoding internal/message uses.
//
// A raw "@name" is deliberately NOT matched: names are not unique, they change,
// and resolving one at read time means a comment's meaning depends on who is
// called what today.
var mentionPattern = regexp.MustCompile(
	`<@([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})>`)

// ExtractMentions returns the distinct users named in a body, in order of first
// appearance.
func ExtractMentions(body string) []string {
	matches := mentionPattern.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		id := strings.ToLower(m[1])
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Repository owns the comments table.
type Repository struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
}

func NewRepository(pool *pgxpool.Pool, az *authz.Checker) *Repository {
	return &Repository{pool: pool, authz: az}
}

const columns = `c.id::text, c.workspace_id::text, c.object_type, c.object_id::text,
	c.author_id::text, c.parent_id::text,
	CASE WHEN c.deleted_at IS NULL THEN c.body ELSE '' END,
	c.deleted_at IS NOT NULL,
	c.edited_at, c.created_at, c.updated_at`

func scan(row pgx.Row) (*Comment, error) {
	var c Comment
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.ObjectType, &c.ObjectID,
		&c.AuthorID, &c.ParentID, &c.Body, &c.Deleted,
		&c.EditedAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan comment: %w", err)
	}
	return &c, nil
}

// Thread lists an object's comments, oldest first — which is how a conversation
// reads, and the opposite of every other listing in this product.
//
// Top-level comments only, each with its reply count. The replies themselves are
// fetched per comment: a thread with one 400-reply argument in it should not make
// every other reader page through it to reach the next topic.
func (r *Repository) Thread(ctx context.Context, obj authz.ObjectRef, page httputil.PaginationParams) ([]Comment, string, bool, error) {
	limit := page.Limit + 1

	var (
		cursorAt *time.Time
		cursorID string
	)
	if !page.Cursor.IsZero() {
		cursorAt, cursorID = &page.Cursor.CreatedAt, page.Cursor.ID
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+columns+`,
		       (SELECT count(*) FROM comments rc WHERE rc.parent_id = c.id)::int
		  FROM comments c
		 WHERE c.object_type = $1 AND c.object_id = $2
		   AND c.parent_id IS NULL
		   AND ($3::timestamptz IS NULL OR (c.created_at, c.id) > ($3::timestamptz, $4::uuid))
		 ORDER BY c.created_at ASC, c.id ASC
		 LIMIT $5`,
		obj.Type, obj.ID, cursorAt, nullableUUID(cursorID), limit)
	if err != nil {
		return nil, "", false, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()

	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.ObjectType, &c.ObjectID,
			&c.AuthorID, &c.ParentID, &c.Body, &c.Deleted,
			&c.EditedAt, &c.CreatedAt, &c.UpdatedAt, &c.ReplyCount); err != nil {
			return nil, "", false, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, fmt.Errorf("list comments: %w", err)
	}

	hasMore := len(out) > page.Limit
	if hasMore {
		out = out[:page.Limit]
	}
	cursor := ""
	if hasMore && len(out) > 0 {
		last := out[len(out)-1]
		cursor = httputil.EncodeCursor(last.CreatedAt, last.ID)
	}
	return out, cursor, hasMore, nil
}

// Replies lists one comment's replies. Not paginated: threading is one level
// deep and a reply list is bounded by how many people answered one thing.
func (r *Repository) Replies(ctx context.Context, parentID string) ([]Comment, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+columns+` FROM comments c WHERE c.parent_id = $1 ORDER BY c.created_at ASC`,
		parentID)
	if err != nil {
		return nil, fmt.Errorf("list replies: %w", err)
	}
	defer rows.Close()

	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.ObjectType, &c.ObjectID,
			&c.AuthorID, &c.ParentID, &c.Body, &c.Deleted,
			&c.EditedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan reply: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repository) Get(ctx context.Context, id string) (*Comment, error) {
	return scan(r.pool.QueryRow(ctx, `SELECT `+columns+` FROM comments c WHERE c.id = $1`, id))
}

// Create writes a comment and returns it with the mentions it carries.
//
// The workspace is read from acl_object rather than taken from the caller: the
// target's workspace is the authority, and a comment whose workspace disagreed
// with its object's would be visible to a tenancy filter that scans comments and
// invisible to one that joins acl_object. The trigger enforces the same thing —
// this is the cheap check, that is the one that cannot be forgotten.
func (r *Repository) Create(ctx context.Context, obj authz.ObjectRef, authorID, parentID, body string) (*Comment, Mentions, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, Mentions{}, ErrEmpty
	}
	if len([]rune(body)) > MaxBody {
		return nil, Mentions{}, fmt.Errorf("comment: a comment may be at most %d characters", MaxBody)
	}

	var out *Comment
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var workspaceID string
		err := tx.QueryRow(ctx,
			`SELECT workspace_id::text FROM acl_object WHERE object_type = $1 AND object_id = $2`,
			obj.Type, obj.ID).Scan(&workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("resolve comment target: %w", err)
		}

		out, err = scan(tx.QueryRow(ctx, `
			INSERT INTO comments (workspace_id, object_type, object_id, author_id, parent_id, body)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+strings.ReplaceAll(columns, "c.", "comments."),
			workspaceID, obj.Type, obj.ID, nullableUUID(authorID), nullableUUID(parentID), body))
		if err != nil {
			// The trigger's refusals are user errors, not server faults: a reply
			// to a reply is something a client can do by mistake.
			if strings.Contains(err.Error(), "nest one level") {
				return ErrNested
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, Mentions{}, err
	}
	return out, Mentions{UserIDs: ExtractMentions(body)}, nil
}

// Update edits a body. Only the author, or somebody with admin on the target.
func (r *Repository) Update(ctx context.Context, id, actorID, body string, actorIsAdmin bool) (*Comment, Mentions, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, Mentions{}, ErrEmpty
	}
	if len([]rune(body)) > MaxBody {
		return nil, Mentions{}, fmt.Errorf("comment: a comment may be at most %d characters", MaxBody)
	}

	existing, err := r.Get(ctx, id)
	if err != nil {
		return nil, Mentions{}, err
	}
	if existing == nil {
		return nil, Mentions{}, ErrNotFound
	}
	if existing.Deleted {
		return nil, Mentions{}, ErrDeleted
	}
	if !actorIsAdmin && (existing.AuthorID == nil || *existing.AuthorID != actorID) {
		return nil, Mentions{}, ErrNotAuthor
	}

	out, err := scan(r.pool.QueryRow(ctx, `
		UPDATE comments SET body = $2, edited_at = NOW() WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+strings.ReplaceAll(columns, "c.", "comments."), id, body))
	if err != nil {
		return nil, Mentions{}, err
	}
	if out == nil {
		return nil, Mentions{}, ErrNotFound
	}

	// Only the NEWLY mentioned are notified — see the handler. Returning every
	// mention and letting the caller diff keeps that decision in one place.
	return out, Mentions{UserIDs: ExtractMentions(body)}, nil
}

// Delete blanks a comment and leaves the tombstone.
//
// Soft, and the body is genuinely cleared rather than merely hidden: "deleted"
// has to mean the text is gone, or the feature is a lie the first time somebody
// reads the table. The row survives to hold the replies beneath it.
func (r *Repository) Delete(ctx context.Context, id, actorID string, actorIsAdmin bool) error {
	existing, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotFound
	}
	if existing.Deleted {
		return nil // Idempotent: a double tap is not an error.
	}
	if !actorIsAdmin && (existing.AuthorID == nil || *existing.AuthorID != actorID) {
		return ErrNotAuthor
	}

	_, err = r.pool.Exec(ctx, `
		UPDATE comments
		   SET body = '', deleted_at = NOW(), deleted_by = $2
		 WHERE id = $1 AND deleted_at IS NULL`, id, nullableUUID(actorID))
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}

// CountFor returns how many live comments an object has, for a badge.
func (r *Repository) CountFor(ctx context.Context, obj authz.ObjectRef) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM comments
		  WHERE object_type = $1 AND object_id = $2 AND deleted_at IS NULL`,
		obj.Type, obj.ID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count comments: %w", err)
	}
	return n, nil
}

func nullableUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
