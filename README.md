# SuperOps

Free, self-hosted team messenger for organizations of any size. Slack/Mattermost alternative you own completely.

A React Native (Expo) client for **iOS, Android and the web**, and a Go backend that deploys with Docker Compose or Kubernetes.

## Features

**Messaging**
- Real-time channels (public/private) over WebSocket
- Threaded replies
- Direct messages (1:1 and group)
- Emoji reactions
- Message editing and deletion (soft delete)
- Pinned messages, bookmarks, scheduled messages, forwarding
- Keyset-paginated history
- File sharing with inline preview

**Collaboration**
- Multi-workspace support
- Full-text search (Meilisearch), scoped to the channels you can actually read
- User presence (online/away/DND/offline) and typing indicators
- Per-channel unread counts and read tracking
- Mobile push notifications via Expo (opt-in; needs your own APNs/FCM credentials — see [Push notifications](#push-notifications))
- User blocking, custom emoji, incoming webhooks

**Administration**
- Admin dashboard (stats, user management, audit logs, invitations) — every endpoint scoped to the workspaces you administer
- Role-based access control (owner/admin/member/guest)
- TOTP two-factor authentication with single-use backup codes
- Redis sliding-window rate limiting
- Audit logging

**Infrastructure**
- Horizontal scaling with a NATS-bridged WebSocket hub — no sticky sessions
- JetStream durable consumers for search indexing and notifications (at-least-once, with a poison-message path)
- Auto-scaling via Kubernetes HPA, PodDisruptionBudget, NetworkPolicy
- Prometheus metrics including a request-latency histogram and pgxpool saturation

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Client | React Native (Expo 55) + TypeScript |
| Backend | Go 1.25 (net/http, no framework) |
| Database | PostgreSQL 16+ |
| Cache | Redis 7+ |
| Search | Meilisearch |
| Storage | MinIO (S3-compatible) |
| Messaging | NATS + JetStream |
| WebSocket | coder/websocket + NATS bridge |

## Supported Platforms

| Platform | Method |
|----------|--------|
| iOS | `npx expo run:ios` |
| Android | `npx expo run:android` |
| Web | `npx expo start --web` (dev) · static export via `deploy/docker/Dockerfile.frontend` (prod) |

## Quick Start

### 1. Start the backend

```bash
git clone https://github.com/Wick-Lim/SuperOps.git
cd SuperOps

./scripts/setup.sh          # generates deploy/docker/.env with real random secrets
make docker-up              # API on http://localhost:8081, web client on http://localhost:8080
```

`setup.sh` generates every secret; do not ship `.env.example` values. `JWT_SECRET`
must be at least 32 bytes, and malformed configuration values now fail startup
rather than silently falling back to a default.

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
there is no shell-variable prefix to get wrong.

### Kubernetes

```bash
helm install superops deploy/k8s/helm/superops \
  --set admin.email="you@example.com" \
  --set admin.password="..." \
  --set jwt.secret="at-least-32-bytes-of-random" \
  --set postgresql.auth.password="..." \
  --set redis.auth.password="..." \
  --set minio.auth.rootPassword="..." \
  --set meilisearch.masterKey="..." \
  --set global.domain="chat.example.com"
```

Those values are required — the chart fails at render time rather than
CrashLooping. Includes: backend HPA, worker and frontend Deployments, a
pre-upgrade migration Job, ingress with WebSocket support, PDB, NetworkPolicy
and a ServiceAccount.

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
│  │          │    │ notifier)│    │         │    │
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
replica does not double-deliver.

**Durability.** The worker binds JetStream durable pull consumers for search
indexing and notifications, with explicit ack, bounded redelivery and a terminal
path for poison messages. The WebSocket relay stays on core NATS — it is
deliberately fire-and-forget fan-out.

## Project Structure

```
SuperOps/
├── app/                        # React Native (Expo)
│   ├── src/
│   │   ├── api/                # REST modules (auth, channels, messages, admin, webhooks, …)
│   │   ├── stores/             # Zustand (auth, channel, message, workspace, ui, user)
│   │   ├── lib/                # types, websocket manager, errors, secure storage, theme
│   │   ├── screens/            # Login, Invite, Onboarding, Workspace, Search, Members,
│   │   │                       #   Notifications, Settings, Admin, NewChannel, NewDM,
│   │   │                       #   Pinned, Bookmarks, Scheduled, ChannelDetail
│   │   ├── components/
│   │   │   ├── channel/        # ChannelView
│   │   │   └── message/        # MessageList, MessageItem, MessageInput, ThreadView,
│   │   │                       #   ReactionBar, EmojiPicker, RichText, AttachmentViewer
│   │   ├── navigation/         # AppNavigator (single stack, auth-gated groups)
│   │   └── config.ts
│   └── App.tsx
├── backend/                    # Go API server
│   ├── cmd/
│   │   ├── superops/           # API server
│   │   ├── worker/             # JetStream consumers + scheduled/retention/GC jobs
│   │   ├── migrate/            # Database migrations
│   │   ├── seed/               # Demo data
│   │   └── reindex/            # Rebuild the Meilisearch index from Postgres
│   ├── internal/
│   │   ├── app/                # Composition root, config, metrics
│   │   ├── authz/              # Canonical membership/role checks
│   │   ├── auth/               # JWT, sessions, TOTP, invites
│   │   ├── rbac/               # Route-level role middleware
│   │   ├── user/ workspace/ channel/ message/
│   │   ├── ws/ presence/       # WebSocket hub, NATS bridge/relay, presence
│   │   ├── file/ search/ notification/ push/ webhook/ emoji/ block/
│   │   ├── admin/ audit/ ratelimit/
│   ├── pkg/                    # authctx, crypto, database, httputil, logger, nats, redis
│   ├── migrations/             # SQL migrations (000–012)
│   └── test/integration/       # Build-tagged suite against real infrastructure
├── deploy/
│   ├── docker/                 # Compose, Dockerfiles, nginx, TLS overlay, Prometheus
│   └── k8s/helm/superops/      # Helm chart
├── scripts/                    # setup.sh, backup.sh, restore.sh
└── .github/workflows/          # CI (lint/test/build), Release (Docker images)
```

## API Reference

94 routes, two of which (device registration) exist only when `PUSH_ENABLED=true`.
All application endpoints are under `/api/v1`; `/health`, `/ready` and
`/metrics` sit at the root. Response envelope:

```json
{"data": {...}, "meta": {"cursor": "...", "has_more": true}}
```

`meta` and `error` are omitted when empty — a successful response has no `error`
key at all, and an error response is `{"data": null, "error": {"code","message"}}`.
Request bodies reject unknown fields.

| Group | Key Endpoints |
|-------|--------------|
| **Auth** | `POST /auth/login`, `/refresh`, `/logout`, `/accept-invite`, `/change-password`, `GET /auth/invite/{token}` |
| **2FA (TOTP)** | `GET /auth/totp/status`, `POST /auth/totp/setup`, `/verify`, `/disable` |
| **Users** | `GET /users/me`, `PATCH /users/me`, `PUT /users/me/status`, `GET /users/{id}`, `/users/search?q=` |
| **Devices (push)** | `POST /users/me/devices`, `DELETE /users/me/devices/{token}` — registered only when `PUSH_ENABLED=true` |
| **Workspaces** | `POST /workspaces`, `GET/PATCH/DELETE /workspaces/{id}`, `GET /workspaces/{id}/members`, `PATCH`/`DELETE .../members/{user_id}`, `POST /workspaces/{id}/transfer-ownership`, `GET /workspaces/{id}/presence` |
| **Channels** | `POST/GET /workspaces/{id}/channels`, `/browse`, `GET/PATCH .../channels/{cid}`, `/join`, `/leave`, `/archive`, `/unarchive`, `GET/POST .../members`, `DELETE .../members/{user_id}` |
| **Direct messages** | `POST /workspaces/{id}/channels/dm` (1:1 idempotent + group) |
| **Messages** | `POST/GET /channels/{id}/messages`, `GET/PATCH/DELETE .../{mid}`, `POST .../{mid}/forward`; `file_ids` and `scheduled_at` accepted on send |
| **Threads** | `GET/POST /messages/{id}/thread` (paginated) |
| **Reactions** | `POST /channels/{id}/messages/{mid}/reactions`, `DELETE .../reactions/{emoji}`, `GET /messages/{id}/reactions` |
| **Pins / Bookmarks** | `POST/DELETE /channels/{id}/messages/{mid}/pin`, `GET /channels/{id}/pins`, `POST/DELETE /messages/{id}/bookmark`, `GET /bookmarks` |
| **Scheduled** | `GET /channels/{id}/scheduled`, `DELETE .../scheduled/{mid}` |
| **Read state** | `PUT /channels/{id}/read`, `GET /channels/{id}/unread`, `PATCH /channels/{id}/prefs` |
| **Typing** | `GET /channels/{id}/typing` |
| **Files** | `POST /files/upload` (multipart), `GET/DELETE /files/{id}` |
| **Search** | `GET /workspaces/{id}/search?q=&channel=&from=` |
| **Notifications** | `GET /notifications`, `PUT .../{id}/read`, `/read-all`, `GET .../unread-count` |
| **Blocks / Emoji** | `GET/POST /blocks`, `DELETE /blocks/{blocked_id}`, `GET/POST /workspaces/{id}/emojis`, `DELETE .../emojis/{emoji_id}` |
| **Webhooks** | `POST/GET /webhooks`, `PATCH/DELETE /webhooks/{id}`, `PUT /webhooks/{id}/token` (rotate), `POST /webhooks/incoming` |
| **Admin** | `GET /admin/stats`, `/admin/users`, `PATCH /admin/users/{id}`, `GET /admin/audit-logs`, `GET/POST /admin/invitations` |
| **WebSocket** | `GET /api/v1/ws?token={jwt}` |
| **Ops** | `GET /health` (liveness, no dependencies), `GET /ready` (Postgres/Redis/NATS), `GET /metrics` |

Authentication is `Authorization: Bearer <jwt>`. The `?token=` query parameter is
accepted **only** on the WebSocket and file-download routes, which cannot set a
header.

## WebSocket Protocol

```
ws://host/api/v1/ws?token={jwt}
```

Frame: `{"type": "event_type", "seq": 1, "data": {...}}`. `seq` is per-connection
and strictly monotonic on every frame, so a gap means backpressure dropped
frames and the client should refetch — there is no server-side replay.

| Direction | Events |
|-----------|--------|
| Client | `ping`, `subscribe`, `unsubscribe`, `typing.start`, `typing.stop`, `presence.update` |
| Server | `hello`, `pong`, `subscribed`, `unsubscribed`, `error`, `message.new`, `message.updated`, `message.deleted`, `reaction.added`, `reaction.removed`, `channel.created`, `channel.updated`, `member.joined`, `member.left`, `typing.indicator`, `presence.changed`, `notification.new`, `unread.update` |

`subscribe` is acknowledged with `subscribed`, so a client need not race the
upgrade. `unsubscribed` carries a reason (`client` or `revoked`); on `revoked`
the server has removed the caller's access and the client should drop the
channel. `error` frames carry a `code`. The server sends WebSocket pings and
closes sockets that stop responding; clients do not need to send `ping`.

## Configuration

All backend configuration is environment variables — see
[`deploy/docker/.env.example`](deploy/docker/.env.example) for the full list with
defaults. Notes that bite:

- `JWT_SECRET` must be **≥ 32 bytes**; startup fails otherwise.
- Malformed values (`SERVER_PORT=eighty`, `RATE_LIMIT_ENABLED=yes`) now **fail
  startup** with every offending variable reported at once, instead of silently
  taking the default.
- `RATE_LIMIT_TRUST_PROXY` defaults to **false**. Behind a reverse proxy set it
  to `true` and `RATE_LIMIT_TRUSTED_PROXY_HOPS` to the number of proxies; the
  client IP is read that many entries from the **right** of `X-Forwarded-For`,
  which a client cannot forge past.
- `SEARCH_ENABLED` / `FILES_ENABLED` turn Meilisearch and MinIO off. When
  disabled (or unreachable) the corresponding routes are not registered.
- `DATABASE_URL` overrides the `DB_*` parts entirely — use it for a pooler,
  a multi-host DSN or `target_session_attrs`.
- `METRICS_TOKEN` guards `/metrics`; if you set it, add a matching
  `authorization` block to `deploy/docker/prometheus.yml` or scrapes will 401.

Client configuration: `EXPO_PUBLIC_API_URL` (and optionally
`EXPO_PUBLIC_WS_URL`, `EXPO_PUBLIC_EXPO_PROJECT_ID`). See
[`app/src/config.ts`](app/src/config.ts).

## Push notifications

Mobile push via [Expo's push service](https://docs.expo.dev/push-notifications/overview/),
which fronts APNs and FCM. **Off by default** — enabling it sends the first 140
characters of every notified message to Expo, and through Expo to Apple and
Google, which is a decision to make deliberately.

How it works: the client registers its Expo push token
(`POST /users/me/devices`), and the worker — which already creates notification
rows off the JetStream `message.created` / `reaction.added` /
`channel.member_added` events — enqueues a push to that user's devices on the
same path. Audience is identical to the in-app notification: per-channel `muted`
and `notification_pref`, blocks and self-exclusion all apply before push is
reached. A `DeviceNotRegistered` receipt deletes the token, so a reinstalled or
rotated device stops being retried.

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
   skipped.
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

Sending is best-effort by construction: the notification row is committed first
and the push is queued to an in-process dispatcher that drops rather than blocks
when full, because stalling on a third party inside a JetStream consumer
callback would stall event acking. Redelivery does not double-buzz — the push is
sent only when the notification row was genuinely new.

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

The integration suite covers auth, messaging, threads, pagination, unread
counts, RBAC and the invite flow, real WebSocket delivery and revocation, and —
as explicit attack scenarios — cross-tenant search, cross-tenant channel access,
admin-endpoint scoping, cross-channel IDOR, and deactivation/eviction actually
revoking access.

It **skips** locally when infrastructure is unreachable but **fails** under CI
(`CI=true`, or `SUPEROPS_REQUIRE_INFRA=1` anywhere). Skipping everywhere is how
the suite silently went green for months on a Redis password mismatch.

CI (`.github/workflows/ci.yml`) runs lint/vet/unit tests, the integration suite
with `-race` against service containers, `tsc`, govulncheck, both Docker image
builds, and `helm lint` + `helm template`.

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
cd backend && make reindex      # rebuild the Meilisearch index from Postgres
```

`backup.sh` produces a directory per run
(`database.dump`, `globals.sql`, `objects/`, `manifest.txt`, `SHA256SUMS`) — file
bytes live in MinIO, so a SQL-only dump would restore rows pointing at objects
that no longer exist.

## Limitations

Things this does **not** do, so you can decide before deploying:

- **Push notifications need an Expo project you own.** The pipeline is built and
  off by default (`PUSH_ENABLED=false`); see [Push notifications](#push-notifications)
  for the credentials it cannot supply for you. There is still no email delivery.
- **No outgoing webhooks.** Incoming only; the `outgoing` type is rejected.
- **No reconnect backfill.** `seq` makes a gap *detectable*; recovery is a REST
  refetch, not a server-side replay.
- **No SSO/OAuth/SCIM.**
- **Single-region.** The app uses one Postgres connection pool and one Redis
  address; it does not follow a Sentinel failover or route reads to a replica.
  Use `DATABASE_URL` with a pooler if you need more.
- **Search is eventually consistent** — indexing happens in the worker off the
  JetStream stream. If the index is lost, rebuild it with `cd backend && make reindex`.
- **Message content is not encrypted at rest** beyond whatever your database and
  object storage provide, and `users.totp_secret` is necessarily stored
  recoverable. Use database-level encryption for sensitive deployments.

## License

[AGPL-3.0](LICENSE)
