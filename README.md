# SuperOps

Free, self-hosted team messenger for organizations of any size. Slack/Mattermost alternative you own completely.

Native apps for **iOS, Android, macOS, and Windows** from a single React Native codebase. Backend deploys with Docker Compose or scales to thousands with Kubernetes.

## Features

**Messaging**
- Real-time channels (public/private) with WebSocket
- Threaded replies
- Direct messages (1:1)
- Emoji reactions
- Message editing and deletion (soft delete)
- Cursor-based pagination for message history
- File sharing with inline preview

**Collaboration**
- Multi-workspace support
- Full-text search powered by Meilisearch
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
| Mobile/Desktop App | React Native (Expo) + TypeScript |
| Backend | Go 1.25 (net/http, no framework) |
| Database | PostgreSQL 16 (HA replication) |
| Cache | Redis 7 (Sentinel) |
| Search | Meilisearch |
| Storage | MinIO (S3-compatible) |
| Messaging | NATS + JetStream (clustered) |
| WebSocket | coder/websocket + NATS bridge |

## Supported Platforms

| Platform | Method |
|----------|--------|
| iOS | Expo / `npx expo run:ios` |
| Android | Expo / `npx expo run:android` |
| macOS | `react-native-macos` |
| Windows | `react-native-windows` |

## Quick Start

### 1. Start the backend

```bash
git clone https://github.com/Wick-Lim/SuperOps.git
cd SuperOps

cp deploy/docker/.env.example deploy/docker/.env
# Edit .env — set JWT_SECRET and passwords

cd deploy/docker
docker compose up -d
# Backend API available at http://localhost:8081
```

### 2. Run the app

```bash
cd app
npm install
npx expo start
# Press 'i' for iOS simulator, 'a' for Android emulator
```

### Development (local backend)

```bash
# Start infrastructure only
cd deploy/docker
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d

# Run migrations + start backend
cd ../../backend
JWT_SECRET=dev_secret_change_me_32chars_long \
DB_HOST=localhost DB_PASSWORD=changeme_db_password \
REDIS_PASSWORD=changeme_redis_password \
go run ./cmd/migrate -direction up && go run ./cmd/superops

# Run app (new terminal)
cd app && npx expo start
```

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

Includes: backend HPA (2-10 replicas), worker, pre-install migration job, ingress with WebSocket, PodDisruptionBudget, PostgreSQL HA, Redis Sentinel, NATS cluster.

## Architecture

```
  ┌──────────────┐
  │  Native App  │  React Native (iOS / Android / macOS / Windows)
  │  (Expo)      │
  └──────┬───────┘
         │ REST + WebSocket
         ▼
┌─────────────────────────────────────────────────┐
│                Kubernetes / Docker                │
│                                                   │
│  ┌──────────┐    ┌──────────┐    ┌─────────┐    │
│  │Backend-1 │◄──►│   NATS   │◄──►│  Redis  │    │
│  │  (Hub)   │    │ JetStream│    │Sentinel │    │
│  ├──────────┤    │ (cluster)│    │  (HA)   │    │
│  │Backend-N │◄──►│          │    │         │    │
│  │  (Hub)   │    └────┬─────┘    └─────────┘    │
│  └──────┬───┘         │                          │
│         │        ┌────▼─────┐    ┌─────────┐    │
│  ┌──────▼───┐    │  Worker  │    │  MinIO  │    │
│  │PostgreSQL│    │(indexer, │    │  (S3)   │    │
│  │  (HA)    │    │ notifier)│    │         │    │
│  │ primary  │    └──────────┘    └─────────┘    │
│  │+replica  │                                    │
│  └──────────┘    ┌──────────┐                    │
│                  │Meilisearch│                    │
│                  └──────────┘                    │
└─────────────────────────────────────────────────┘
```

**Multi-replica WebSocket delivery:**
Each backend instance runs a local WebSocket Hub. Messages are delivered to local clients AND published to NATS `ws.broadcast.{channelId}`. Other instances receive via NATS and forward to their local clients. No sticky sessions required.

## Project Structure

```
SuperOps/
├── app/                        # React Native (Expo)
│   ├── src/
│   │   ├── api/                # REST client (auth, channels, messages, workspaces)
│   │   ├── stores/             # Zustand + AsyncStorage (auth, channel, message, workspace)
│   │   ├── lib/                # Types, WebSocket manager
│   │   ├── screens/            # Login, Register, Setup, Workspace, Admin
│   │   ├── components/
│   │   │   ├── channel/        # ChannelView
│   │   │   └── message/        # MessageList (FlatList), MessageItem, MessageInput
│   │   ├── navigation/         # React Navigation (AuthStack / MainStack)
│   │   └── config.ts           # API_BASE_URL, WS_BASE_URL
│   └── App.tsx
├── backend/                    # Go API server
│   ├── cmd/
│   │   ├── superops/           # API server
│   │   ├── worker/             # Async worker (search index, notifications, cleanup)
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
│   └── migrations/             # SQL migrations (12+ tables)
├── deploy/
│   ├── docker/                 # Compose (backend, worker, migrate, PG, Redis, NATS, MinIO, Meilisearch)
│   ├── k8s/helm/superops/      # Helm chart (HA values, HPA, PDB, Sentinel)
│   └── nginx/                  # Reverse proxy config
├── scripts/                    # setup.sh, backup.sh, restore.sh
└── .github/workflows/          # CI (lint/test/build), Release (Docker images)
```

## API Reference

All endpoints under `/api/v1`. Response envelope:

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
# Backend (Go)
cd backend && go test ./... -v

# App (TypeScript)
cd app && npx tsc --noEmit
```

## Operations

```bash
./scripts/setup.sh              # First-time dev environment setup
./scripts/backup.sh [dir]       # Backup PostgreSQL (gzip)
./scripts/restore.sh <file.gz>  # Restore from backup
```

## Configuration

All backend configuration via environment variables. See [`deploy/docker/.env.example`](deploy/docker/.env.example) for the full list.

App server URL is configured in [`app/src/config.ts`](app/src/config.ts).

## License

[AGPL-3.0](LICENSE)
