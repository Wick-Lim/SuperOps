# SuperOps

Free, self-hosted team messenger for organizations of any size. Slack/Mattermost alternative you own completely.

Deploy with a single `docker compose up` or scale to thousands of users with Kubernetes.

## Features

**Messaging**
- Real-time channels (public/private) with WebSocket
- Threaded replies with slide-out panel
- Direct messages (1:1)
- Emoji reactions
- Message editing and deletion (soft delete)
- Cursor-based pagination for message history
- File sharing with inline preview (images, documents)

**Collaboration**
- Multi-workspace support
- Full-text search powered by Meilisearch (Cmd+K)
- User presence (online/away/DND/offline)
- Typing indicators
- Unread counts and read tracking

**Administration**
- Admin dashboard (stats, user management, audit logs)
- Role-based access control (owner/admin/member/guest)
- Rate limiting (Redis sliding window)
- Audit logging for compliance

**Infrastructure**
- Horizontal scaling with NATS-bridged WebSocket hub
- Auto-scaling via Kubernetes HPA (2-10 replicas)
- PostgreSQL HA with read replicas
- Redis Sentinel for automatic failover
- JetStream durable streams for reliable async processing
- PodDisruptionBudget for zero-downtime maintenance

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Backend | Go 1.25 (net/http, no framework) |
| Frontend | React 19 + TypeScript + TailwindCSS v4 + Zustand |
| Database | PostgreSQL 16 (HA replication) |
| Cache | Redis 7 (Sentinel) |
| Search | Meilisearch |
| Storage | MinIO (S3-compatible) |
| Messaging | NATS + JetStream (clustered) |
| WebSocket | coder/websocket + NATS bridge |

## Quick Start

### Docker Compose

```bash
git clone https://github.com/Wick-Lim/SuperOps.git
cd SuperOps

cp deploy/docker/.env.example deploy/docker/.env
# Edit .env — set JWT_SECRET and passwords

cd deploy/docker
docker compose up -d

# Open http://localhost
```

### Development

```bash
# Prerequisites: Go 1.25+, Node.js 22+, Docker

# Start infrastructure
cd deploy/docker
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d

# Run migrations
cd ../../backend
JWT_SECRET=dev_secret_change_me_32chars_long \
DB_HOST=localhost DB_PASSWORD=changeme_db_password \
go run ./cmd/migrate -direction up

# Start backend
JWT_SECRET=dev_secret_change_me_32chars_long \
DB_HOST=localhost DB_PASSWORD=changeme_db_password \
REDIS_PASSWORD=changeme_redis_password \
go run ./cmd/superops

# Start frontend (new terminal)
cd frontend && npm ci && npm run dev
# Open http://localhost:3000
```

Or simply: `./scripts/setup.sh`

### Kubernetes

```bash
helm install superops deploy/k8s/helm/superops \
  --set jwt.secret="your-secret" \
  --set postgresql.auth.password="pg-pass" \
  --set redis.auth.password="redis-pass" \
  --set minio.auth.rootPassword="minio-pass" \
  --set meilisearch.masterKey="meili-key" \
  --set global.domain="chat.example.com"
```

Includes: backend HPA (2-10 replicas), frontend, worker, pre-install migration job, ingress with WebSocket, PodDisruptionBudget, PostgreSQL HA, Redis Sentinel, NATS cluster.

## Architecture

```
                        ┌─────────────────────────────────────────────┐
                        │              Kubernetes / Docker             │
  Browser ──────────►   │  ┌─────────┐                                │
  (React SPA)    WS/    │  │  Nginx  │──► Frontend (static)           │
                 REST   │  │ Ingress │                                │
                        │  └────┬────┘                                │
                        │       │                                     │
                        │  ┌────▼────┐   ┌──────────┐   ┌─────────┐  │
                        │  │Backend-1│◄─►│          │◄─►│         │  │
                        │  │  (Hub)  │   │   NATS   │   │  Redis  │  │
                        │  ├─────────┤   │ JetStream│   │Sentinel │  │
                        │  │Backend-N│◄─►│ (cluster)│   │  (HA)   │  │
                        │  │  (Hub)  │   │          │   │         │  │
                        │  └────┬────┘   └────┬─────┘   └─────────┘  │
                        │       │             │                       │
                        │  ┌────▼────┐   ┌────▼─────┐   ┌─────────┐  │
                        │  │PostgreSQL│   │  Worker  │   │  MinIO  │  │
                        │  │  (HA)   │   │(indexer, │   │  (S3)   │  │
                        │  │ primary │   │ notifier)│   │         │  │
                        │  │+replica │   └──────────┘   └─────────┘  │
                        │  └─────────┘                                │
                        │                    ┌──────────┐             │
                        │                    │Meilisearch│            │
                        │                    └──────────┘             │
                        └─────────────────────────────────────────────┘
```

**Multi-replica WebSocket delivery:**
Each backend instance runs a local WebSocket Hub. When a message arrives, it's delivered to local clients AND published to NATS `ws.broadcast.{channelId}`. All other instances receive via NATS subscription and forward to their local clients. No sticky sessions required.

## Project Structure

