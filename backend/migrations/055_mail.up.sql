-- 055: the shared inbox — email as a first-class surface.
--
-- A mailbox is an addressable destination (support@…); a conversation is a
-- thread of messages with a customer; an agent is somebody granted access to
-- the mailbox. Attachments are Drive files, because a second file story is a
-- second quota story, a second GC story and a second permission story.

CREATE TABLE mail_domains (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain       TEXT NOT NULL,
    -- Until this is true nothing may SEND from the domain. Receiving is
    -- harmless; sending as a domain you do not control is how a deployment
    -- becomes a spam source and gets its IP burned.
    verified_at  TIMESTAMPTZ,
    -- The token the operator publishes in DNS. Random, and never derived from
    -- the domain, or anybody could compute it for a domain they do not own.
    verify_token TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT mail_domains_domain_shape CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'),
    -- One tenant per domain, globally. Two workspaces claiming the same domain
    -- is an unresolvable routing question, and it is better answered at the
    -- second INSERT than at the first inbound message.
    CONSTRAINT mail_domains_domain_key UNIQUE (domain)
);

CREATE TABLE mailboxes (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain_id    UUID REFERENCES mail_domains(id) ON DELETE SET NULL,

    address      TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',

    -- The per-mailbox conversation counter, so a customer sees SUP-14 rather
    -- than a uuid. A ROW COUNTER under FOR UPDATE, not a sequence: a sequence
    -- burns numbers on rollback, and a gap in a customer-visible reference is
    -- indistinguishable from a deletion.
    next_number BIGINT NOT NULL DEFAULT 1,
    prefix      TEXT   NOT NULL,

    -- Auto-reply is per mailbox and OFF by default. On by default is how an
    -- inbox gets into a loop with another auto-responder.
    auto_reply_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    auto_reply_body    TEXT    NOT NULL DEFAULT '',

    archived_at TIMESTAMPTZ,
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT mailboxes_address_shape CHECK (address ~ '^[^@[:space:]]+@[^@[:space:]]+$'),
    CONSTRAINT mailboxes_prefix_shape CHECK (prefix ~ '^[A-Z][A-Z0-9]{1,9}$'),
    CONSTRAINT mailboxes_next_number_positive CHECK (next_number >= 1),
    -- Addresses are matched case-insensitively on the way in, so uniqueness has
    -- to be too, or two mailboxes could both claim Support@ and support@.
    CONSTRAINT mailboxes_address_key UNIQUE (address)
);

CREATE INDEX idx_mailboxes_workspace ON mailboxes (workspace_id) WHERE archived_at IS NULL;

-- A conversation is the unit an agent works on and the unit permissions attach
-- to. It is a real acl_object: agents are granted on the MAILBOX and the
-- conversation inherits, which is the whole reason for the path.
CREATE TABLE mail_conversations (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    mailbox_id UUID NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    -- Denormalized from the mailbox so every tenancy filter is one table.
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    number  BIGINT NOT NULL,
    subject TEXT   NOT NULL DEFAULT '',

    -- Closed sets in CHECKs rather than enums: adding a state must not be a
    -- migration that cannot run inside a transaction.
    state    TEXT NOT NULL DEFAULT 'open'
             CHECK (state IN ('open', 'pending', 'closed', 'spam')),
    priority TEXT NOT NULL DEFAULT 'normal'
             CHECK (priority IN ('low', 'normal', 'high', 'urgent')),

    assignee_id UUID REFERENCES users(id) ON DELETE SET NULL,

    -- Who we are talking to. Denormalized from the first message because every
    -- list view shows it and joining to the message table for a list is the
    -- N+1 this design exists to avoid.
    customer_email TEXT NOT NULL DEFAULT '',
    customer_name  TEXT NOT NULL DEFAULT '',

    last_message_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Set when the LAST message was inbound, so "waiting on us" is a column
    -- rather than a subquery over the message table.
    awaiting_reply BOOLEAN NOT NULL DEFAULT TRUE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT mail_conversations_number_key UNIQUE (mailbox_id, number)
);

