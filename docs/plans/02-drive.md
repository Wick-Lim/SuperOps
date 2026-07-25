# Plan 02 — Drive, and the editor registry

**Phase 1.** Depends on Plan 00 (object permissions) and on the in-flight collaboration
layer (`migrations/015_collab.up.sql`, `internal/ws/room.go`). Three later phases —
Docs, Spreadsheet, Design — are handlers behind the contract this plan defines.

Status: design.

---

## 1. What it is

A workspace-scoped file store with a folder tree, sharing, versions, a trash and a
quota — and an **editor registry** that says what happens when you open something.
A file has a *type*; the type decides which surface renders it and where its bytes
actually live. "New document" creates a Drive file of type `document`, which is a
row in `files`, an `acl_object`, and a `collab_documents` row, all in one
transaction. Opening it returns a descriptor the client dispatches on.

Concretely, the user gets: a folder tree per workspace, drag/move, rename, upload,
download, per-file and per-folder sharing (to people, and by link), version history
with restore, a trash with restore and a purge job, thumbnails for images, and a
storage-usage figure that an admin can cap.

**Not in scope:**

- **Desktop sync.** Roadmap §7. It is a product, not a feature.
- **Preview/conversion of anything that needs an external binary** — PDF rasterising,
  Office documents, video posters. That means ImageMagick/ffmpeg/LibreOffice in the
  image, which is a CVE surface and an ops category, not a line item. Images only.
- **Server-side export of native editor types** to `.docx`/`.xlsx`/`.pdf`. See §8 —
  it is not "later work", it is architecturally blocked by the deliberate decision
  that the server never interprets CRDT state.
- **Per-file comments.** Work tracking (Phase 2) builds the comment surface; Drive
  adopts it rather than growing a second one.
- **Content-hash deduplication.** Breaks version and quota accounting, needs
  refcounting, saves nothing at this scale.
- **Anonymous write via share link.** Read-only links only.
- **Native GCS backend.** See §6 — S3-compatible interop only, and the reason is a
  dependency, stated.

---

## 2. What exists, and the two things that will destroy data if this lands naively

`internal/file` is ~60% of the storage half and 0% of the Drive half. It is
**chat-attachment-shaped**, and two consequences are load-bearing:

**The `files` row's ACL is its message's channel.** `file.Handler.canRead`
(`backend/internal/file/handler.go:251-264`) authorizes against
`authz.MessageChannel`, falling back to uploader-only when `message_id IS NULL`.
There is no other notion of who may read a file.

**`message_id IS NULL` currently means "garbage".** `Repository.ListOrphans`
(`backend/internal/file/repository.go:54-62`) selects
`WHERE message_id IS NULL AND created_at < $1`, and `runObjectGC`
(`backend/cmd/worker/main.go:1121-1157`) deletes those objects and rows an hour at a
time, 24 hours after upload (`objectGCGrace`, `main.go:114`).

> **A Drive file is, by that definition, an orphan.** Ship folders without touching
> the GC predicate and every file a user uploads to Drive is silently deleted the
> next day, by a job that logs it as success. This is the single most dangerous
> interaction in the phase and it must be in the first commit, not the last.

The second, quieter one: the bucket sweep
(`main.go:1159-1196` → `Repository.StorageKeysPresent`, `repository.go:100-122`)
deletes any object whose key is absent from `files.storage_key`/`thumbnail_key`.
Add `file_versions` and every non-head version object becomes "unreferenced" and is
swept. Both predicates change in migration 017's PR, with the regression tests to
match (§11).

Reusable as-is: the MinIO client and `ListKeys` prefix sharding
(`internal/file/storage.go:27-107`), the content-type sniffing and
inline/attachment policy (`handler.go:53-135` — that allowlist is a security
control, not a convenience), the keyset cursor (`pkg/httputil/pagination.go:22-27`),
the `{data,meta,error}` envelope (`pkg/httputil/response.go`), the durable-consumer
scaffolding and advisory-locked job loops in `cmd/worker`, and `search.FileDoc`
(`internal/search/doc.go:311-339`), which already exists and already has the right
ACL story.

### What Plan 00 gives us, and the one gap

Drive takes from Plan 00: `acl_object` (with the materialized path — Drive keeps
**no** second path column), `Capability`/`Can`, `KeysFor` for list and search,
`Move` as a prefix rewrite, and `GrantsFor`/`SubjectsOf` for the sharing UI.

Drive should be **the first package cut over** to the new checker under 00's
dual-run plan. It has almost no legacy authorization to preserve: the only
behaviour that must not move is `canRead` for message attachments, which is one
function and is covered by `TestCrossChannelIDOR`.

**Gap: share links are not designed in Plan 00.** It says only that `subject_type`
"leaves room for a link token" and calls it deferred. Drive needs them in v1, so
this plan proposes the minimal extension rather than a parallel permission system:

- `subject_type = 'link'`, `subject_id = drive_share_links.id`, capability `read`
  or `comment`. An `acl_grant` row like any other, so `SubjectsOf` shows links in
  the same sharing panel and `GrantsFor` audit answers include them.
