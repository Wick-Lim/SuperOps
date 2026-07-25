package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// messageColumns is the canonical column list (and scan order) for a message
// row. It must stay in lockstep with scanArgs — scanning is positional.
const messageColumns = `id, channel_id, user_id, parent_id, content, content_type,
	is_edited, is_deleted, reply_count, is_pinned, pinned_by, pinned_at,
	scheduled_at, is_scheduled, metadata, created_at, updated_at`

// liveOnly restricts a read (or a state transition) to rows that are actually
// part of the conversation. Soft-deleted rows survive for thread integrity and
// scheduled rows have not been sent yet; neither may be served to, or mutated
// by, a channel member.
const liveOnly = ` AND is_deleted = FALSE AND is_scheduled = FALSE`

var (
	// ErrInvalidParent is returned when a reply targets a message that is not a
	// live, top-level message of the same channel.
	ErrInvalidParent = errors.New("parent must be a top-level message in the same channel")

	// ErrFilesUnavailable is returned when a file id does not belong to the
	// caller, does not exist, or is already attached to another message.
	ErrFilesUnavailable = errors.New("file is not available to attach")

	// ErrMessageNotFound is returned by message-addressed writes whose target
	// row does not exist in the expected channel.
	ErrMessageNotFound = errors.New("message not found")
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// Create inserts a message and, atomically in the same transaction, validates
// and bumps the parent reply_count (for thread replies), attaches the caller's
// files and touches the channel's last_message_at. File linking used to run
// after the transaction with its error discarded, so a 201 could be returned
// for a message whose attachments never attached.
func (r *Repository) Create(ctx context.Context, m *Message, fileIDs []string) error {
	meta, err := json.Marshal(m.Metadata)
	if err != nil {
		return fmt.Errorf("encode message metadata: %w", err)
	}

	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		// The reply_count bump doubles as parent validation: its predicate is
		// exactly "live, top-level, same channel", so an invalid parent fails
		// here rather than as an opaque FK violation (or, worse, silently
		// inflating a foreign thread's counter).
		if m.ParentID != nil {
			tag, err := tx.Exec(ctx,
				`UPDATE messages SET reply_count = reply_count + 1
				  WHERE id = $1 AND channel_id = $2 AND parent_id IS NULL`+liveOnly,
				*m.ParentID, m.ChannelID)
			if err != nil {
				return fmt.Errorf("increment reply count: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrInvalidParent
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO messages (id, channel_id, user_id, parent_id, content, content_type, metadata)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			m.ID, m.ChannelID, m.UserID, m.ParentID, m.Content, m.ContentType, meta,
		); err != nil {
			return fmt.Errorf("create message: %w", err)
		}

		if err := linkFiles(ctx, tx, m.ID, m.UserID, fileIDs); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE channels SET last_message_at = NOW() WHERE id = $1`, m.ChannelID,
		); err != nil {
			return fmt.Errorf("update last message: %w", err)
		}
		return nil
	})
}

// CreateScheduled inserts a message in the scheduled (not-yet-sent) state. It
// does not bump channel last_message_at or reply_count — the worker does that
// when the message becomes due — but it validates the parent on the same terms
// as Create so a pending reply cannot land in a foreign thread.
func (r *Repository) CreateScheduled(ctx context.Context, m *Message, scheduledAt time.Time, fileIDs []string) error {
	meta, err := json.Marshal(m.Metadata)
	if err != nil {
		return fmt.Errorf("encode message metadata: %w", err)
	}

	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if m.ParentID != nil {
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM messages
				   WHERE id = $1 AND channel_id = $2 AND parent_id IS NULL`+liveOnly+`)`,
				*m.ParentID, m.ChannelID,
			).Scan(&ok); err != nil {
				return fmt.Errorf("check parent message: %w", err)
			}
			if !ok {
				return ErrInvalidParent
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO messages (id, channel_id, user_id, parent_id, content, content_type, metadata, is_scheduled, scheduled_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, $8)`,
			m.ID, m.ChannelID, m.UserID, m.ParentID, m.Content, m.ContentType, meta, scheduledAt,
		); err != nil {
			return fmt.Errorf("create scheduled message: %w", err)
		}

		return linkFiles(ctx, tx, m.ID, m.UserID, fileIDs)
	})
}

// linkFiles attaches the caller's previously-uploaded files to a message. Every
// requested id must actually be linkable: a partial match means the caller
// referenced someone else's upload (or one already attached elsewhere), which
// is a bad request, not a silently smaller message.
func linkFiles(ctx context.Context, tx pgx.Tx, messageID, userID string, fileIDs []string) error {
	ids := dedupe(fileIDs)
	if len(ids) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx,
		`UPDATE files SET message_id = $1 WHERE id = ANY($2) AND user_id = $3 AND message_id IS NULL`,
		messageID, ids, userID)
	if err != nil {
		return fmt.Errorf("link files: %w", err)
	}
	if int(tag.RowsAffected()) != len(ids) {
		return ErrFilesUnavailable
	}
	return nil
}

func dedupe(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Update rewrites the body of a live message owned by the caller. Ownership is
// part of the predicate rather than a preceding SELECT, so a concurrent delete
// cannot slip between the check and the write. updated_at is maintained by the
// trg_messages_updated_at trigger (migration 009).
func (r *Repository) Update(ctx context.Context, id, userID, content string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE messages SET content = $3, is_edited = TRUE
		  WHERE id = $1 AND user_id = $2 AND is_deleted = FALSE`,
		id, userID, content)
	if err != nil {
		return false, fmt.Errorf("update message: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SoftDelete blanks a message in place and, in the same transaction, gives the
// parent thread its reply_count back. Create incremented that counter, so a
// delete that skipped the decrement made thread counters drift upward forever.
//
// Scheduled rows are out of scope on purpose: they have not been counted yet
// (cmd/worker bumps the parent at promotion) and a soft-deleted row that is
// still is_scheduled would be promoted by the worker as an empty message.
// Cancelling is the operation for those — see CancelScheduled.
func (r *Repository) SoftDelete(ctx context.Context, id, channelID string) (bool, error) {
	var deleted bool
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var parentID *string
		err := tx.QueryRow(ctx,
			`UPDATE messages SET is_deleted = TRUE, content = ''
			  WHERE id = $1 AND channel_id = $2`+liveOnly+`
			  RETURNING parent_id`,
			id, channelID,
		).Scan(&parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("soft delete message: %w", err)
		}
		deleted = true

		if parentID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE messages SET reply_count = GREATEST(reply_count - 1, 0) WHERE id = $1`,
				*parentID,
			); err != nil {
				return fmt.Errorf("decrement reply count: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// SetPinned pins or unpins a live message. The channel is part of the
// predicate: pinning is addressed by message id, and without this a member of
// any channel could pin — and thereby broadcast — a message from any other.
func (r *Repository) SetPinned(ctx context.Context, id, channelID, pinnedBy string, pinned bool) (bool, error) {
	query := `UPDATE messages SET is_pinned = FALSE, pinned_by = NULL, pinned_at = NULL
	           WHERE id = $1 AND channel_id = $2` + liveOnly
	args := []any{id, channelID}
	if pinned {
		query = `UPDATE messages SET is_pinned = TRUE, pinned_by = $3, pinned_at = NOW()
		          WHERE id = $1 AND channel_id = $2` + liveOnly
		args = append(args, pinnedBy)
	}

	tag, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("set pinned: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CancelScheduled deletes a not-yet-sent scheduled message owned by the caller
// in the given channel. reply_count is deliberately untouched: a scheduled
// reply has not been counted yet (cmd/worker bumps the parent at promotion).
func (r *Repository) CancelScheduled(ctx context.Context, messageID, channelID, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM messages
		  WHERE id = $1 AND channel_id = $2 AND user_id = $3 AND is_scheduled = TRUE`,
		messageID, channelID, userID)
	if err != nil {
		return false, fmt.Errorf("cancel scheduled: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetByID returns a live message: soft-deleted and not-yet-sent scheduled rows
// are not part of the conversation and must never be served by id.
func (r *Repository) GetByID(ctx context.Context, id string) (*Message, error) {
	return r.getOne(ctx, `WHERE id = $1`+liveOnly, id)
}

// GetForUser is GetByID plus the caller's own pending scheduled messages — the
// author may read what they scheduled, nobody else may.
func (r *Repository) GetForUser(ctx context.Context, id, userID string) (*Message, error) {
	return r.getOne(ctx,
		`WHERE id = $1 AND is_deleted = FALSE AND (is_scheduled = FALSE OR user_id = $2)`,
		id, userID)
}

func (r *Repository) getOne(ctx context.Context, where string, args ...any) (*Message, error) {
	m := &Message{}
	err := r.pool.QueryRow(ctx,
		`SELECT `+messageColumns+` FROM messages `+where, args...,
	).Scan(scanArgs(m)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}

// paginate closes a list query with its keyset predicate, ordering and limit.
//
// The predicate compares the row (timeCol, idCol) as a tuple. Comparing the
// timestamp alone is not a total order, so at a page boundary every row but
// one sharing a timestamp was silently skipped — which the scheduled-message
// promotion produces by design, stamping a whole batch with a single NOW().
//
// asc selects the direction: ">" / ASC for threads and scheduled queues that
// read oldest-first, "<" / DESC for the timeline, pins and bookmarks.
func paginate(base string, args []any, timeCol, idCol string, asc bool, cur httputil.Cursor, limit int) (string, []any) {
	cmp, dir := "<", "DESC"
	if asc {
		cmp, dir = ">", "ASC"
	}

	// Copy rather than append in place: the caller's slice must not be written
	// through if it happens to have spare capacity.
	out := make([]any, len(args), len(args)+3)
	copy(out, args)

	if !cur.IsZero() {
		base += keysetPredicate(timeCol, idCol, cmp, len(out)+1, len(out)+2)
		out = append(out, cur.CreatedAt, cur.ID)
	}
	base += fmt.Sprintf(" ORDER BY %s %s, %s %s LIMIT $%d", timeCol, dir, idCol, dir, len(out)+1)
	return base, append(out, limit)
}

func keysetPredicate(timeCol, idCol, cmp string, timeIdx, idIdx int) string {
	return fmt.Sprintf(" AND (%s, %s) %s ($%d, $%d)", timeCol, idCol, cmp, timeIdx, idIdx)
}

// ListByChannel returns the newest live top-level messages of a channel.
func (r *Repository) ListByChannel(ctx context.Context, channelID string, cur httputil.Cursor, limit int) ([]*Message, error) {
	query, args := paginate(
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE channel_id = $1 AND parent_id IS NULL`+liveOnly,
		[]any{channelID}, "created_at", "id", false, cur, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

// ListThread returns a thread's replies oldest-first. It was capped at a hard
// LIMIT 100 with no cursor, so the 101st reply was unreachable through the API.
func (r *Repository) ListThread(ctx context.Context, parentID string, cur httputil.Cursor, limit int) ([]*Message, error) {
	query, args := paginate(
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE parent_id = $1`+liveOnly,
		[]any{parentID}, "created_at", "id", true, cur, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list thread: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

// ListPinned returns the pinned messages of a channel, most recently pinned
// first. The cursor is keyed on (pinned_at, id) — the list's own ordering.
func (r *Repository) ListPinned(ctx context.Context, channelID string, cur httputil.Cursor, limit int) ([]*Message, error) {
	query, args := paginate(
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE channel_id = $1 AND is_pinned = TRUE`+liveOnly,
		[]any{channelID}, "pinned_at", "id", false, cur, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pinned: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

// ListScheduled returns a user's own pending scheduled messages in a channel,
// soonest first. The cursor is keyed on (scheduled_at, id).
func (r *Repository) ListScheduled(ctx context.Context, channelID, userID string, cur httputil.Cursor, limit int) ([]*Message, error) {
	query, args := paginate(
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE channel_id = $1 AND user_id = $2 AND is_scheduled = TRUE AND is_deleted = FALSE`,
		[]any{channelID, userID}, "scheduled_at", "id", true, cur, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list scheduled: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

// ---------------------------------------------------------------------------
// Bookmarks
// ---------------------------------------------------------------------------

func (r *Repository) AddBookmark(ctx context.Context, userID, messageID string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO bookmarks (user_id, message_id) VALUES ($1, $2)
		 ON CONFLICT (user_id, message_id) DO NOTHING`, userID, messageID)
	if err != nil {
		return fmt.Errorf("add bookmark: %w", err)
	}
	return nil
}

// RemoveBookmark deletes the caller's own bookmark. Scoping by user_id is the
// authorization: the previous handler deleted by message id for whoever asked.
func (r *Repository) RemoveBookmark(ctx context.Context, userID, messageID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM bookmarks WHERE user_id = $1 AND message_id = $2`, userID, messageID)
	if err != nil {
		return false, fmt.Errorf("remove bookmark: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListBookmarks returns the caller's saved messages, newest save first.
//
// Bookmarking a message does not grant permanent access to it: the join
// re-applies channel membership on every read, so a user removed from a
// channel stops seeing its content even though the bookmark row survives. The
// membership predicate mirrors authz.Checker's channel-membership rung, which is the
// gate every other read in this package uses; it lives in SQL here only
// because the filter has to run before LIMIT for has_more to be correct.
func (r *Repository) ListBookmarks(ctx context.Context, userID string, cur httputil.Cursor, limit int) ([]*Bookmark, error) {
	query, args := paginate(
		`SELECT b.created_at,`+prefixed(messageColumns, "m")+`
		 FROM bookmarks b
		 JOIN messages m ON m.id = b.message_id
		 WHERE b.user_id = $1
		   AND m.is_deleted = FALSE AND m.is_scheduled = FALSE
		   AND EXISTS (SELECT 1 FROM channel_members cm
		                WHERE cm.channel_id = m.channel_id AND cm.user_id = $1)`,
		// The tiebreaker is the bookmarked message id (unique per user by the
		// bookmarks UNIQUE(user_id, message_id)), because that is the id the
		// handler has when it encodes the cursor.
		[]any{userID}, "b.created_at", "b.message_id", false, cur, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list bookmarks: %w", err)
	}
	defer rows.Close()

	bookmarks := []*Bookmark{}
	for rows.Next() {
		b := &Bookmark{Message: &Message{}}
		if err := rows.Scan(append([]any{&b.BookmarkedAt}, scanArgs(b.Message)...)...); err != nil {
			return nil, fmt.Errorf("scan bookmark: %w", err)
		}
		bookmarks = append(bookmarks, b)
	}
	return bookmarks, rows.Err()
}

// prefixed qualifies every column of a comma-separated list with a table alias
// so messageColumns stays the single source of truth in joined queries.
func prefixed(columns, alias string) string {
	out := make([]byte, 0, len(columns)*2)
	for i, col := range strings.Split(columns, ",") {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, ' ')
		out = append(out, alias...)
		out = append(out, '.')
		out = append(out, strings.TrimSpace(col)...)
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// AddReaction inserts a reaction on a live message of the given channel and
// returns the row that actually exists, together with whether this call
// created it. The row is read back rather than echoed: the previous version
// returned a fabricated object whose created_at was the zero time and whose id
// belonged to no row at all when the insert hit a conflict.
func (r *Repository) AddReaction(ctx context.Context, reaction *Reaction, channelID string) (*Reaction, bool, error) {
	out := &Reaction{}
	err := r.pool.QueryRow(ctx,
		`INSERT INTO reactions (id, message_id, user_id, emoji)
		 SELECT $1, $2, $3, $4
		  WHERE EXISTS (SELECT 1 FROM messages WHERE id = $2 AND channel_id = $5`+liveOnly+`)
		 ON CONFLICT (message_id, user_id, emoji) DO NOTHING
		 RETURNING id, message_id, user_id, emoji, created_at`,
		reaction.ID, reaction.MessageID, reaction.UserID, reaction.Emoji, channelID,
	).Scan(&out.ID, &out.MessageID, &out.UserID, &out.Emoji, &out.CreatedAt)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("add reaction: %w", err)
	}

	// Nothing was inserted: either the caller had already reacted with this
	// emoji, or the message is not a live message of this channel.
	err = r.pool.QueryRow(ctx,
		`SELECT re.id, re.message_id, re.user_id, re.emoji, re.created_at
		   FROM reactions re
		   JOIN messages m ON m.id = re.message_id
		  WHERE re.message_id = $1 AND re.user_id = $2 AND re.emoji = $3 AND m.channel_id = $4`,
		reaction.MessageID, reaction.UserID, reaction.Emoji, channelID,
	).Scan(&out.ID, &out.MessageID, &out.UserID, &out.Emoji, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrMessageNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("get reaction: %w", err)
	}
	return out, false, nil
}

// RemoveReaction deletes the caller's reaction, scoped to the message's own
// channel.
func (r *Repository) RemoveReaction(ctx context.Context, messageID, channelID, userID, emoji string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM reactions re
		  USING messages m
		  WHERE m.id = re.message_id
		    AND re.message_id = $1 AND re.user_id = $2 AND re.emoji = $3
		    AND m.channel_id = $4`,
		messageID, userID, emoji, channelID,
	)
	if err != nil {
		return false, fmt.Errorf("remove reaction: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) ListReactions(ctx context.Context, messageID, channelID string) ([]*Reaction, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT re.id, re.message_id, re.user_id, re.emoji, re.created_at
		   FROM reactions re
		   JOIN messages m ON m.id = re.message_id
		  WHERE re.message_id = $1 AND m.channel_id = $2
		  ORDER BY re.created_at, re.id`, messageID, channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("list reactions: %w", err)
	}
	defer rows.Close()

	reactions := []*Reaction{}
	for rows.Next() {
		re := &Reaction{}
		if err := rows.Scan(&re.ID, &re.MessageID, &re.UserID, &re.Emoji, &re.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan reaction: %w", err)
		}
		reactions = append(reactions, re)
	}
	return reactions, rows.Err()
}

// ---------------------------------------------------------------------------
// Hydration
// ---------------------------------------------------------------------------

// Hydrate attaches both reactions and files to the given messages.
func (r *Repository) Hydrate(ctx context.Context, messages []*Message) error {
	if err := r.hydrateReactions(ctx, messages); err != nil {
		return err
	}
	return r.hydrateFiles(ctx, messages)
}

// hydrateReactions attaches reactions to the given messages with a single
// query (avoids N+1).
func (r *Repository) hydrateReactions(ctx context.Context, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}

	rows, err := r.pool.Query(ctx,
		`SELECT id, message_id, user_id, emoji, created_at
		   FROM reactions WHERE message_id = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return fmt.Errorf("hydrate reactions: %w", err)
	}
	defer rows.Close()

	var found []*Reaction
	for rows.Next() {
		re := &Reaction{}
		if err := rows.Scan(&re.ID, &re.MessageID, &re.UserID, &re.Emoji, &re.CreatedAt); err != nil {
			return fmt.Errorf("scan reaction: %w", err)
		}
		found = append(found, re)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	attachReactions(messages, found)
	return nil
}

// hydrateFiles attaches file metadata to messages with a single query.
func (r *Repository) hydrateFiles(ctx context.Context, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, fileSourceID(m))
	}

	rows, err := r.pool.Query(ctx,
		`SELECT id, message_id, name, content_type, size_bytes FROM files WHERE message_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("hydrate files: %w", err)
	}
	defer rows.Close()

	var found []attachedFile
	for rows.Next() {
		af := attachedFile{Ref: &FileRef{}}
		if err := rows.Scan(&af.Ref.ID, &af.MessageID, &af.Ref.Name, &af.Ref.ContentType, &af.Ref.SizeBytes); err != nil {
			return fmt.Errorf("scan file: %w", err)
		}
		found = append(found, af)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	attachFiles(messages, found)
	return nil
}

type attachedFile struct {
	MessageID string
	Ref       *FileRef
}

// fileSourceID is the message whose attachments a message displays. A forward
// shows the attachments of the message it copied instead of duplicating the
// file rows (which would share a storage object with an independent lifetime).
func fileSourceID(m *Message) string {
	if m.Metadata.ForwardedFrom != nil && m.Metadata.ForwardedFrom.MessageID != "" {
		return m.Metadata.ForwardedFrom.MessageID
	}
	return m.ID
}

// attachReactions distributes reaction rows over their messages. Every message
// ends up with a non-nil slice: `reactions` must serialize as [] rather than
// disappearing from the payload.
func attachReactions(messages []*Message, reactions []*Reaction) {
	byID := make(map[string][]*Message, len(messages))
	for _, m := range messages {
		m.Reactions = []*Reaction{}
		byID[m.ID] = append(byID[m.ID], m)
	}
	for _, re := range reactions {
		for _, m := range byID[re.MessageID] {
			m.Reactions = append(m.Reactions, re)
		}
	}
}

// attachFiles distributes file rows over the messages that display them, keyed
// on fileSourceID so a forwarded copy carries the original's attachments.
func attachFiles(messages []*Message, files []attachedFile) {
	bySource := make(map[string][]*Message, len(messages))
	for _, m := range messages {
		m.Files = []*FileRef{}
		bySource[fileSourceID(m)] = append(bySource[fileSourceID(m)], m)
	}
	for _, af := range files {
		for _, m := range bySource[af.MessageID] {
			m.Files = append(m.Files, af.Ref)
		}
	}
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

// scanArgs is the positional destination list for messageColumns. Change one
// and you must change the other.
func scanArgs(m *Message) []any {
	return []any{
		&m.ID, &m.ChannelID, &m.UserID, &m.ParentID, &m.Content, &m.ContentType,
		&m.IsEdited, &m.IsDeleted, &m.ReplyCount, &m.IsPinned, &m.PinnedBy, &m.PinnedAt,
		&m.ScheduledAt, &m.IsScheduled, &m.Metadata, &m.CreatedAt, &m.UpdatedAt,
	}
}

func (r *Repository) scanMessages(rows pgx.Rows) ([]*Message, error) {
	messages := []*Message{}
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(scanArgs(m)...); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}
