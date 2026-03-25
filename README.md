# SuperOps

Free, self-hosted team messenger. Deploy with Docker Compose or Kubernetes.

## Features

- **Real-time messaging** - WebSocket-based instant delivery
- **Channels** - Public/private channels with topic, description
- **Threads** - Reply to messages in threads
- **Reactions** - Emoji reactions on messages
- **Presence** - Online/away/DND status tracking
- **Typing indicators** - See who's typing in real-time
- **File sharing** - Upload/download via S3-compatible storage (MinIO)
- **Full-text search** - Powered by Meilisearch
- **Notifications** - In-app notification system with unread counts
- **Workspaces** - Multi-workspace support with membership
- **RBAC** - Owner/admin/member/guest roles
- **Admin panel** - User management, stats, audit logs
- **Rate limiting** - Redis-backed sliding window

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Backend | Go 1.23 (net/http, no framework) |
| Frontend | React 19 + TypeScript + TailwindCSS v4 |
| Database | PostgreSQL 16 |
| Cache | Redis 7 |
| Search | Meilisearch |
| Storage | MinIO (S3-compatible) |
| Message Broker | NATS + JetStream |
| WebSocket | coder/websocket |

## Quick Start (Docker Compose)

```bash
# Clone
git clone https://github.com/Wick-Lim/SuperOps.git
cd SuperOps

# Configure
cp deploy/docker/.env.example deploy/docker/.env
# Edit deploy/docker/.env - set JWT_SECRET and passwords

# Start everything
cd deploy/docker
docker compose up -d

# Access
# http://localhost:80
```

## Development Setup

```bash
# Prerequisites: Go 1.23+, Node.js 22+, Docker

# 1. Start infrastructure only
cd deploy/docker
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d

# 2. Run migrations
cd backend
JWT_SECRET=dev_secret_change_me_32chars_long \
DB_HOST=localhost DB_PASSWORD=changeme_db_password \
go run ./cmd/migrate -direction up

# 3. Start backend
JWT_SECRET=dev_secret_change_me_32chars_long \
DB_HOST=localhost DB_PASSWORD=changeme_db_password \
REDIS_PASSWORD=changeme_redis_password \
go run ./cmd/superops

# 4. Start frontend (new terminal)
cd frontend
npm ci
npm run dev
# Open http://localhost:3000
```

Or use the setup script:

```bash
./scripts/setup.sh
```

## Kubernetes (Helm)

```bash
cd deploy/k8s/helm

# Install with custom values
helm install superops ./superops \
  --set jwt.secret="your-secret-here" \
  --set postgresql.auth.password="pg-password" \
  --set redis.auth.password="redis-password" \
  --set minio.auth.rootPassword="minio-password" \
  --set meilisearch.masterKey="meili-key" \
  --set global.domain="chat.example.com"
```

The chart includes:
- Backend Deployment with HPA (2-10 replicas)
- Frontend Deployment
- Worker Deployment
- Pre-install migration Job
- Ingress with WebSocket support
- ConfigMap + Secret
- Subchart dependencies (PostgreSQL, Redis, NATS, MinIO)

## Project Structure

```
SuperOps/
├── backend/
│   ├── cmd/                    # Entry points (superops, worker, migrate)
│   ├── internal/
│   │   ├── app/                # Bootstrap, config
│   │   ├── auth/               # JWT, login, register, middleware
│   │   ├── user/               # User CRUD
│   │   ├── workspace/          # Workspace + membership
│   │   ├── channel/            # Channel + membership
│   │   ├── message/            # Messages, threads, reactions
│   │   ├── ws/                 # WebSocket hub
│   │   ├── presence/           # Online status (Redis)
│   │   ├── file/               # File upload (MinIO)
│   │   ├── search/             # Full-text search (Meilisearch)
│   │   ├── notification/       # Notifications
│   │   ├── admin/              # Admin endpoints
│   │   └── ratelimit/          # Rate limiting
│   ├── pkg/                    # Shared packages
│   └── migrations/             # SQL migrations
├── frontend/
│   └── src/
│       ├── api/                # REST client
│       ├── stores/             # Zustand stores
│       ├── components/         # React components
│       ├── pages/              # Route pages
│       └── lib/                # WebSocket, types
├── deploy/
│   ├── docker/                 # Docker Compose + Dockerfiles
│   ├── k8s/helm/               # Kubernetes Helm chart
│   └── nginx/                  # Reverse proxy config
├── scripts/                    # Setup, backup, restore
└── .github/workflows/          # CI/CD
```

## API

All endpoints under `/api/v1`. Response format:

```json
{"data": {}, "meta": {"cursor": "", "has_more": false}, "error": null}
```

| Group | Endpoints |
|-------|-----------|
| Auth | `POST /auth/register`, `/login`, `/refresh`, `/logout` |
| Users | `GET /users/me`, `PATCH /users/me`, `GET /users/search` |
| Workspaces | CRUD `/workspaces`, members management |
| Channels | CRUD `/workspaces/{id}/channels`, join/leave |
| Messages | CRUD `/channels/{id}/messages`, reactions, threads |
| Files | `POST /files/upload`, `GET /files/{id}` |
| Search | `GET /workspaces/{id}/search?q=` |
| Notifications | `GET /notifications`, mark read |
| Admin | `GET /admin/users`, `/admin/stats`, `/admin/audit-logs` |
| WebSocket | `GET /ws?token={jwt}` |
| Health | `GET /health`, `GET /ready` |

## WebSocket Protocol

Connect: `ws://host/api/v1/ws?token={jwt}`

```json
{"type": "event_type", "seq": 1, "data": {}}
```

| Direction | Events |
|-----------|--------|
| Client -> Server | `ping`, `subscribe`, `unsubscribe`, `typing.start`, `presence.update` |
| Server -> Client | `pong`, `hello`, `message.new`, `message.updated`, `typing.indicator`, `presence.changed` |

## Scripts

```bash
./scripts/setup.sh              # First-time dev setup
./scripts/backup.sh [dir]       # Backup PostgreSQL
./scripts/restore.sh <file.gz>  # Restore from backup
```

## License

[AGPL-3.0](LICENSE)