- Resolving a link mints a short-lived, link-scoped session whose `KeysFor` returns
  exactly `link:<id>` — never the workspace member key. `acl_object.workspace_id`
  stays authoritative, so a link can never widen tenancy.
- If Plan 00 rejects link subjects, sharing links move behind a feature flag and
  the rest of Drive ships unchanged. Say so before starting, not after.

Not covered by 00 and correctly so: **quota** is not a permission. It lives here.

---

## 3. Data model

Migration numbers 000–012 exist, 014 and 015 are in flight, 016 is claimed.
**This plan takes 017 (Drive core) and 018 (share links).**

> `013` is an unused number. Do not reclaim it. `golang-migrate` stores a single
> version integer (`cmd/migrate/main.go`) and `Up()` only applies files above it,
> so a `013` added now would be silently skipped on every database that has already
> passed `015` — a migration that never runs and never errors.

### 017 — folders, Drive-shaped files, versions, trash, quota

```sql
CREATE TABLE drive_folders (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- RESTRICT, not CASCADE: deletion goes through trash + the purge job, which
    -- walks the subtree deliberately. A cascade here would let one DELETE remove
    -- an arbitrary number of files with no object cleanup and no audit trail.
    parent_id    UUID REFERENCES drive_folders(id) ON DELETE RESTRICT,
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    is_root      BOOLEAN NOT NULL DEFAULT FALSE,
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    trashed_at   TIMESTAMPTZ,
    trashed_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT drive_folders_parent_required CHECK (is_root OR parent_id IS NOT NULL)
);
CREATE UNIQUE INDEX idx_drive_folders_root     ON drive_folders (workspace_id) WHERE is_root;
CREATE INDEX        idx_drive_folders_children ON drive_folders (parent_id, name) WHERE trashed_at IS NULL;
CREATE INDEX        idx_drive_folders_trash    ON drive_folders (workspace_id, trashed_at) WHERE trashed_at IS NOT NULL;
```

Every workspace gets exactly one root folder, created by a backfill for existing
workspaces and by `workspace.Handler.Create` thereafter. Names are **not** unique
within a parent — a unique index fights restore-from-trash and copy, and Drive-style
duplicate names are the expected behaviour.

```sql
ALTER TABLE files
    ADD COLUMN folder_id       UUID REFERENCES drive_folders(id) ON DELETE RESTRICT,
    ADD COLUMN file_type       TEXT NOT NULL DEFAULT 'file',
    ADD COLUMN current_version INT  NOT NULL DEFAULT 1,
    ADD COLUMN trashed_at      TIMESTAMPTZ,
    ADD COLUMN trashed_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN updated_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    -- Same shape rule as collab_documents.resource_type (migrations/015): a new
    -- editor must not need a migration, and the value is never spliced into a
    -- query, a key or a subject.
    ADD CONSTRAINT files_type_valid CHECK (file_type ~ '^[a-z][a-z0-9_]{0,31}$');

CREATE INDEX idx_files_folder ON files (folder_id, created_at DESC, id DESC) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_trash  ON files (workspace_id, trashed_at) WHERE trashed_at IS NOT NULL;
```

`folder_id` and `message_id` are **independent**. A chat upload has `folder_id NULL`.
A Drive file has `folder_id NOT NULL`. A Drive file attached to a message has both —
which is the "attached without a copy" seam from ROADMAP §1, and it is one `UPDATE`
on the existing attach path (`internal/message/repository.go:153`).

`storage_key` stays as the denormalized head pointer so the download path
(`handler.go:290-315`) and the GC do not move. For a native (CRDT-backed) type it is
the empty string; `Orphan.Keys()` already skips empty keys
(`repository.go:41-50`), so that costs nothing.

**The GC predicate changes in this PR:**

```sql
-- ListOrphans
WHERE folder_id IS NULL AND message_id IS NULL AND trashed_at IS NULL AND created_at < $1

-- StorageKeysPresent gains a third arm
UNION SELECT storage_key FROM file_versions WHERE storage_key = ANY($1)
```

```sql
CREATE TABLE file_versions (
    file_id      UUID   NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    version      INT    NOT NULL CHECK (version > 0),
    storage_key  TEXT   NOT NULL,
    size_bytes   BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type TEXT   NOT NULL,
    checksum     TEXT   NOT NULL DEFAULT '',  -- hex sha256, '' when not computed
    created_by   UUID   REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_id, version)
);
CREATE INDEX idx_file_versions_key ON file_versions (storage_key);
```

Backfilled with one row per existing `files` row at `version = 1`. **The file id is
stable across versions** — that is what lets a message attachment, a
`collab_documents.resource_id` and a search document all keep pointing at the same
object when someone uploads a new revision.

