package mailbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Attachments.
//
// A MAIL ATTACHMENT IS A DRIVE FILE. Not a second file story with a second
// quota story, a second garbage-collection story and a second permission story
// — `files.mail_message_id` and the `acl_object_expected` conversation arm both
// exist to make one story serve both, and until this file was written nothing
// populated the column, so both were protecting a relation nothing established.
//
// THE MIME PARSING IS THE PROVIDER'S. Every inbound provider — SendGrid,
// Mailgun, Postmark, SES — already parses the message and hands back decoded
// parts, because it has to in order to run its own spam and virus checks.
// Re-parsing the raw RFC822 ourselves would mean owning a MIME parser, its
// encoding edge cases and its decompression-bomb surface, to arrive at the same
// answer. The raw original is still archived under `raw_key`, so nothing is
// lost if a provider's parse is ever in doubt.

// Attachment limits. Every one is a refusal, not a truncation: half an
// attachment is a corrupt file that looks like a real one.
const (
	// One attachment. Larger than any sane email and small enough that a
	// hostile sender cannot exhaust memory through the ingest route.
	maxAttachmentBytes = 25 << 20
	// Per message. Beyond this the sender is using email as a file transfer,
	// and the raw original is archived anyway.
	maxAttachmentsPerMessage = 20
)

// InboundAttachment is one decoded part, as the provider hands it over.
type InboundAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	// Content is base64, which is how it arrived on the wire and how every
	// provider sends it. Decoded once, here.
	Content string `json:"content"`
}