-- The inbox list: one mailbox, newest activity first, keyset-paginated.
CREATE INDEX idx_mail_conversations_inbox
    ON mail_conversations (mailbox_id, last_message_at DESC, id DESC);
CREATE INDEX idx_mail_conversations_assignee
    ON mail_conversations (assignee_id, last_message_at DESC) WHERE assignee_id IS NOT NULL;
CREATE INDEX idx_mail_conversations_workspace ON mail_conversations (workspace_id);

CREATE TABLE mail_messages (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    conversation_id UUID NOT NULL REFERENCES mail_conversations(id) ON DELETE CASCADE,

    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),

    -- RFC 5322 identifiers, kept because threading depends on them and because
    -- a resent message must not create a second conversation.
    message_id  TEXT NOT NULL,
    in_reply_to TEXT NOT NULL DEFAULT '',
    -- The References header, newest last. TEXT[] rather than a join table: it
    -- is read whole or not at all.
    references_ids TEXT[] NOT NULL DEFAULT '{}',

    from_address TEXT NOT NULL DEFAULT '',
    from_name    TEXT NOT NULL DEFAULT '',
    to_addresses TEXT[] NOT NULL DEFAULT '{}',
    cc_addresses TEXT[] NOT NULL DEFAULT '{}',

    subject   TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    -- The sanitized HTML, or empty. Sanitization happens on the way IN, once,
    -- because a renderer that sanitizes on the way out is one XSS away from a
    -- caller that forgot to.
    body_html TEXT NOT NULL DEFAULT '',

    -- WHERE THE ORIGINAL LIVES. An object key in the same bucket Drive uses,
    -- with NO files row — the raw RFC822 is not something a user browses.
    --
    -- THIS COLUMN IS WHY internal/file's StorageKeysPresent GREW AN ARM in the
    -- same change as this migration. The bucket sweep deletes any object whose
    -- key is absent from the tables it knows about, so a raw_key it has never
    -- heard of means every archived email is deleted an hour after it arrives.
    raw_key TEXT NOT NULL DEFAULT '',

    -- Set by the author for an outbound message that has not been sent yet.
    -- NULL sent_at with direction='outbound' is a draft.
    author_id UUID REFERENCES users(id) ON DELETE SET NULL,
    sent_at   TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- An inbound message is never a draft and always has a time.
    CONSTRAINT mail_messages_inbound_is_sent
        CHECK (direction = 'outbound' OR sent_at IS NOT NULL)
);

CREATE INDEX idx_mail_messages_conversation ON mail_messages (conversation_id, created_at);
-- Threading looks a message up by its RFC id, per mailbox.
CREATE INDEX idx_mail_messages_message_id ON mail_messages (message_id);

-- Inbound events, before they become messages.
--
-- Two jobs: the idempotency key for a provider that delivers at least once, and
-- the parking place for the raw bytes of a message that failed to parse. A
-- message that could not be parsed must still be recoverable — the alternative
-- is telling a customer their email vanished.
CREATE TABLE mail_inbound_events (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID REFERENCES workspaces(id) ON DELETE CASCADE,
    -- The provider's id. UNIQUE, so redelivery is a no-op rather than a
    -- duplicate conversation.
    provider_event_id TEXT NOT NULL,
    recipient         TEXT NOT NULL DEFAULT '',

    -- The SECOND raw_key, and the second arm in StorageKeysPresent. An event
    -- that never became a message still owns its bytes.
    raw_key TEXT NOT NULL DEFAULT '',

    status TEXT NOT NULL DEFAULT 'received'
           CHECK (status IN ('received', 'processed', 'rejected', 'failed')),
    error  TEXT NOT NULL DEFAULT '',

    message_id   UUID REFERENCES mail_messages(id) ON DELETE SET NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,

    CONSTRAINT mail_inbound_events_provider_key UNIQUE (provider_event_id)
);