```sql
ALTER TABLE workspaces ADD COLUMN storage_quota_bytes BIGINT NOT NULL DEFAULT 0; -- 0 = unlimited

CREATE TABLE workspace_storage (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    bytes_used   BIGINT NOT NULL DEFAULT 0 CHECK (bytes_used >= 0),
    -- Bytes held by CRDT logs and snapshots, recomputed by the accounting job.
    -- Separate because it is eventually consistent and blob bytes are not.
    collab_bytes BIGINT NOT NULL DEFAULT 0 CHECK (collab_bytes >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Enforcement is one atomic statement in the upload transaction, so concurrent uploads
serialize on the row instead of racing a check-then-insert:

```sql
UPDATE workspace_storage
   SET bytes_used = bytes_used + $1, updated_at = NOW()
 WHERE workspace_id = $2
   AND ($3 = 0 OR bytes_used + $1 <= $3)
RETURNING bytes_used;   -- no row = over quota → 507
```

**Trashed files and old versions still count.** That is the point of a quota, and
saying so in the UI is cheaper than explaining it in support.

Finally, 017 closes the FK that migration 015 left open on purpose:

```sql
ALTER TABLE collab_documents
    ADD CONSTRAINT collab_documents_resource_fk
    FOREIGN KEY (resource_id) REFERENCES files(id) ON DELETE CASCADE;