// storeAttachments writes decoded parts as Drive files owned by a mail message.
//
// Owned by the MESSAGE, not by a folder: an attachment is part of a
// conversation, and putting it in somebody's Drive would make it appear in a
// file browser as though a person had uploaded it. The acl_object path runs
// through the conversation, so an agent granted on the mailbox can open it and
// nobody else can — with no per-attachment grant to keep in sync.
func (r *Repository) storeAttachments(ctx context.Context, store storage.Backend,
	workspaceID, messageID, actorID string, parts []InboundAttachment) (int, error) {

	if store == nil || len(parts) == 0 {
		return 0, nil
	}

	// files.user_id is NOT NULL and an inbound email has no person behind it.
	//
	// Attributed to whoever CREATED THE MAILBOX rather than to a synthetic
	// account: the product has no machine principal, inventing one would put a
	// row in `users` that nothing else understands, and the mailbox's creator
	// is the closest true answer — they are the person who decided this address
	// would receive files.
	if actorID == "" {
		if err := r.pool.QueryRow(ctx, `
			SELECT COALESCE(mb.created_by::text, '')
			  FROM mail_messages m
			  JOIN mail_conversations c ON c.id = m.conversation_id
			  JOIN mailboxes mb ON mb.id = c.mailbox_id
			 WHERE m.id = $1`, messageID).Scan(&actorID); err != nil {
			return 0, fmt.Errorf("resolve attachment owner: %w", err)
		}
	}
	if actorID == "" {
		// A mailbox whose creator has been deleted. Refused rather than
		// attributed to nobody: an unattributed file is one the offboarding
		// surface cannot account for.
		return 0, errors.New("mailbox: no principal to attribute the attachment to")
	}
	if len(parts) > maxAttachmentsPerMessage {
		parts = parts[:maxAttachmentsPerMessage]
	}

	stored := 0
	for _, part := range parts {
		// Decoded BEFORE the transaction, because a decode failure must not
		// abort a message that is otherwise fine: an attachment we cannot read
		// is one missing attachment, and losing the whole email over it would
		// be the worse trade.
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(part.Content))
		if err != nil {
			continue
		}
		if len(raw) == 0 || len(raw) > maxAttachmentBytes {
			continue
		}

		fileID := uuid.NewString()
		name := safeFilename(part.Filename)
		key := file.StorageKey(workspaceID, fileID, filepath.Ext(name))

		// The OBJECT FIRST, then the row.
		//
		// This order is the one the collector is built for: an object with no
		// row is an orphan the sweep removes an hour later, while a row with no
		// object is a file that exists in every listing and fails on download
		// forever, with nothing to clean it up.
		contentType := part.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if err := store.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), contentType); err != nil {
			return stored, fmt.Errorf("store attachment %q: %w", name, err)
		}

		if err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO files (id, workspace_id, user_id, name, file_type, content_type,
				                   size_bytes, storage_key, mail_message_id)
				VALUES ($1, $2, $3, $4, 'file', $5, $6, $7, $8)`,
				fileID, workspaceID, actorID, name, contentType,
				len(raw), key, messageID); err != nil {
				return fmt.Errorf("insert attachment row: %w", err)
			}
			// The ACL object, in the same commit. Without it the file has no
			// path, so the conversation arm of acl_object_expected has nothing
			// to attach to and no agent can open the attachment.
			return authz.MaterializeTx(ctx, tx, authz.FileObject(fileID))
		}); err != nil {
			return stored, err
		}
		stored++
	}
	return stored, nil
}

// safeFilename strips everything that would let a name escape its directory or
// break a Content-Disposition header.
//
// The name is attacker-supplied — it comes from an email anybody can send — and
// it ends up in a header and in a download filename. `filepath.Base` alone is
// not enough: a name with a newline in it splits the header.
func safeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '/' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, name)
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if len(name) > 200 {
		// Truncated from the FRONT of the extension side, so "report.pdf" stays
		// recognisably a pdf.
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// AttachToOutbound links existing Drive files to a reply.
//
// It takes file IDS rather than bytes: an agent attaching something has already
// uploaded it, or is forwarding one that arrived, and re-uploading would make a
// second copy that counts against the quota twice. Authorization is the
// caller's capability on each file, checked here rather than assumed — a reply
// must not be able to attach a file its author cannot read.
func (r *Repository) AttachToOutbound(ctx context.Context, messageID, actorID string, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	if len(fileIDs) > maxAttachmentsPerMessage {
		return fmt.Errorf("mailbox: at most %d attachments per message", maxAttachmentsPerMessage)
	}

	// SHARE, NOT READ — and this is the whole security story of the route.
	//
	// Attaching RE-PARENTS the file: 055 hangs a file with mail_message_id off
	// its conversation, and internal/authz now follows that on the DECISION
	// path too. So setting the column hands every grantee of the mailbox the
	// ability to download the file. That is a share, and "you cannot give away
	// more than you hold" is the rule the sharing route already enforces.
	//
	// It used to require CapRead, and read is not share: a channel member holds
	// CapWrite on a file posted in that channel, which /drive/file/{id}/shares
	// correctly refuses to let them share. An audit demonstrated the escalation
	// end to end — a private-channel file reaching somebody outside the channel
	// through a reply — so this route now asks the same question that one does.
	for _, id := range fileIDs {
		got, err := r.authz.Capability(ctx, authz.UserSubject(actorID), authz.FileObject(id))
		if err != nil {
			return fmt.Errorf("authorize attachment %s: %w", id, err)
		}
		if !got.Implies(authz.CapShare) {
			// 404-shaped: naming the file would confirm one exists at that id.
			return ErrNotFound
		}
	}

	// SAME TENANT, checked in the statement.
	//
	// Capability alone is not enough: a person who belongs to two workspaces
	// legitimately holds read on a file in one and write on a conversation in
	// the other, so authorization passes while the link crosses tenants. The
	// ACL side then fails closed — 055's conversation arm joins on
	// mc.workspace_id = f.workspace_id and drops the branch — so the file's
	// bytes stayed unreachable, but its NAME, content type and size rendered in
	// the other tenant's thread.
	// Counted rather than compared against len(fileIDs), and counted on the
	// END STATE rather than on rows affected.
	//
	// Rows-affected is wrong twice: a duplicate id in the request updates one
	// row and would look like a miss, and re-sending the same attach — which a
	// client retrying a timed-out reply does — updates nothing at all because
	// the file is already linked. Both are successes.
	// The UNION is not stylistic. A data-modifying CTE's writes are NOT visible
	// to the rest of the same statement — the outer query reads the snapshot
	// from before the UPDATE — so counting the end state here returns zero on a
	// first attach. The count has to be "what this statement linked" plus "what
	// was already linked", and UNION deduplicates the overlap.
	// IN A TRANSACTION, because the count check below is a REFUSAL and a refusal
	// that has already written is not one. On the pool this statement committed
	// the rows it did link before ErrNotFound was returned, so a reply the API
	// reported as failed still moved files onto the conversation — and, through
	// the re-parenting above, still widened who could read them. Demonstrated by
	// an audit: a refused batch left its first file linked and downloadable by
	// a mailbox grantee.
	//
	// The ACL is materialized in the SAME commit, for the reason storeAttachments
	// already does it: leaving it to the hourly drift job means an attachment
	// whose stored path still says "the uploader alone" is invisible to every
	// acl_key-driven listing — search, backlinks — while being downloadable. The
	// two authorities must not disagree, in either direction.
	var attached int
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
		WITH linked AS (
			UPDATE files f
			   SET mail_message_id = $2
			  FROM mail_messages m
			  JOIN mail_conversations c ON c.id = m.conversation_id
			 WHERE m.id = $2
			   AND f.id = ANY($1)
			   AND f.mail_message_id IS NULL
			   AND f.workspace_id = c.workspace_id
			RETURNING f.id
		), already AS (
			SELECT id FROM files WHERE id = ANY($1) AND mail_message_id = $2
		)
		SELECT count(*) FROM (
			SELECT id FROM linked UNION SELECT id FROM already
		) AS attached`, fileIDs, messageID).Scan(&attached); err != nil {
			return fmt.Errorf("attach files to message: %w", err)
		}
		if attached != len(dedupeIDs(fileIDs)) {
			// 404-shaped and all-or-nothing: a partially attached reply is one
			// whose author believes they sent something they did not. The usual
			// cause is a file in another tenant — authorization passes for
			// somebody who belongs to both, and the tenancy predicate above is
			// what refuses it. Returning here rolls the link back.
			return ErrNotFound
		}
		for _, id := range dedupeIDs(fileIDs) {
			if err := authz.MaterializeTx(ctx, tx, authz.FileObject(id)); err != nil {
				return fmt.Errorf("materialize attached file ACL: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// dedupeIDs collapses a repeated id, which a client sending the same attachment
// twice will produce and which must not read as a missing file.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// AttachmentsFor lists a message's attachments.
func (r *Repository) AttachmentsFor(ctx context.Context, messageIDs []string) (map[string][]Attachment, error) {
	out := map[string][]Attachment{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT mail_message_id::text, id::text, name, content_type, size_bytes
		  FROM files
		 WHERE mail_message_id = ANY($1) AND trashed_at IS NULL
		 ORDER BY created_at`, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var msgID string
		var a Attachment
		if err := rows.Scan(&msgID, &a.FileID, &a.Name, &a.ContentType, &a.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		out[msgID] = append(out[msgID], a)
	}
	return out, rows.Err()
}

// Attachment is one file on a message, as the thread view shows it.
type Attachment struct {
	FileID      string `json:"file_id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}