CREATE INDEX idx_mail_inbound_events_status ON mail_inbound_events (status, received_at)
    WHERE status IN ('received', 'failed');

-- The ingest credential. A bearer token, stored as its sha256 and never in
-- plaintext, matched by an indexed hash — the same shape as every other token
-- in this product.
CREATE TABLE mail_ingest_tokens (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    token_hash   TEXT NOT NULL,
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at   TIMESTAMPTZ,

    CONSTRAINT mail_ingest_tokens_hash_key UNIQUE (token_hash)
);

CREATE INDEX idx_mail_ingest_tokens_live ON mail_ingest_tokens (token_hash) WHERE revoked_at IS NULL;

-- Attachments are Drive files, and this is the link.
--
-- The column goes on `files` rather than a join table because it is what the
-- GC predicate reads: a file owned by a mail message is not an orphan, and
-- expressing that as "not in a join table" would make the collector's predicate
-- a NOT EXISTS over an unindexed relation.
ALTER TABLE files ADD COLUMN mail_message_id UUID REFERENCES mail_messages(id) ON DELETE SET NULL;

CREATE INDEX idx_files_mail_message ON files (mail_message_id) WHERE mail_message_id IS NOT NULL;

-- THE GC PREDICATE, REBUILT.
--
-- The collector deletes files owned by neither a folder nor a message, and a
-- mail attachment is owned by neither. Without this rebuild every attachment is
-- deleted an hour after it arrives — the exact failure the original index's
-- comment describes, one owner later.
DROP INDEX IF EXISTS idx_files_unowned;
CREATE INDEX idx_files_unowned ON files (created_at)
    WHERE folder_id IS NULL AND message_id IS NULL AND mail_message_id IS NULL
      AND trashed_at IS NULL;

-- Mailboxes and conversations are ACL-NATIVE objects, exactly as Drive folders
-- are: Register owns their acl_object row, so they are ABSENT from the expected
-- view. That is what lets an agent be granted on a mailbox and reach every
-- conversation in it through the path — derivedTypes stays {workspace, channel,
-- file}, and adding a derived type here would make Register and Move refuse
-- them.
--
-- The file arm gains ONE branch: a mail attachment inherits its conversation's
-- path. acl_key_expected is NOT touched — its container arm already inherits a
-- container's grants, so a conversation picks up the mailbox's keys with no
-- change at all.
--
-- Dropped and recreated rather than CREATE OR REPLACE, for the reason migration
-- 025 gives: REPLACE only accepts an identical column list, and a half-applied
-- view is the worst outcome available.
DROP VIEW acl_key_expected;
DROP VIEW acl_object_expected;

CREATE VIEW acl_object_expected AS
    SELECT 'workspace'::TEXT AS object_type,
           w.id              AS object_id,
           w.id              AS workspace_id,
           '/workspace:' || w.id::TEXT || '/' AS path
      FROM workspaces w
UNION ALL
    SELECT 'channel'::TEXT, c.id, c.workspace_id,
           '/workspace:' || c.workspace_id::TEXT || '/channel:' || c.id::TEXT || '/'
      FROM channels c