```

This is Drive claiming the collaboration resource space, exactly as ROADMAP §3b
says it should: `collab_documents.resource_type` **is** `files.file_type`, not a
parallel vocabulary. It also makes revocation and purge correct by construction.

### 018 — share links

```sql
CREATE TABLE drive_share_links (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    object_type   TEXT NOT NULL CHECK (object_type IN ('file','folder')),
    object_id     UUID NOT NULL,
    -- sha256 of the token. The token itself is returned once, at creation, and
    -- never stored: it is a bearer credential for content. (webhooks.token in
    -- migration 007 stores plaintext; that is a wart to fix, not a precedent.)
    token_hash    BYTEA NOT NULL UNIQUE,
    capability    TEXT NOT NULL DEFAULT 'read' CHECK (capability IN ('read','comment')),
    password_hash TEXT NOT NULL DEFAULT '',   -- bcrypt; '' = no password
    expires_at    TIMESTAMPTZ,
    max_uses      INT CHECK (max_uses IS NULL OR max_uses > 0),
    use_count     INT NOT NULL DEFAULT 0,
    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_drive_share_links_object ON drive_share_links (object_type, object_id) WHERE revoked_at IS NULL;
```

Creating a link also writes the `acl_grant` row with `subject_type='link'`;
revoking deletes it. The link table carries the things a grant does not have a
column for (expiry, password, use cap) — the *authorization* still comes from one
place.

---

## 4. The editor registry

This is the part that outlives the phase, so it is written down before the CRUD.

### It is two registries joined by one string

**Server registry** — type → *lifecycle policy*: where the bytes live, whether a
new version can be POSTed, what "new" creates, how it is indexed, how it is purged.

**Client registry** — type → *surface*: which React component opens the pane.

They are joined by `file_type`, and that string is already constrained identically
in two tables (`files.file_type` and `collab_documents.resource_type`) and appears a
third time as `search.ObjectType` (`internal/search/doc.go:22-37`, which already
declares `document`, `spreadsheet`, `design`). **One vocabulary, three tables**, and
a unit test asserts that every registered `Kind` has a matching `search.ObjectType`
so the three cannot drift silently.

### The contract

```go
// internal/drive/registry
type StorageMode int
const (
    StorageBlob   StorageMode = iota // bytes in object storage are the truth
    StorageCollab                    // the CRDT log in Postgres is the truth
)

type Kind struct {
    Type        string      // "file", "document", "spreadsheet", "design"
    DisplayName string
    Storage     StorageMode

    // Import mapping, for uploads and for "open with".
    Extensions  []string
    MimeTypes   []string

    // New creates the initial state of an empty object of this type, inside the
    // caller's transaction. For StorageBlob this writes an object; for
    // StorageCollab it writes the collab_documents row and (optionally) a seed
    // update. It must be idempotent on (workspace, file id).
    New func(ctx context.Context, tx pgx.Tx, req NewRequest) error

    // Text returns indexable body text, or ("", nil) when the type has none.
    // Called by the worker, never in a request. nil means title-only indexing.
    Text func(ctx context.Context, src io.Reader) (string, error)

    // Thumb produces a preview image, or (nil, ErrNoPreview).
    Thumb func(ctx context.Context, src io.ReadSeeker) (Preview, error)

    Versioned  bool // false for StorageCollab in v1 — see §8
    Previewable bool
}

func Register(k Kind)                      // called from app.New, not from init()
func Lookup(t string) (Kind, bool)
func ForUpload(name, contentType string) Kind  // never fails; falls back to "file"
func All() []Kind                              // served by GET /drive/registry
```

`Register` is called explicitly from `app.New` rather than from package `init()`,
for the same reason the existing wiring is explicit: an editor that is not
configured in a deployment should not be discoverable, and `init()` ordering is not
a place to put an authorization-adjacent decision.

### How a file opens

`GET /api/v1/drive/files/{file_id}` returns the **open descriptor** — the whole
dispatch contract in one payload:

```json
{ "data": {
  "id": "...", "name": "Q3 plan", "file_type": "document",
  "storage_mode": "collab",
  "capability": "write",
  "collab_document_id": "...",
  "content_url": null,
  "thumbnail_url": "https://...",
  "current_version": 4,
  "folder_id": "...", "workspace_id": "...",
  "size_bytes": 0, "created_at": "...", "updated_at": "..."
}}
```

`storage_mode: "blob"` fills `content_url` and leaves `collab_document_id` null.
The client looks up `file_type` in its own registry and renders; it never branches
on MIME type, and it never has to know that a document is CRDT-backed except to
pick a component.

`POST /api/v1/workspaces/{id}/drive/files` with `{"file_type":"document","name":"…","folder_id":"…"}`
is "new document": one transaction writing the `files` row, the `acl_object` row,
`Kind.New`, and the audit entry.

### What the registry deliberately does not do

It does not carry permissions (that is `authz`), does not carry the WebSocket
protocol (that is `ws`, already built — `internal/ws/room.go:63-68`), and does not
carry rendering. Each editor phase adds **one `Kind` value and one client
component**, and gets storage, ACLs, sharing, versions, trash, search and activity
for free. If a later editor needs a new field on `Kind`, that is the signal that the
contract was under-specified — it is cheap to add now and expensive after three
implementations.

---

## 5. API surface

Conventions unchanged: `RegisterRoutes(mux, authMw)`, `{data,meta,error}`,
`httputil.JSONList` with keyset cursors, `err != nil → 500`, `!ok → 403/404`, never
collapsed.

**Existing routes stay.** `POST /api/v1/files/upload`, `GET|DELETE /api/v1/files/{file_id}`
(`internal/file/handler.go:47-51`) remain the message-attachment path; the client
(`app/src/api/files.ts`) keeps working unchanged.

```
Folders
  POST   /api/v1/workspaces/{workspace_id}/drive/folders
  GET    /api/v1/workspaces/{workspace_id}/drive/folders/{folder_id}          -- metadata + breadcrumb
  GET    /api/v1/workspaces/{workspace_id}/drive/folders/{folder_id}/children -- keyset, folders then files
  PATCH  /api/v1/drive/folders/{folder_id}                                    -- rename
  POST   /api/v1/drive/folders/{folder_id}/move                               -- {parent_id}
  DELETE /api/v1/drive/folders/{folder_id}                                    -- trash

Files
  POST   /api/v1/workspaces/{workspace_id}/drive/files          -- new-from-registry (JSON)
  POST   /api/v1/workspaces/{workspace_id}/drive/files/upload   -- multipart, into a folder
  GET    /api/v1/drive/files/{file_id}                          -- open descriptor
  PATCH  /api/v1/drive/files/{file_id}                          -- rename
  POST   /api/v1/drive/files/{file_id}/move
  DELETE /api/v1/drive/files/{file_id}                          -- trash
  GET    /api/v1/drive/files/{file_id}/content                  -- 302 to presigned, or proxied
  POST   /api/v1/drive/files/{file_id}/content                  -- new version (409 for storage_mode=collab)
  GET    /api/v1/drive/files/{file_id}/thumbnail                -- 302 to presigned
  GET    /api/v1/drive/files/{file_id}/versions                 -- keyset
  GET    /api/v1/drive/files/{file_id}/versions/{version}/content
  POST   /api/v1/drive/files/{file_id}/versions/{version}/restore

Trash
  GET    /api/v1/workspaces/{workspace_id}/drive/trash          -- keyset
  POST   /api/v1/drive/{object_type}/{object_id}/restore
  DELETE /api/v1/workspaces/{workspace_id}/drive/trash          -- empty now

Sharing            (thin wrappers over authz; the capability vocabulary is Plan 00's)
  GET    /api/v1/drive/{object_type}/{object_id}/shares
  PUT    /api/v1/drive/{object_type}/{object_id}/shares         -- {subject_type, subject_id, capability}
  DELETE /api/v1/drive/{object_type}/{object_id}/shares/{subject_type}/{subject_id}
  POST   /api/v1/drive/{object_type}/{object_id}/links          -- returns the token ONCE
  GET    /api/v1/drive/{object_type}/{object_id}/links
  DELETE /api/v1/drive/links/{link_id}
  POST   /api/v1/drive/links/{token}/resolve                    -- NO authMw; rate-limited by IP

Meta
  GET    /api/v1/drive/registry                                 -- All(), so the client hardcodes nothing
  GET    /api/v1/workspaces/{workspace_id}/drive/usage
  POST   /api/v1/admin/storage/test                             -- §6; adminMw + its own IP budget
```

`/drive/links/{token}/resolve` is POST and unauthenticated, mirroring the auth
routes' rate-limit chain (`internal/app/app.go:304-315`). POST, not GET, so the
token never lands in a `Referer` and cannot be prefetched. It returns a link-scoped
access token, not the content.

**Listing never calls `Can` per row.** Children, trash and search all filter on
`KeysFor` — Plan 00 names this as the failure mode the whole design exists to
prevent, and Drive is the endpoint where it would first bite.

---

## 6. Packages

| Package | Owns | Reuses rather than rebuilds |
|---|---|---|
| `internal/storage` (new) | The `Backend` interface, the S3/MinIO implementation, presigning, boot validation | Moved out of `internal/file/storage.go` almost verbatim |
| `internal/drive` (new) | Folders, files-as-Drive-objects, versions, trash, quota, share links, handlers | `authz` for every decision, `httputil` for envelope/cursor, `audit` for the trail |
| `internal/drive/registry` (new) | `Kind`, `Register`, `Lookup` | — |
| `internal/file` (kept) | Message attachments, upload sniffing, the GC repository | Now calls `storage.Backend` instead of holding a MinIO client |
| `internal/search` | Unchanged shape; gains `folder_id` and Drive ACL keys | `FileDoc` already exists (`doc.go:311-339`) |
| `cmd/worker` | Two new consumers (`thumbnailer`, `drive-reindex`), two new jobs (`trash_purge`, `storage_accounting`) | `bindDurable`, `runLoop`, `withSingletonLock` unchanged |

### Storage as a deployment-dependent capability (ROADMAP §3c)

```go
package storage

type Backend interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
    Head(ctx context.Context, key string) (ObjectInfo, error)
    Delete(ctx context.Context, keys ...string) error
    List(ctx context.Context, prefix string, limit int) ([]string, error)
    // PresignGet returns "" when the backend cannot presign, so the caller
    // falls back to proxying rather than failing.
    PresignGet(ctx context.Context, key string, ttl time.Duration, opt PresignOptions) (string, error)
    PresignPut(ctx context.Context, key string, ttl time.Duration, maxSize int64) (string, error)
    Name() string
}
```

**No new dependency.** `minio-go` is already vendored and already speaks the S3 API,
so `STORAGE_BACKEND=minio` and `STORAGE_BACKEND=s3` are the same implementation with
different endpoint/region/path-style defaults. GCS is reachable today through its
S3 interoperability endpoint with HMAC keys; a *native* GCS backend needs
`cloud.google.com/go/storage`, and that is a deliberate later decision with a named
cost, not an oversight.

The three §3c rules, applied literally:

1. **Safe default.** `minio`, pointing at the compose service, exactly as today
   (`internal/app/config.go:255-265`).
2. **Fail at boot.** Selecting `s3` without a region or credentials is a startup
   error, not a 500 on the first upload. The bucket existence check already runs at
   construction (`storage.go:38-47`); extend it to a round-trip Put/Get/Delete of a
   marker object so an unwritable bucket fails the boot too.
3. **Operator verification.** `POST /api/v1/admin/storage/test` performs that
   round-trip on demand and returns the transport's real error, in the same shape
   as the mail configuration test wired at `app.go:352-362`.

### Presigning, and why it does not weaken the download hardening

Today every downloaded byte transits the Go process (`handler.go:290-315`). That is
fine for chat attachments and wrong for Drive. But the proxy path is also where the
security headers live — `X-Content-Type-Options: nosniff`, a restrictive CSP,
and the `inlineTypes` allowlist that keeps anything scriptable from rendering on our
origin (`handler.go:53-74`, `299-306`). A presigned URL cannot carry a CSP header.

The split:

- **Originals** are presigned with `response-content-disposition: attachment` and
  `response-content-type: application/octet-stream`, always. A downloaded file is
  never rendered by the browser, so the CSP has nothing to protect.
- **Inline rendering** uses the **thumbnail**, which the server generated, whose
  type is always `image/webp`, and which can therefore be presigned and used as an
  `<img src>` safely.
- **Small inline originals** (the PDF/plain-text preview case) keep the proxied
  path with its existing headers. Bounded by size, not by taste.

A useful side effect: presigned URLs carry their own authorization, so Drive does
**not** need to extend `allowsQueryToken` (`internal/auth/middleware.go:59-70`).
That allowlist exists because a token in a URL leaks into proxy logs and `Referer`;
adding a second Drive-shaped prefix to it would have been a quiet regression.
`LoggingMiddleware` logs `r.URL.Path` only (`pkg/httputil/middleware.go:70`), so
presigned query strings do not land in our own logs either.

---

## 7. Realtime and search seams

**Revocation.** `internal/ws/room.go:226-236` already has `RevokeRoom` and
`RevokeRoomForAll`, keyed on the *collab document* id. Drive knows the file id, so
it declares an interface it does not implement:

```go
// internal/drive
type RoomRevoker interface {
    RevokeObject(ctx context.Context, fileID, userID string) // userID "" = everyone
}
```

`internal/collab` implements it (it owns the `files.id → collab_documents.id`
lookup), and `app.New` wires it. The dependency runs `drive → interface ← collab`,
so neither imports the other — the same shape as `collab → ws` in
`room.go:56-62`. Called on: revoke a grant, revoke a link, move an object out of a
shared folder, trash, purge.

**Search.** `search.Doc` gains `folder_id` (the comment at `doc.go:178` already
reserves it, and explicitly forbids overloading `channel_id`), which joins
`wantFilterable` (`service.go:78-82`). `FileDoc.Doc()` changes its ACL derivation
from "channel key, else user key" to "the object's `acl_key` rows", which is exactly
the seam Plan 00 flags. Indexing is a new durable consumer on
`superops.*.drive.*` — the same `bindDurable` contract, permanent errors terminate,
transient errors nak.

---

## 8. The hard part

**A file whose bytes are not the source of truth.**

Everything Drive does is written against "an object in a bucket": download,
versioning, quota accounting, search extraction, copy, purge, export. For three of
the four types the registry will carry — `document`, `spreadsheet`, `design` — that
is false. Their state is an append-only CRDT log in Postgres
(`collab_updates`/`collab_snapshots`, migration 015), and the server **cannot
interpret it**: migration 015's own header says a Go re-implementation that
disagreed with the client's would be "a corruption bug debuggable from neither
side". That decision is correct and this plan does not reopen it.

So the registry has to be honest about a type whose bytes the server cannot produce.
Concretely, seven things break, and each needs an answer *now* because three later
phases inherit whichever answer we give:

| Breaks | Answer in v1 |
|---|---|
| **Download / export** | `storage_mode: collab` returns `content_url: null`. `GET /content` serves the latest **snapshot blob** with `Content-Type: application/vnd.superops.<type>` — a backup/portability artifact, not a document. Human-readable export (`.docx`, `.pdf`) is **cut**, and it is cut for an architectural reason, not a scheduling one. |
| **New version (`POST /content`)** | `409 CONFLICT`. A PUT into a CRDT-backed object would be silently discarded by the next merge, which is worse than refusing. |
| **Version history** | `Kind.Versioned = false` for collab types in v1. Their history is the update log, and a *named* version is a marker on it. Named snapshots are the natural v2, and the schema already supports several snapshots per document (015 keeps a small history deliberately). |
| **Quota accounting** | Cannot be a per-write update — writes are keystrokes. A `storage_accounting` job sums `octet_length(payload)` over `collab_updates` + `collab_snapshots` per workspace into `workspace_storage.collab_bytes`. Over quota sets a flag that `RoomHandler.Join` reads to grant read-only access (`RoomAccess.CanWrite=false`, already in the protocol at `room.go:51-54`). Enforcement is therefore *eventual* for these types, and that must be stated in the admin UI rather than discovered. |
| **Search content** | `Kind.Text` gets the snapshot, not the log — and only a type that ships a Go-side snapshot *reader* can implement it. For v1 collab types index **title only**, which is what `FileDoc` already does for blobs (`doc.go:323-339`). Full-text search inside documents arrives with the Docs phase, which is the phase that owns the block model. |
| **Copy / duplicate** | Copy the head snapshot into a new `collab_documents` row with `head_seq = 0`. Do **not** copy the update log: sequence numbers are per-document and the log's provenance (`actor_id`) would be wrong. Copy of a blob is a server-side object copy. |
| **Purge** | The `ON DELETE CASCADE` added to `collab_documents.resource_id` in 017 makes this correct by construction. Without the FK, purging a file would leave the log — user-content Postgres bytes with nothing pointing at them, invisible to the object GC because they are not objects. |

**How I would attack it.** Build the blob path first and completely, then add
`StorageMode` and drive one real collab type through every endpoint before Docs
starts. The type to use is a **stub `document` Kind** that creates a
`collab_documents` row and nothing else — no editor, no client component. Ten lines
of Go, and it forces every Drive endpoint to answer the collab question in code
rather than in a design doc: what does trash do, what does copy do, what does the
usage figure say, what does `GET /content` return, what does `POST /content`
return. Anything that only works because "the file has bytes" fails loudly against
the stub, in Phase 1, where fixing it costs a day. Discovered in Phase 3 it costs a
schema change plus three editors' worth of client code.

The second-order trap worth naming: the temptation to give collab types a
"materialized blob" that the server keeps up to date, so everything else can pretend
they are normal files. It cannot be kept up to date without server-side merge. A
stale materialization that *looks* like the file is how you ship a Drive that hands
users a three-day-old copy of their document and calls it a download.

---

## 9. Sequencing

**1. Foundation (blocking, ships alone).** Migration 017; `internal/storage`
extracted with `internal/file` rewired onto `Backend`; **the two GC predicate
fixes**; root-folder backfill. No user-visible change, one very visible regression
test. Nothing else starts until this is merged, because everything else writes rows
that the current GC would eat.

**2. In parallel, once (1) is merged:**

- **2a. Folder tree + list/move/rename** — needs Plan 00's `acl_object` and
  `Move`. Drive is the first package to cut over to the new checker.
- **2b. Registry + new-from-type + open descriptor + the stub `document` Kind.**
  Small, and it should land *early* even though it looks like a Phase 3 concern:
  publishing the descriptor shape unblocks Docs planning, and the stub is what
  keeps §8 honest.
- **2c. Storage backend selection, presigning, boot validation, admin test.**
  Independent of the schema.

**3. After 2a: versions, trash + purge job, quota + accounting job.** These share
the file repository and are cheapest done by one person in sequence.

**4. After 2a, blocked on Plan 00's link-subject decision: sharing.** Grants first
(they are a thin wrapper over `authz` and can ship the week 2a does), links second.

**5. Independent, any time after (1): previews.** A `thumbnailer` durable consumer.
Images only, via `image/jpeg`,`image/png`,`image/gif` from the standard library plus
`golang.org/x/image` for WebP decoding — the one new dependency in this plan, from
the Go extended standard library, decode-only, no cgo. Decode `image.DecodeConfig`
**first** and reject anything over a megapixel budget before allocating, or a 20KB
PNG becomes a 10GB allocation in the worker. SVG is never rendered and never served
inline (it is already excluded from `inlineTypes`, `handler.go:60-74`).

**6. Client (React Native).** Can start against the frozen API shapes from step 2
and runs the whole length of the phase. Web-first per ROADMAP §3: full browser and
preview pane on web; mobile gets list, upload, download and share.

**Long pole: the client.** Not because it is hard, but because a file browser is
tree navigation, drag-move, upload progress, a preview pane, a sharing dialog and a
version panel, and it is one surface that cannot be split across people cleanly.

**Critical path: the registry (2b).** It is small and it is the only item three
later phases cannot start without. Ship it early and review it as a contract.

---

## 10. Risks and failure modes

**The GC eats Drive.** Covered above; first commit, with a regression test that
fails if the predicate is reverted. Rated first because it is silent, delayed by 24
hours, and logs as success.

**The bucket sweep eats old versions.** Same PR, same reasoning.

**`acl_key` fan-out on a folder move.** Moving a folder with 20k descendants is one
`UPDATE` on `acl_object.path` (Plan 00's whole reason for a materialized path) but
it is 20k `acl_key` rewrites *and* 20k search reindexes. In a request handler that
is a multi-second transaction holding locks. Design: the handler rewrites the path
and enqueues one `drive.subtree_moved` event; a worker consumer expands it in
batches, in the same shape as `purgeRetentionBatch`
(`cmd/worker/main.go:971-1054`). The window where search results are stale for
moved descendants is real and acceptable; the window where *authorization* is stale
is not, which is why `acl_key` is rewritten transactionally and only the index lags.

**Move cycles.** Moving a folder into its own descendant produces an unreachable
subtree. With the materialized path it is a prefix check, and it must be inside the
same transaction as the update. Also cap depth at 32 per Plan 00.

**Presigned URL leakage.** The URL *is* the grant for its TTL. 5-minute TTL,
`attachment` disposition forced, never logged (we log paths only,
`middleware.go:70`), and a revoked share does **not** invalidate an already-issued
URL — which is a real, permanent property of presigning and belongs in the
operator docs, not in a hope that nobody notices.

**Quota confusion at the support desk.** Trash and old versions consume quota; users
will not expect it. Show the breakdown in the usage endpoint (`bytes_used`,
`trashed_bytes`, `version_bytes`, `collab_bytes`), so the first question answers
itself.

**Quota drift.** `workspace_storage` is denormalized state maintained
transactionally by uploads and asynchronously for collab bytes. The accounting job
recomputes and reports mismatches, exactly as Plan 00 requires for `acl_key`. Drift
here is a billing/capacity bug, not an access-control one, so reporting beats
blocking.

**Upload path at scale.** Every byte still transits the Go process on upload even
after read-path presigning. `multipartMemory` is already tuned for this
(`handler.go:26-30`, the comment records the OOM that produced it). The real fix is
presigned PUT: issue an upload ticket and a `files` row in a pending state, let the
client PUT directly to storage, then `POST .../complete` where the server `Head`s
for the true size and range-`Get`s the first 512 bytes to sniff the type — the
sniffing at `handler.go:95-105` is a security control and must not be skipped just
because the bytes did not pass through us. `PresignPut` is in the `Backend`
interface for this reason; the flow is the **first thing to cut** if the phase runs
long, because the proxy path is correct, merely expensive.

**Trash restore into a deleted parent.** Restore walks up; if any ancestor is
trashed or purged, restore lands the object in the workspace root and says so.

**The `files` table now serves two lifecycles.** A single-purpose table gaining a
second purpose is how the `message_id IS NULL` ambiguity happened in the first
place. Mitigation is a repository boundary: nothing outside `internal/drive` and
`internal/file` writes `files`, and every predicate touching `folder_id`/
`message_id`/`trashed_at` names all three.

---

## 11. Verification

The suite is `backend/test/integration`, build tag `integration`, real
Postgres/Redis/NATS with optional MinIO/Meilisearch, skipping locally but
**failing under CI** (`harness_test.go:1-21`). Everything below goes there unless
noted.

**`drive_gc_test.go` — the regression that justifies the phase.** This is the file
to write first, before the feature:

1. A file with `folder_id` set and `message_id NULL`, created 48 hours ago, is
   **still present** after `runObjectGC`, in both Postgres and the bucket.
2. A file with three versions: the two non-head version objects **survive** the
   bucket sweep.
3. A chat upload with neither `folder_id` nor `message_id`, past the grace period,
   is still collected — the existing behaviour must not regress in the other
   direction.

This needs `runObjectGC` to be callable from a test. It currently lives in
`package main` (`cmd/worker/main.go:1121`) and is only reachable from
`cmd/worker/main_test.go`. Move it into `internal/file` (or `internal/drive/gc.go`)
and have `cmd/worker` call it — a small refactor with a concrete reason.

**`drive_test.go`** — folder create/rename/move with breadcrumbs; children listing
paged with a real cursor across a page boundary; move-into-own-descendant rejected;
depth cap; upload → new version → list versions → restore version → head reflects
it; trash → list trash → restore → purge; quota rejection returns 507 and the row
count is unchanged; usage figure matches a recomputation.

**`drive_registry_test.go`** — for every registered `Kind`: create → open descriptor
has the right `storage_mode`, `content_url`/`collab_document_id` are exactly one of
null; `POST /content` on a collab type is 409. Plus the golden test on
`GET /drive/registry` so an editor phase that changes the contract has to change the
golden file deliberately. Plus the unit assertion that every `Kind.Type` is a valid
`search.ObjectType`.

**`tenancy_test.go` additions** — the existing tests
(`TestCrossTenantSearch`, `TestCrossChannelIDOR`, `TestWorkspaceRemovalRevokesAccess`)
must pass unchanged. New attacks, written as the attack: read another tenant's file
by uuid; move a file into another tenant's folder; a share link scoped to workspace
A resolving against an object in workspace B; a presigned URL for a key belonging to
another tenant; a revoked grant's `RevokeObject` actually closing the collab room.

**Storage conformance** — a table-driven suite run against every configured
`Backend` (MinIO in CI; S3 skipped unless credentialed) covering put/get/head/
delete/list/presign, including presign of a key that does not exist and delete of a
key that does not exist (must be idempotent — `runObjectGC` depends on it).

**Unit, in-package** — thumbnail decoding against a decompression-bomb fixture
(assert it is rejected before allocation); path prefix and cycle detection; the
quota `UPDATE ... RETURNING` under two concurrent transactions.

---

## 12. Sizing

| Piece | Size | Note |
|---|---|---|
| Migration 017 + `files` reshape + GC fixes | **M** | Small diff, highest consequence |
| `internal/storage` extraction + presigning + boot validation | **M** | Mostly a move; presigning is the new part |
| Folder tree, list, move, rename | **M** | Cheap *because* Plan 00 owns the hierarchy |
| Registry + new-from-type + open descriptor + stub Kind | **S** | Smallest item, largest blast radius |
| Versions | **M** | |
| Trash + purge job | **S** | Fifth job loop, existing scaffolding |
| Quota + accounting job | **S–M** | The collab-bytes half is the awkward half |
| Sharing: grants | **S** | Wrapper over `authz` |
| Sharing: links | **M** | Blocked on Plan 00's link-subject decision |
| Previews (thumbnailer consumer) | **M** | One new dependency (`golang.org/x/image`) |
| Search wiring (`folder_id`, ACL keys, reindex consumer) | **S** | `FileDoc` already exists |
| Presigned PUT upload flow | **M** | First thing to cut if the phase runs long |
| Client (RN, web-first) | **L** | |

**Phase total: M–L**, against ROADMAP §5's M — the delta is the registry and the
collab-type semantics, which §5 did not price because §3b was written later.

**Long pole: the client.** **Critical path: the registry.** They are different
things and the plan is scheduled around both.

---

## 13. Cuts, and why

| Cut | Why |
|---|---|
| Desktop sync client | ROADMAP §7. A product in itself. |
| PDF/Office/video previews | Needs ImageMagick/ffmpeg/LibreOffice in the image: a CVE surface and an ops category, for a thumbnail. |
| Server-side export of collab types to `.docx`/`.pdf`/`.xlsx` | Requires server-side CRDT interpretation, which migration 015 refuses on correctness grounds. Not deferred — blocked, deliberately. |
| Full-text extraction inside documents | Belongs to the phase that owns the block model. Title-only until then, which is what blobs already get. |
| Named version history for collab types | The log is the history in v1. The schema supports it; the UI does not need to. |
| Native GCS backend | A new dependency for a case S3 interop already covers. Named, priced, not taken. |
| Per-file comments | Phase 2 builds the comment surface once. Two comment systems is the §1 failure. |
| Content-hash dedup | Breaks version and quota accounting, needs refcounting, saves little at this scale. |
| Unique names within a folder | Fights restore and copy; Drive-style duplicates are the expected behaviour. |
| Anonymous write links | The abuse surface of anonymous write is not worth v1. |
| A real "Shared with me" tree | Flat list in v1. The tree is a rendering problem over grants that may span workspaces, and it is not worth solving before anyone has used sharing. |
| Client-side encryption | Would make previews, search and server-side copy impossible. A different product. |