```
SuperOps/
├── backend/
│   ├── cmd/
│   │   ├── superops/           # API server
│   │   ├── worker/             # Async worker (search index, notifications)
│   │   └── migrate/            # Database migrations
│   ├── internal/
│   │   ├── app/                # Bootstrap, config, wiring
│   │   ├── auth/               # JWT, login/register, middleware
│   │   ├── user/               # User CRUD, search
│   │   ├── workspace/          # Workspaces + membership
│   │   ├── channel/            # Channels + join/leave
│   │   ├── message/            # Messages, threads, reactions
│   │   ├── ws/                 # WebSocket hub + NATS bridge
│   │   ├── presence/           # Online status (Redis-backed)
│   │   ├── file/               # File upload/download (MinIO)
│   │   ├── search/             # Meilisearch indexing + query
│   │   ├── notification/       # In-app notifications
│   │   ├── admin/              # Admin endpoints
│   │   ├── rbac/               # Role-based access middleware
│   │   ├── audit/              # Audit logging
│   │   └── ratelimit/          # Redis rate limiting
│   ├── pkg/                    # Shared: database, redis, nats, httputil, crypto
│   └── migrations/             # 6 SQL migration files (12+ tables)
├── frontend/
│   └── src/
│       ├── api/                # REST client (auth, channels, messages, search, admin, files)
│       ├── stores/             # Zustand (auth, channel, message, presence, thread)
│       ├── components/
│       │   ├── layout/         # Sidebar (responsive, DM section)
│       │   ├── channel/        # ChannelView
│       │   ├── message/        # MessageList (virtualized), MessageItem, MessageInput
│       │   ├── thread/         # ThreadPanel (slide-out)
│       │   ├── search/         # SearchModal (Cmd+K)
│       │   ├── dm/             # DMCreate
│       │   ├── file/           # FileUpload
│       │   ├── presence/       # PresenceIndicator, TypingIndicator
│       │   └── shared/         # Modal, Button, Avatar, Spinner, Badge, Toast, EmojiPicker, NotificationBell, ErrorBoundary
│       ├── pages/              # Login, Register, Setup, Workspace, Admin, 404
│       └── lib/                # WebSocket manager, types
├── deploy/
│   ├── docker/                 # Compose (9 services), Dockerfiles, nginx
│   ├── k8s/helm/superops/      # Helm chart (12 templates, HA values)
│   └── nginx/                  # Reverse proxy (WS upgrade, API routing)
├── scripts/                    # setup.sh, backup.sh, restore.sh
└── .github/workflows/          # CI (lint/test/build), Release (Docker images)
```

## API Reference

All endpoints under `/api/v1`. Consistent response envelope:

```json
{"data": {...}, "meta": {"cursor": "...", "has_more": true}, "error": null}
```

| Group | Key Endpoints |
|-------|--------------|
| **Auth** | `POST /auth/register`, `/login`, `/refresh`, `/logout` |
| **Users** | `GET /users/me`, `PATCH /users/me`, `GET /users/search?q=` |
| **Workspaces** | `POST /workspaces`, `GET /workspaces/{id}/members` |
| **Channels** | `POST /workspaces/{id}/channels`, `POST .../join`, `POST .../leave` |
| **Messages** | `POST /channels/{id}/messages`, `GET ...?cursor=&limit=` |
| **Threads** | `GET /messages/{id}/thread`, `POST /messages/{id}/thread` |
| **Reactions** | `POST /channels/{id}/messages/{id}/reactions` |
| **DM** | `POST /workspaces/{id}/dm` |
| **Files** | `POST /files/upload` (multipart), `GET /files/{id}` |
| **Search** | `GET /workspaces/{id}/search?q=&channel=&from=` |
| **Notifications** | `GET /notifications`, `PUT .../read-all`, `GET .../unread-count` |
| **Admin** | `GET /admin/stats`, `/admin/users`, `/admin/audit-logs` |
| **WebSocket** | `GET /ws?token={jwt}` |
| **Health** | `GET /health`, `GET /ready` |

## WebSocket Protocol

```
ws://host/api/v1/ws?token={jwt}
```

Frame: `{"type": "event_type", "seq": 1, "data": {...}}`

| Direction | Events |
|-----------|--------|
| Client | `ping`, `subscribe`, `unsubscribe`, `typing.start`, `typing.stop`, `presence.update` |
| Server | `pong`, `hello`, `message.new`, `message.updated`, `message.deleted`, `reaction.added`, `typing.indicator`, `presence.changed`, `notification.new`, `unread.update` |

## Testing

```bash
# Backend (9 tests)
cd backend && go test ./... -v

# Frontend (7 tests)
cd frontend && npm test
```

## Operations

```bash
./scripts/setup.sh              # First-time dev environment setup
./scripts/backup.sh [dir]       # Backup PostgreSQL (gzip)
./scripts/restore.sh <file.gz>  # Restore from backup
```

## Configuration

All configuration via environment variables. See [`deploy/docker/.env.example`](deploy/docker/.env.example) for the full list.

Key variables: `JWT_SECRET`, `DB_HOST`, `DB_PASSWORD`, `REDIS_PASSWORD`, `NATS_URL`, `MINIO_ENDPOINT`, `MEILI_HOST`.

## License

[AGPL-3.0](LICENSE)