UNION ALL
    -- Precedence: folder, then conversation, then the attached message's
    -- channel, then the workspace. A file has at most one owner, so the order
    -- only decides what happens to corrupt data — and every branch degrades
    -- toward the workspace path, which is the fail-closed reading.
    --
    -- The workspace equality on each join is not redundant: a cross-workspace
    -- owner is corrupt data, and inheriting its path would produce a row whose
    -- root segment disagrees with workspace_id, which
    -- acl_object_path_in_workspace then rejects — turning a data bug into a
    -- failed backfill instead of a tenancy leak.
    SELECT 'file'::TEXT, f.id, f.workspace_id,
           CASE
               WHEN fo.object_id IS NOT NULL
                   THEN fo.path || 'file:' || f.id::TEXT || '/'
               WHEN co.object_id IS NOT NULL
                   THEN co.path || 'file:' || f.id::TEXT || '/'
               WHEN ch.id IS NOT NULL
                   THEN '/workspace:' || f.workspace_id::TEXT || '/channel:' || ch.id::TEXT
                        || '/file:' || f.id::TEXT || '/'
               ELSE '/workspace:' || f.workspace_id::TEXT || '/file:' || f.id::TEXT || '/'
           END
      FROM files f
      LEFT JOIN acl_object fo ON fo.object_type = 'folder'
                             AND fo.object_id   = f.folder_id
                             AND fo.workspace_id = f.workspace_id
      LEFT JOIN mail_messages mm ON mm.id = f.mail_message_id
      LEFT JOIN mail_conversations mc ON mc.id = mm.conversation_id
                                     AND mc.workspace_id = f.workspace_id
      LEFT JOIN acl_object co ON co.object_type = 'conversation'
                             AND co.object_id   = mc.id
                             AND co.workspace_id = f.workspace_id
      LEFT JOIN messages m    ON m.id = f.message_id
      LEFT JOIN channels ch   ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

-- Recreated verbatim from migration 028. It is UNCHANGED: a conversation is
-- a container like a folder, and the container arm already inherits its grants.
CREATE VIEW acl_key_expected AS
    -- 1. The workspace object is readable by every member of the workspace.
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT AS key
      FROM acl_object o
     WHERE o.object_type = 'workspace'
UNION
    -- 2a. A channel is readable by its members, whatever its type.
    SELECT o.object_type, o.object_id, 'u-' || cm.user_id::TEXT
      FROM acl_object o
      JOIN channel_members cm ON cm.channel_id = o.object_id
     WHERE o.object_type = 'channel'
UNION
    -- 2b. ...and a PUBLIC channel is additionally readable by every member of
    --     its workspace.
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT
      FROM acl_object o
      JOIN channels c ON c.id = o.object_id
     WHERE o.object_type = 'channel' AND c.type = 'public'
UNION
    -- 3. Anything inside a container inherits the container's key.
    SELECT o.object_type, o.object_id, acl_container_key(acl_parent_segment(o.path))
      FROM acl_object o
     WHERE acl_container_key(acl_parent_segment(o.path)) IS NOT NULL
UNION
    -- 4. A file owned by neither a folder nor a channel is readable by its
    --    uploader alone.
    SELECT o.object_type, o.object_id, 'u-' || f.user_id::TEXT
      FROM acl_object o
      JOIN files f ON f.id = o.object_id
     WHERE o.object_type = 'file'
       AND split_part(coalesce(acl_parent_segment(o.path), ''), ':', 1) = 'workspace'
UNION
    -- 5. Explicit grants, inherited by the whole subtree of the granted object.
    --
    --    'link' is new in this migration and shares the 'g-' prefix. A sixth
    --    prefix would have to be added to internal/search's CLOSED validator,
    --    and a key that fails it is DROPPED from the filter — a dropped
    --    narrowing term WIDENS the query, which for a tenancy filter is a
    --    cross-tenant leak (README ruling 3). 'g-' already means "a subject that
    --    is not one person".
    SELECT d.object_type, d.object_id,
           CASE g.subject_type
               WHEN 'user'      THEN 'u-'
               WHEN 'group'     THEN 'g-'
               WHEN 'workspace' THEN 'w-'
               WHEN 'link'      THEN 'g-'
           END || g.subject_id::TEXT
      FROM acl_grant g
      JOIN acl_object go ON go.object_type = g.object_type AND go.object_id = g.object_id
      JOIN acl_object d  ON d.path LIKE go.path || '%'
     WHERE g.subject_type IN ('user', 'group', 'workspace', 'link')
UNION
    -- 6. A file attached to a message is readable by that channel, whatever its
    --    Drive home.
    SELECT 'file'::TEXT, f.id, 'c-' || ch.id::TEXT
      FROM files f
      JOIN messages m  ON m.id = f.message_id
      JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;
