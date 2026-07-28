# SuperOps

Free, self-hosted workspace suite for organizations of any size — chat, a file
drive with collaborative documents, work tracking, automation, a shared email
inbox and huddles, in one deployment you own completely.

A React Native (Expo) client for **iOS, Android and the web**, and a Go backend
that deploys with Docker Compose or Kubernetes.

It started as a Slack/Mattermost alternative and the messaging core is still the
centre of it, but the product is no longer only a messenger: `internal/` carries
35 packages and the API is 201 route registrations across ten product areas.
Everything below was checked against the tree rather than remembered, and where
something is built but not reachable, or built but not finished, this file says
so — see [Limitations](#limitations) and [`docs/KNOWN-GAPS.md`](docs/KNOWN-GAPS.md).

## Features

**Messaging**
- Real-time channels (public/private) over WebSocket, threaded replies, DMs (1:1 and group)
- Emoji reactions, custom emoji, message editing and soft deletion
- Pinned messages, bookmarks, scheduled messages, forwarding
- Keyset-paginated history, per-channel unread counts and read tracking
- File sharing with inline preview, user presence and typing indicators
- User blocking, incoming webhooks

**Drive and documents**
- A per-workspace folder tree, shared with the whole workspace by one grant, so a
  new member sees the Drive without anyone sharing anything
- Uploads to 100 MB with server-side content sniffing, version history (list,
  download any version, restore one), trash with subtree restore, storage quotas
- Per-object sharing on folders, files, mailboxes and conversations at five
  capabilities (read/comment/write/share/admin), inherited down the tree
- **Collaborative documents, spreadsheets and design canvases** — file kinds whose
  content is a CRDT log rather than bytes in a bucket. The server stores and fans
  out opaque updates it never interprets; merging, snapshots and searchable text
  all come from the clients
- Embed refs between objects resolved per caller, plus backlinks
- Image thumbnails generated in the background

**Work tracking**
- Projects with a short key (`PROJ-14`), a gapless per-project issue counter and
  five fixed workflow states
- One drag-orderable issue list whose order the server computes, assignment
  notifications and audit entries

**Comments** — one product-wide thread attachable to any object the permission
system knows (issue, file, folder, channel, project, mailbox, conversation), with
one level of replies, @-mentions, tombstoning deletes, and — on documents — an
anchor pinning a comment to a range of text.

**Automation** — "when this happens, do these things", saved by a workspace admin
and executed by the worker under the authority of whoever saved it last, with a
per-run history of what ran and why anything was refused.

**Shared email inbox** — customer mail arrives on domains you own and threads into
conversations your team triages and replies to. Outbound goes through a pluggable
transport: log, SMTP relay, direct-to-MX with DKIM signing, or Resend.

**Huddles** — live audio and screen share attached to a channel. The server owns
whether a call exists and who may speak; an external LiveKit media server you run
owns the audio. Off unless you configure it.

**Search and inbox**
- Full-text search (Meilisearch) spanning what you are allowed to read, filtered
  by ACL keys in the query rather than after it
- A unified inbox that chat, comments, issues and workflows file into, coalesced
  per subject (forty mentions in one channel are one row saying 40), with per-kind
  delivery preferences and a batched email digest
- Mobile push via Expo (opt-in — see [Push notifications](#push-notifications))

**Administration**
- Admin dashboard (stats, users, audit logs, invitations), every endpoint scoped
  to the workspaces you administer
- Role-based access control (owner/admin/member/guest) over an object-level ACL
- TOTP two-factor authentication with single-use backup codes
- Per-workspace OpenID Connect SSO with just-in-time provisioning, account
  linking on proof of password, IdP-group-to-role mapping and enforcement
- Redis sliding-window rate limiting
- Audit logging: two-tier writes, hourly coalescing of repeat reads, monthly
  partitions with DROP-based retention, a per-workspace hash chain and off-box
  anchoring — see [Audit trust boundary](#audit-trust-boundary)

**Infrastructure**
- Horizontal scaling with a NATS-bridged WebSocket hub — no sticky sessions
- JetStream durable consumers for indexing, notifications, mail and automation
  (at-least-once, with a poison-message path)
- Auto-scaling via Kubernetes HPA, PodDisruptionBudget, NetworkPolicy
- Prometheus metrics including a request-latency histogram and pgxpool saturation

## What is optional

Several areas disappear entirely without configuration, and the routes are not
registered rather than failing at call time — a client can probe a 404 and hide
the UI, which is what the shipped one does. Know this before you conclude a
feature is broken.

| Area | Needs | Without it |
|---|---|---|
| **Search** | `SEARCH_ENABLED` (default on) + a reachable `MEILI_HOST` | The search route is not registered and the indexer consumers do not start. The API still boots. |
| **File uploads** | `FILES_ENABLED` + object storage that opens | Chat-attachment routes are not registered. Drive routes *are* still registered and answer `503 STORAGE_NOT_CONFIGURED` on upload. |
| **Huddles** | `RTC_HOST` + `RTC_API_KEY` + `RTC_API_SECRET`, and a LiveKit server you run | All four routes unregistered; the client's huddle bar renders nothing. |
| **SSO** | `SSO_SECRET_KEY` (32 bytes) | All ten SSO routes 404. A key that is set but wrong-sized fails startup. |
| **Push** | `PUSH_ENABLED=true` + your own Expo project credentials | The two device routes are not registered — deliberately, so no token is stored that nothing will send to. |
| **Automation, mail delivery, indexing, digests, thumbnails** | `cmd/worker` running | The API authors workflows and accepts mail but nothing executes, delivers, indexes or previews. An API-only deployment is not a working deployment. |
| **Realtime, at any size** | NATS — a hard dependency, not a scaling add-on | Every realtime event round-trips through NATS even on one replica, and `/ready` answers 503 while it is unreachable. Extra replicas need nothing further: the relay is already what makes them see each other's events. |

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Client | React Native 0.83 + React 19.2 + Expo 55, TypeScript |
| Backend | Go 1.25 (net/http, no framework) |
| Database | PostgreSQL 16+ |
| Cache | Redis 7+ |
| Search | Meilisearch |
| Storage | MinIO (S3-compatible) |
| Messaging | NATS + JetStream |
| WebSocket | coder/websocket + NATS bridge |
| CRDT | Yjs, in the client — the server never interprets an update |
| Media (optional) | LiveKit, run separately by you |

## Supported Platforms

| Platform | Method |
|----------|--------|
| iOS | `npx expo run:ios` |
| Android | `npx expo run:android` |
| Web | `npx expo start --web` (dev) · static export via `deploy/docker/Dockerfile.frontend` (prod) |

Huddles can be **started and ended** from every platform but **joined only on the
web** — the native huddle screen says so rather than pretending.

## Quick Start

### 1. Start the backend

```bash
git clone https://github.com/Wick-Lim/SuperOps.git
cd SuperOps

./scripts/setup.sh          # needs go, node and docker on the host: installs deps,
                            #   writes deploy/docker/.env with real random secrets,
                            #   starts the infra stack and migrates
make docker-up              # API on http://localhost:8081, web client on http://localhost:8080
```

`setup.sh` generates every secret; do not ship `.env.example` values. `JWT_SECRET`
must be at least 32 bytes, and malformed configuration values fail startup rather
than silently falling back to a default.

### 2. Run the app

```bash
cd app
npm install
npx expo start              # 'i' for iOS simulator, 'a' for Android emulator, 'w' for web
```

Point the client at a non-default backend with `EXPO_PUBLIC_API_URL`.

### Development (backend on the host)

```bash
./scripts/setup.sh          # once — writes deploy/docker/.env
make docker-dev             # infrastructure only (Postgres, Redis, NATS, MinIO, Meilisearch)
make migrate
make dev                    # runs cmd/superops on the host with the generated credentials
make app-dev                # in another terminal
```

`make dev`, `make migrate` and `make seed` all read `deploy/docker/.env`, so
there is no shell-variable prefix to get wrong. Run `cmd/worker` too if you want
automation, mail, search indexing, digests or thumbnails.

### Kubernetes

```bash
helm install superops deploy/k8s/helm/superops \
  --set admin.email="you@example.com" \
  --set admin.password="..." \
  --set jwt.secret="at-least-32-bytes-of-random" \
  --set postgresql.auth.password="..." \
  --set redis.auth.password="..." \
  --set minio.auth.rootPassword="..." \
  --set global.domain="chat.example.com"
```

Five of those **fail at render time** rather than CrashLooping when omitted:
`jwt.secret`, `admin.password`, `postgresql.auth.password`, `redis.auth.password`
and `minio.auth.rootPassword`. `admin.email` and `global.domain` have working
defaults (`admin@example.com`, `superops.example.com`) — set them anyway, or you
get an admin account and an ingress host you did not choose.

**Meilisearch is not a chart dependency.** Point the chart at one you run:

```bash
  --set search.enabled=true \
  --set externalMeilisearch.host="http://meilisearch.search.svc.cluster.local:7700" \
  --set externalMeilisearch.masterKey="..."
```

Includes: backend HPA, worker and frontend Deployments, a migration Job, ingress
with WebSocket support, PDB, NetworkPolicy and a ServiceAccount.

## Architecture

```
  ┌──────────────┐
  │    Client    │  React Native (iOS / Android / Web)
  └──────┬───────┘
         │ REST + WebSocket
         ▼
┌─────────────────────────────────────────────────┐
│                Kubernetes / Docker               │
│                                                   │
│  ┌──────────┐    ┌──────────┐    ┌─────────┐    │
│  │Backend-1 │◄──►│   NATS   │◄──►│  Redis  │    │
│  │  (Hub)   │    │ JetStream│    │         │    │
│  ├──────────┤    │          │    │         │    │
│  │Backend-N │◄──►│          │    │         │    │
│  │  (Hub)   │    └────┬─────┘    └─────────┘    │
│  └──────┬───┘         │                          │
│         │        ┌────▼─────┐    ┌─────────┐    │
│  ┌──────▼───┐    │  Worker  │    │  MinIO  │    │
│  │PostgreSQL│    │(indexer, │    │  (S3)   │    │
│  │          │    │ notifier,│    │         │    │
│  │          │    │ mail,    │    │         │    │
│  │          │    │ workflow)│    │         │    │
│  └──────────┘    └──────────┘    └─────────┘    │
│                  ┌──────────┐                    │
│                  │Meilisearch│                   │
│                  └──────────┘                    │
└─────────────────────────────────────────────────┘
```

**Multi-replica WebSocket delivery.** Each backend instance runs a local
WebSocket hub plus an event relay. Application events (messages, reactions,
notifications) are published once to NATS as
`superops.{workspace}.{resource}.{action}`; every replica's relay receives the
event and delivers it only to its own locally-connected clients. Because a
client holds exactly one socket, that is exactly-once per client with no sticky
sessions. Client-originated ephemeral events (typing) take a separate
`ws.broadcast.{channel}` path with an origin-instance guard so the publishing
replica does not double-deliver. Collaborative editing adds `ws.room.>` with the
same shape.

**The worker is not optional in practice.** `cmd/superops` serves the API;
`cmd/worker` binds the JetStream durable consumers (search indexing, inbox
fanout, mail delivery, thumbnails, workflow triggers) and runs the scheduled
jobs (digests, retention, trash purge, session cleanup, projection repair,
huddle reconciliation). An API-only deployment accepts writes that nothing acts
on.

**Durability.** Durable pull consumers with explicit ack, bounded redelivery and
a terminal path for poison messages. The WebSocket relay stays on core NATS — it
is deliberately fire-and-forget fan-out.

## Project Structure

```
SuperOps/
├── app/                        # React Native (Expo)
│   ├── src/
│   │   ├── api/                # REST modules per area
│   │   ├── stores/             # Zustand (auth, channel, message, workspace, ui, user)
│   │   ├── lib/                # types, websocket manager, errors, secure storage, theme, push
│   │   ├── screens/            # Login, Invite, Onboarding, Workspace, Search, Members,
│   │   │                       #   Notifications, Settings, Admin, NewChannel, NewDM,
│   │   │                       #   Pinned, Bookmarks, Scheduled, ChannelDetail,
│   │   │                       #   Drive, DriveShare, Board, Inbox (mail)
│   │   ├── components/         # channel/, message/, a11y, HuddleBar
│   │   ├── editor/ sheet/ design/  # the three collaborative surfaces (Yjs)
│   │   ├── huddle/             # HuddleRoom.web / HuddleRoom.native
│   │   ├── navigation/         # AppNavigator (single stack, auth-gated groups)
│   │   └── config.ts
│   └── App.tsx
├── backend/                    # Go API server
│   ├── cmd/
│   │   ├── superops/           # API server
│   │   ├── worker/             # JetStream consumers + scheduled jobs
│   │   ├── migrate/            # Database migrations
│   │   ├── seed/               # Demo data
│   │   └── reindex/            # Rebuild the Meilisearch index from Postgres
│   ├── internal/               # 35 packages
│   │   ├── app/                # Composition root, config, metrics
│   │   ├── authz/ rbac/        # Object-level ACL checks; route-level role middleware
│   │   ├── auth/ sso/          # JWT, sessions, TOTP, invites; OpenID Connect
│   │   ├── user/ workspace/ channel/ message/
│   │   ├── ws/ presence/       # WebSocket hub, NATS bridge/relay, presence
│   │   ├── drive/ storage/ quota/ thumb/   # Files, folders, sharing, versions, previews
│   │   ├── collab/             # CRDT log, snapshots, rooms
│   │   ├── comment/            # Product-wide comment threads and document anchors
│   │   ├── issue/              # Projects and issues
│   │   ├── workflow/           # Automation authoring; the engine lives in cmd/worker
│   │   ├── huddle/ rtc/        # Call lifecycle; LiveKit token minting
│   │   ├── mail/ mailbox/      # Outbound transports; shared inbox, domains, ingest
│   │   ├── inbox/ notification/ push/      # Unified inbox, compat projections, Expo push
│   │   ├── file/ search/ webhook/ emoji/ block/
│   │   ├── admin/ audit/ ratelimit/
│   ├── pkg/                    # authctx, crypto, database, httputil, logger, nats, redis
│   ├── migrations/             # SQL migrations (000–064; the numbering is sparse)
│   └── test/integration/       # Build-tagged suite against real infrastructure
├── deploy/
│   ├── docker/                 # Compose, Dockerfiles, nginx, TLS overlay, Prometheus
│   └── k8s/helm/superops/      # Helm chart
├── docs/                       # ROADMAP, KNOWN-GAPS, per-area plans
├── scripts/                    # setup.sh, backup.sh, restore.sh
└── .github/workflows/          # CI (lint/test/build), Release (Docker images)
```

## API Reference

201 route registrations — 198 under `/api/v1`, plus `/health`, `/ready` and
`/metrics` at the root. Some are conditional; see [What is optional](#what-is-optional).

Response envelope:

```json
{"data": {...}, "meta": {"cursor": "...", "has_more": true}}
```

`meta` and `error` are omitted when empty — a successful response has no `error`
key at all, and an error response is `{"data": null, "error": {"code","message"}}`.

Request bodies **reject unknown fields**. A `U+0000` anywhere in a decoded body
is stripped rather than refused, because no Postgres column can store one and a
400 naming an invisible character is not actionable — except on the workflow
authoring routes, where a body's strings are map KEYS and stripping one would
change which key the executor reads, so those refuse and name the step. A path
parameter named `{*_id}` must be a UUID; a value that is not one is a 400 naming
the parameter, not a 500 from the failed cast.

`test/integration/contract_test.go` is what keeps the client and the server
agreeing about all of this.

### Messaging core

| Group | Key Endpoints |
|-------|--------------|
| **Auth** | `POST /auth/login`, `/refresh`, `/logout`, `/accept-invite`, `/change-password`, `GET /auth/invite/{token}` |
| **2FA (TOTP)** | `GET /auth/totp/status`, `POST /auth/totp/setup`, `/verify`, `/disable` |
| **Users** | `GET /users/me`, `PATCH /users/me`, `PUT /users/me/status`, `GET /users/{user_id}`, `/users/search?q=` |
| **Devices (push)** | `POST /users/me/devices`, `DELETE /users/me/devices/{token}` — registered only when `PUSH_ENABLED=true` |
| **Workspaces** | `POST /workspaces`, `GET/PATCH/DELETE /workspaces/{workspace_id}`, `GET .../members`, `PATCH`/`DELETE .../members/{user_id}`, `POST .../transfer-ownership`, `GET .../presence` |
| **Channels** | `POST/GET /workspaces/{workspace_id}/channels`, `/browse`, `GET/PATCH .../channels/{channel_id}`, `/join`, `/leave`, `/archive`, `/unarchive`, `GET/POST .../members`, `DELETE .../members/{user_id}` |
| **Direct messages** | `POST /workspaces/{workspace_id}/channels/dm` (1:1 idempotent + group) |
| **Messages** | `POST/GET /channels/{channel_id}/messages`, `GET/PATCH/DELETE .../{message_id}`, `POST .../{message_id}/forward`; `file_ids` and `scheduled_at` accepted on send |
| **Threads** | `GET/POST /messages/{message_id}/thread` (paginated) |
| **Reactions** | `POST /channels/{channel_id}/messages/{message_id}/reactions`, `DELETE .../reactions/{emoji}`, `GET /messages/{message_id}/reactions` |
| **Pins / Bookmarks** | `POST/DELETE /channels/{channel_id}/messages/{message_id}/pin`, `GET /channels/{channel_id}/pins`, `POST/DELETE /messages/{message_id}/bookmark`, `GET /bookmarks` |
| **Scheduled** | `GET /channels/{channel_id}/scheduled`, `DELETE .../scheduled/{message_id}` |
| **Read state** | `PUT /channels/{channel_id}/read`, `GET .../unread`, `PATCH .../prefs` |
| **Typing** | `GET /channels/{channel_id}/typing` |
| **Chat files** | `POST /files/upload` (multipart), `GET/DELETE /files/{file_id}` — registered only when object storage opened |
| **Blocks / Emoji** | `GET/POST /blocks`, `DELETE /blocks/{blocked_id}`, `GET/POST /workspaces/{workspace_id}/emojis`, `DELETE .../emojis/{emoji_id}` |
| **Webhooks** | `POST/GET /webhooks`, `PATCH/DELETE /webhooks/{webhook_id}`, `PUT .../token` (rotate), `POST /webhooks/incoming` (+ a legacy `/incoming/{token}` form) |

### Drive, documents and comments

| Group | Key Endpoints |
|-------|--------------|
| **Drive** | `GET /drive/registry`, `GET /workspaces/{workspace_id}/drive/root`, `POST .../drive/folders`, `POST .../drive/files` (new from the registry), `POST .../drive/files/upload` (multipart, 100 MB), `GET .../drive/usage`, `PUT .../drive/quota`, `GET/DELETE .../drive/trash` |
| **Folders** | `GET /drive/folders/{folder_id}`, `/children` (keyset; ordered by `kind` ascending, so files list before folders), `PATCH`, `POST .../move`, `DELETE` (trash) |
| **Files** | `GET /drive/files/{file_id}` (open descriptor), `PATCH`, `POST .../move`, `DELETE`, `POST .../content` (new version) |
| **Versions** | `GET /drive/files/{file_id}/versions`, `GET .../versions/{version}/content`, `POST .../versions/{version}/restore` |
| **Trash** | `POST /drive/{object_type}/{object_id}/restore` (subtree) |
| **Sharing** | `GET/PUT /drive/{object_type}/{object_id}/shares`, `DELETE .../shares/{subject_type}/{subject_id}`; `object_type` is folder, file, mailbox or conversation |
| **Share links** | `POST/GET /drive/{object_type}/{object_id}/links`, `DELETE /drive/links/{link_id}`, `POST /drive/links/{token}/resolve` — **see [Limitations](#limitations): resolving one grants nothing yet** |
| **Refs / backlinks** | `POST /drive/files/{file_id}/refs/resolve` (per caller, ≤100), `GET .../backlinks`, `GET /drive/refs/{ref_type}/{ref_id}/files` |
| **Projection** | `POST /drive/files/{file_id}/projection` — the client publishes the searchable text of a document the server cannot read |
| **Collaboration** | `POST /workspaces/{workspace_id}/collab/documents` (open), `GET /collab/documents/{document_id}`, `/state?since=`, `POST .../updates`, `POST .../snapshot` |
| **Comments** | `GET/POST /comments/{object_type}/{object_id}`, `GET /comments/{comment_id}/replies`, `PATCH/DELETE /comments/{comment_id}` |
| **Anchors** | `PUT /drive/files/{file_id}/comments/{comment_id}/anchor`, `GET /drive/files/{file_id}/anchors` |

### Work, automation, mail and calls

| Group | Key Endpoints |
|-------|--------------|
| **Projects / Issues** | `GET/POST /workspaces/{workspace_id}/projects`, `GET /projects/{project_id}`, `/states`, `GET/POST /projects/{project_id}/issues`, `GET/PATCH /issues/{issue_id}`, `POST /issues/{issue_id}/move` |
| **Workflows** | `GET/POST /workspaces/{workspace_id}/workflows`, `GET/PATCH/DELETE /workflows/{workflow_id}`, `GET .../runs`, `/rejections`, `GET /runs/{run_id}`, `GET /workflow-steps` (any authenticated caller) — reading a workflow, its runs and its rejections requires workspace **admin** |
| **Mailboxes** | `GET/POST /workspaces/{workspace_id}/mailboxes`, `GET /mailboxes/{mailbox_id}/conversations`, `GET/PATCH /conversations/{conversation_id}`, `POST .../reply` |
| **Mail domains** | `GET/POST /workspaces/{workspace_id}/mail/domains`, `POST /mail/domains/{domain_id}/verify`, `DELETE /mail/domains/{domain_id}` |
| **Mail ingest** | `GET/POST /workspaces/{workspace_id}/mail/ingest-tokens`, `DELETE /mail/ingest-tokens/{token_id}`, `POST /mail/inbound` (SuperOps' own JSON shape, ≤2 MiB) |
| **Huddles** | `POST/GET/DELETE /channels/{channel_id}/huddle`, `POST /rtc/webhook` — the three channel routes only when a media server is configured; the webhook additionally needs `RTC_WEBHOOK_SECRET`, which boot validation does not require |

### Search, inbox and administration

| Group | Key Endpoints |
|-------|--------------|
| **Search** | `GET /workspaces/{workspace_id}/search?q=&channel=&from=&type=&limit=` — registered only when Meilisearch is reachable |
| **Inbox** | `GET /inbox`, `/inbox/count`, `/inbox/{item_id}/events`, `PUT /inbox/{item_id}/read\|unread\|done\|undone`, `POST /inbox/read-all`, `GET/PUT /notification-prefs` |
| **Notifications** (compat) | `GET /notifications`, `PUT /notifications/{notification_id}/read`, `/read-all`, `GET .../unread-count` — projections of the inbox, and what the shipped client actually calls |
| **SSO** | `GET /auth/sso/workspace/{workspace_slug}`, `POST /auth/sso/start`, `/callback`, `/totp`, `/link`; `GET/PUT/DELETE /workspaces/{workspace_id}/sso`, `POST .../sso/enforcement`, `.../sso/verify` |
| **Admin** | `GET /admin/stats`, `/admin/users`, `PATCH /admin/users/{user_id}`, `GET/POST /admin/invitations`, `POST /admin/storage/test`, `POST /admin/mail/test` |
| **Audit** | `GET /admin/audit-logs` (filters: `actor_id`, `action` — trailing `.` is a prefix match — `resource_type`, `resource_id`, `from`, `to`), `GET /admin/audit-logs/export` (NDJSON), `GET /admin/audit-logs/verify`, `POST /admin/audit-sink/test` |
| **WebSocket** | `GET /api/v1/ws?token={jwt}` |
| **Ops** | `GET /health` (liveness, no dependencies), `GET /ready` (Postgres/Redis/NATS), `GET /metrics` |

Authentication is `Authorization: Bearer <jwt>`. A JWT may be passed as
`?token=` **only** on the WebSocket and file-download routes, which cannot set a
header. `/metrics` also accepts `?token=`, but that is `METRICS_TOKEN` rather
than a JWT — worth knowing if you audit where credentials can appear in access
logs.

## WebSocket Protocol

```
ws://host/api/v1/ws?token={jwt}
```

Frame: `{"type": "event_type", "seq": 1, "data": {...}}`. `seq` is per-connection
and strictly monotonic on every frame, so a gap means backpressure dropped
frames and the client should refetch — there is no server-side replay.

| Direction | Events |
|-----------|--------|
| Client | `ping`, `subscribe`, `unsubscribe`, `typing.start`, `typing.stop`, `presence.update`, `collab.join`, `collab.leave`, `collab.update`, `collab.awareness` |
| Server | `hello`, `pong`, `subscribed`, `unsubscribed`, `error`, `message.new`, `message.updated`, `message.deleted`, `reaction.added`, `reaction.removed`, `channel.created`, `channel.updated`, `member.joined`, `member.left`, `typing.indicator`, `presence.changed`, `notification.new`, `unread.update`, `huddle.started`, `huddle.ended`, `huddle.roster`, `collab.joined`, `collab.left`, `collab.update`, `collab.awareness`, `collab.compact`, `collab.project` |

Two server frames are REQUESTS, not notifications, and a client that ignores
them degrades the product rather than merely missing an update:

- `collab.compact` asks one client in a document's room for a snapshot. The
  server cannot merge CRDT state, so nothing else stops a long-lived
  document's log from growing forever.
- `collab.project` asks for the document's searchable text. Same reason: the
  server never interprets an update, so a document whose stored projection is
  behind the log can only be repaired by a client that has it in memory.

Both are addressed to ONE member per replica, chosen by connection id. A
read-only client should ignore both; the server would refuse its write.

`subscribe` is acknowledged with `subscribed`, so a client need not race the
upgrade. `unsubscribed` carries a reason (`client` or `revoked`); on `revoked`
the server has removed the caller's access and the client should drop the
channel. `error` frames carry a `code`. The server sends WebSocket pings and
closes sockets that stop responding; clients do not need to send `ping`.

`collab.update` and `collab.awareness` have their own budget — 40/s with a burst
of 120, separate from the 5/s general one, which `collab.join` and `collab.leave`
still pay. One socket update is capped at 32 KiB decoded; the
HTTP append path takes 1 MiB and a snapshot 16 MiB.

## Configuration

All backend configuration is environment variables — see
[`deploy/docker/.env.example`](deploy/docker/.env.example) for the full list with
defaults. Notes that bite:

- `JWT_SECRET` must be **≥ 32 bytes**; startup fails otherwise.
- Malformed values (`SERVER_PORT=eighty`, `RATE_LIMIT_ENABLED=yes`) **fail
  startup** with every offending variable reported at once, instead of silently
  taking the default.
- `RATE_LIMIT_TRUST_PROXY` defaults to **false**. Behind a reverse proxy set it
  to `true` and `RATE_LIMIT_TRUSTED_PROXY_HOPS` to the number of proxies; the
  client IP is read that many entries from the **right** of `X-Forwarded-For`,
  which a client cannot forge past.
- `SEARCH_ENABLED` / `FILES_ENABLED` turn Meilisearch and MinIO off. When search
  is disabled or Meilisearch is unreachable the search route is not registered;
  when storage is disabled or unreachable the chat-attachment routes are not
  registered, while **Drive routes stay registered** and answer
  `503 STORAGE_NOT_CONFIGURED`.
- `DATABASE_URL` replaces the connection target (`DB_HOST`, `DB_PORT`, `DB_USER`,
  `DB_PASSWORD`, `DB_NAME`, `DB_SSLMODE`) — use it for a pooler, a multi-host DSN
  or `target_session_attrs`. The pool and timeout settings (`DB_MAX_CONNS`,
  `DB_MIN_CONNS`, `DB_APPLICATION_NAME`, `DB_STATEMENT_TIMEOUT`,
  `DB_LOCK_TIMEOUT`, `DB_IDLE_IN_TX_TIMEOUT`) still apply on top of it.
- `METRICS_TOKEN` guards `/metrics`. The bundled Prometheus already scrapes with
  it — `deploy/docker/prometheus.yml` reads the token from the `metrics_token`
  compose secret that `setup.sh` writes. A Prometheus outside this stack needs its
  own matching `authorization` block or scrapes will 401.
- `SSO_SECRET_KEY` is the AES-256-GCM key sealing every workspace's OIDC client
  secret. **Rotating it makes every stored secret unopenable** — the error says
  so by name. Absent, SSO is simply off.
- `RTC_HOST` / `RTC_API_KEY` / `RTC_API_SECRET` enable huddles, and setting some
  but not all of those three fails the boot. `RTC_WEBHOOK_SECRET` can never
  satisfy that check, only trip it: without it the boot succeeds and only the three
  channel routes register, leaving `POST /rtc/webhook` unmounted.
- `MAIL_TRANSPORT` (`log` / `smtp` / `smtp-direct` / `resend`) — the worker refuses to
  boot without a working one. Outbound replies additionally require a **verified**
  domain.
- `AUDIT_SINK` (`log` / `file` / `http`) chooses where audit chain anchors are shipped. A transport
  named without its credentials is a **boot failure** on both the backend and the worker. See
  [Audit trust boundary](#audit-trust-boundary).
- `AUDIT_RETENTION_DAYS` (default 365) is deployment-wide and enforced by **dropping monthly
  partitions**. `0` disables retention.
- `INBOX_DIGEST_QUIET_PERIOD` / `INBOX_DIGEST_MIN_INTERVAL` tune the inbox email digest.
- **Alert on `superops_audit_dropped_total`.** The Tier 2 audit buffer drops under pressure, by
  design and counted — and it fills exactly when the load is interesting.

Client configuration: `EXPO_PUBLIC_API_URL` (and optionally `EXPO_PUBLIC_API_PORT`
and `EXPO_PUBLIC_WS_URL`) — see [`app/src/config.ts`](app/src/config.ts) — plus
`EXPO_PUBLIC_EXPO_PROJECT_ID`, read in
[`app/src/lib/push.ts`](app/src/lib/push.ts).

## Audit trust boundary

`audit_logs` carries a per-workspace hash chain: a chained row hashes its own
contents plus its predecessor, so an in-place `UPDATE` or a `DELETE` is
**detectable**.

**Not every row is chained.** Only workspace-scoped, non-coalesced entries get a
`chain_seq`, a `prev_hash` and a hash; everything else is written loose, and
verification — which walks `WHERE workspace_id = $1 AND chain_seq > $2` — cannot
see it. Editing or deleting an unchained row leaves no gap and no break. That set
is exactly the rows this section leans on hardest: every authentication event
(`user.login`, `user.login_failed`, and the rest) carries no workspace, and both
`audit.read` and `file.downloaded` are coalesced. For those, tampering is not
detectable at all and they are never anchored off-box.
`GET /api/v1/admin/audit-logs/verify` walks it and reports breaks by sequence
number, and it answers 200 with the breaks in the body rather than 500 — a
corrupted audit log must not be a denial of service, or corrupting it becomes an
attack rather than something an attack leaves behind.

**Be clear about what that proves.** The chain lives in the same database as the
rows it protects. Anyone with `UPDATE` on `audit_logs` — which includes anyone
holding the application's database credentials — can edit a row and recompute
every hash after it. A chain guarded by an administrator with `psql` is
tamper-evidence theatre on its own.

It becomes real at exactly one point: when the chain head is shipped somewhere
that administrator cannot rewrite. `audit_chain_heads.anchored_seq` records how
far that has got, and the verify endpoint reports it beside `head_seq`:

- entries **at or below `anchored_seq`** are covered by a hash that exists off-box;
- entries **above it** are protected by nothing but a chain in a database an
  administrator can edit.

`AUDIT_SINK` chooses the transport (`log` into your existing log pipeline,
`file` onto an append-only volume, `http` to a SIEM with an HMAC-signed body).
The default is `log` because in most deployments the log pipeline already leaves
the host; **if yours does not, you do not have an anchor**, and the honest thing
is to say so rather than to point at the chain.

Three further layers, priced honestly:

- **No API surface mutates `audit_logs`.** There is none, and there must never be
  one — and there is no configuration that turns auditing off either, so it cannot
  be disabled from outside the source tree.
- **Reads of the audit log are themselves audited** (`audit.read`) — but coalesced
  hourly per (actor, action, resource), and the dedupe key does not include the
  filter. An administrator who lists broadly and then narrows within the same hour
  produces ONE row carrying the *first* filter and a count of 2: the count is
  exact, the filter list is not. `audit.exported` is written synchronously and
  uncoalesced.
- **Append-only at the database role is NOT implemented.** The intended shape is
  the application connecting as a role with `INSERT, SELECT` on `audit_logs` and
  no `UPDATE`/`DELETE`, with migrations and retention on a separate role. It is
  deferred, and the cost of deferring it is concrete: the application's own
  credentials are sufficient to rewrite the log, so the compromise of a backend
  pod — not just of a human administrator — is enough to edit everything **above**
  `anchored_seq`, which is everything the off-box anchor has not yet covered. Implementing it is not only a Go change: coalescing repeat
  reads is an `ON CONFLICT DO UPDATE`, and partition retention is a `DROP TABLE`,
  so the split needs a second connection string, a Compose service definition, a
  Helm Secret and an operator runbook for rotating two roles instead of one. It
  is tracked as the next thing to land in this area.

**Login events are not retrievable through the API.** `user.login` and
`user.login_failed` are written with a NULL workspace — correctly, since they
happen before a workspace is chosen — and both the list and the export are scoped
to administered workspaces. The highest-value audit signal is reachable only with
`psql`.

## Push notifications

Mobile push via [Expo's push service](https://docs.expo.dev/push-notifications/overview/),
which fronts APNs and FCM. **Off by default** — enabling it sends the first 140
characters of every notified message to Expo, and through Expo to Apple and
Google, which is a decision to make deliberately.

How it works: the client registers its Expo push token
(`POST /users/me/devices`), and the worker — which already creates inbox rows off
the JetStream events — enqueues a push to that user's devices on the same path.
Audience is identical to the in-app notification: per-channel `muted` and
`notification_pref`, blocks and self-exclusion all apply before push is reached.
A `DeviceNotRegistered` receipt deletes the token, so a reinstalled or rotated
device stops being retried.

Push is *not* suppressed when the recipient has a live WebSocket. The worker
could consult the Redis presence refcount, but that counts sockets per **user**,
not per device — a desktop tab left open would silence the phone. The
"is the user already looking at this?" decision is made on the client instead
(`app/src/lib/push.ts`), where being wrong costs a redundant banner rather than
a message nobody is told about.

**Turning it on is not sufficient for a notification to reach a phone.** You
also need, on your own Expo project:

1. An **APNs key** (iOS) and an **FCM service account** (Android) uploaded to the
   project — `eas credentials`. Without them Expo accepts every batch and
   answers `InvalidCredentials` per message, which the worker logs at `error`.
2. `extra.eas.projectId` in [`app/app.json`](app/app.json) (or
   `EXPO_PUBLIC_EXPO_PROJECT_ID` in the build environment), or
   `getExpoPushTokenAsync` cannot attribute the token and registration is
   skipped. **The repository does not carry one** — you must add yours.
3. A development build or a store build. Push tokens are not available in Expo
   Go, and web is not supported here at all — the web client already has the
   live WebSocket.

Server-side:

```bash
PUSH_ENABLED=true         # opens POST/DELETE /users/me/devices, starts the worker dispatcher
EXPO_ACCESS_TOKEN=...     # only if the Expo project has "enhanced push security" on
PUSH_TIMEOUT=15s          # bounds one request to Expo
```

On Kubernetes use `push.enabled` / `push.accessToken` / `push.timeout` in Helm
values. Note that the bundled NetworkPolicy allows the worker no egress except
DNS and in-release services, so with `networkPolicy.enabled=true` you must add a
443 rule via `networkPolicy.extraEgress` or the worker cannot reach Expo at all.

Sending is best-effort by construction: the inbox row is committed first and the
push is queued to an in-process dispatcher that drops rather than blocks when
full, because stalling on a third party inside a JetStream consumer callback
would stall event acking. Redelivery does not double-buzz — the push is sent only
when the row was genuinely new.

## Testing

```bash
# Backend unit tests
make backend-test                  # go test ./... -race

# Backend integration tests — drive the fully-wired app over httptest against
# real Postgres/Redis/NATS (and Meilisearch/MinIO when enabled)
make docker-dev && make migrate
make backend-test-integration

# Coverage / vulnerability scan
make backend-cover
make backend-vulncheck

# Client
make app-lint                      # tsc --noEmit
```

About 200 integration tests across 29 files. They cover auth, messaging, threads,
pagination, unread counts, RBAC and the invite flow, real WebSocket delivery and
revocation, Drive sharing and revocation, collaborative editing round-trips,
workflows, mail ingest — and, as explicit attack scenarios, cross-tenant search,
cross-tenant channel and issue access, cross-tenant collaboration-document
claiming, admin-endpoint scoping, cross-channel IDOR, and deactivation/eviction
actually revoking access.

It **skips** locally when infrastructure is unreachable but **fails** under CI
(`CI=true`, or `SUPEROPS_REQUIRE_INFRA=1` anywhere). Skipping everywhere is how
the suite silently went green for months on a Redis password mismatch.

CI (`.github/workflows/ci.yml`) runs lint/vet/unit tests; the integration suite
with `-race` against real Postgres, Redis, NATS, Meilisearch and MinIO (Postgres
and Redis as service containers, the rest as ordinary steps — NATS so JetStream
can be enabled, MinIO because it needs a command, Meilisearch because its image
carries no health-check binary); a second infra-backed unit run with `SUPEROPS_REQUIRE_INFRA=1`; `tsc`
and the app's test suite; govulncheck; both Docker image builds; and `helm lint`
plus `helm template`.

### Seed demo data

With the stack up and migrated:

```bash
make seed                          # demo users log in with password: demo_password_123
```

## Operations

```bash
./scripts/setup.sh              # first-time setup; generates real secrets
./scripts/backup.sh [dir]       # backs up Postgres AND MinIO objects into a
                                #   timestamped directory with a SHA256SUMS manifest
./scripts/restore.sh <dir>      # stops backend/worker, restores, restarts
cd deploy/docker && docker compose --profile tools run --rm reindex
                                # rebuild the Meilisearch index from Postgres.
                                #   `cd backend && make reindex` runs the same
                                #   command but loads no environment — you would
                                #   have to export the full server config first
```

`backup.sh` produces a directory per run
(`database.dump` — or `database.dump.gpg` with `BACKUP_GPG_RECIPIENT` set —
`database.toc`, `globals.sql`, `objects/`, `manifest.txt`, `SHA256SUMS`) — file
bytes live in MinIO, so a SQL-only dump would restore rows pointing at objects
that no longer exist.

That reindex is also the only repair for the search index: nothing reconciles
it automatically, and it is written solely by the live event stream.

## Limitations

Things this does **not** do, so you can decide before deploying.
[`docs/KNOWN-GAPS.md`](docs/KNOWN-GAPS.md) carries the full list with the
reasoning; these are the ones that change a deployment decision.

**Built but not finished**

- **Drive share links grant nothing.** Creating one mints a token and a real ACL
  grant; resolving one checks password, expiry, use count and revocation and
  returns an access key — and *nothing consumes that key*. There is no middleware
  that turns a resolved token into a subject, so every subsequent call is made as
  whoever the caller already was. Finishing it means minting a credential for an
  unauthenticated holder on the most security-sensitive path in the product, so
  it is left rather than half-built.
- **The shipped client can take access away but not give it.** `DriveShareScreen`
  lists grants, removes a user's grant, and mints and revokes share links. What no
  screen calls is `driveApi.share` (`PUT /drive/{object_type}/{object_id}/shares`)
  — the endpoint exists and is tested, so granting somebody access is an API
  operation.
- **The unified inbox has no UI beyond the compatibility shim.** Done/undone, the
  per-item event trail and per-kind delivery preferences are all mounted, enforced
  and unreachable — the client calls `/notifications` instead.
- **Labels, cycles and issue-to-issue links are schema only** (migration 031). No
  writer and no route, and nothing reads those tables — the one exception is
  `issues.cycle_id`, which is selected and shipped to the client permanently
  null.
- **Comments have no resolution.** No column, no method, no route.

**Deliberately absent**

- **No outgoing webhooks.** Incoming only; the `outgoing` type is rejected.
- **No SCIM and no directory sync.** OIDC SSO is implemented (ten routes) and
  gated on `SSO_SECRET_KEY`; membership and role are re-asserted only at login,
  so with JIT on, a locally-removed user returns on their next SSO sign-in.
  **There is no client UI for SSO** — it is API-only.
- **No scheduled/cron workflow trigger and no `http_request` step.** The latter is
  omitted deliberately as an SSRF primitive. There is also no manual run, no
  test-run and no cancel.
- **No reconnect backfill.** `seq` makes a gap *detectable*; recovery is a REST
  refetch, not a server-side replay. The client has no offline write queue —
  a collaborative edit made while the socket is down is lost.
- **No camera in huddles** (microphone and screen share only), **no mobile join**
  (start and end only; joining is web), a hardcoded cap of 10 participants, and
  no media server in the box — you run LiveKit yourself.
- **Search has no pagination** — `limit` is 1..100, so the 101st match is
  unreachable. Issues are declared as a searchable type but nothing indexes one;
  comments and mail are not indexed at all.

**Sharp edges**

- **Mail replies send no attachments** — the MIME builder has no `multipart/mixed`
  path. Inbound attachments are stored but **not charged against the storage
  quota**, and are **discarded silently** when object storage is unconfigured.
- **`POST /mail/inbound` is SuperOps' own JSON shape, not a provider webhook.** A
  SendGrid/Mailgun/Postmark/SES payload posted directly is a 400; you need an
  adapter. The whole body is capped at 2 MiB including base64 attachments.
- **Inbound HTML is stored exactly as delivered.** Migration 055 describes it as
  sanitized on the way in; nothing sanitizes it. Treat rendering as untrusted.
- **Reading a workflow requires workspace admin**, not membership, so ordinary
  members cannot see automation at all. `PATCH`/`DELETE` answer 403 for a workflow
  that exists in another tenant and 404 for one that does not, which is an
  existence oracle across tenants.
- **Collaborative editing trusts its writers.** Any client that may write can
  corrupt a document with a malformed update, and a snapshot does it in one shot —
  inherent to putting the CRDT in the client. The three retained snapshots are
  bytes on disk, not a rollback feature: only the newest is ever read.
- **Revocation of a live editing session is pushed only when a user's share is
  deleted.** A role downgrade or a group-subject change falls back to a periodic
  recheck up to five minutes wide.
- **`GET /notifications/unread-count` changed meaning** when the inbox landed: it
  counts unread *items* rather than *events*, so a burst that used to report 40
  now reports 1. Deliberate, and not versioned.
- **Naming a new document or issue does not work on Android or the web** — the
  client uses `Alert.prompt`, which is iOS-only, and falls through to the
  suggested name elsewhere. There is also no workspace switcher, and native deep
  links do not resolve as shipped (`app.json` has no `scheme`).
- **The onboarding and admin screens say SuperOps does not send email, and that
  is no longer true.** `POST /admin/invitations` renders an invitation and queues it
  durably; the worker's mail consumer delivers it, and the response carries
  `email_queued`. Under the default `MAIL_TRANSPORT=log` the message goes to the
  worker's log rather than an inbox — which is why the screen also shows the
  invite URL — but configured with `smtp`, `smtp-direct` or `resend`, invitations
  really are emailed. The client string is what is stale.

**Operational**

- **Single-region.** One Postgres pool and one Redis address; no Sentinel
  failover, no read-replica routing. Use `DATABASE_URL` with a pooler if you need
  more.
- **Search is eventually consistent** and repaired only by the reindex tool —
  see [Operations](#operations).
- **Trashing or restoring a large folder blocks the request** — one synchronous
  NATS publish per file, uncapped, roughly 330 µs each against a local broker.
- **NATS subjects are unauthenticated inside the cluster.** Anything on the bus
  can forge a room frame. The honest fixes are envelope signing or per-tenant
  accounts, both deployment-shaped decisions.
- **Message content is not encrypted at rest** beyond whatever your database and
  object storage provide, and `users.totp_secret` is necessarily stored
  recoverable. Use database-level encryption for sensitive deployments.

## License

[AGPL-3.0](LICENSE)
