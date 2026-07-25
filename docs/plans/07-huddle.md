# Plan 07 — Huddle

**Order-independent.** Shares almost nothing with the other pillars, so it slots in
whenever a team is free (ROADMAP §6). It is the best value-per-effort of the nine
*because the decision has already been made*: use LiveKit, do not build an SFU.

Status: design. Not started.

---

## What it is

A huddle is a live audio/video/screen-share room attached to a channel. Someone
clicks "Huddle" in `#design`, everyone in the channel sees a bar at the top of
the channel with who is in it, and clicking joins. Audio by default, camera and
screen opt-in. It ends when the last person leaves, or when the starter or a
channel admin ends it. History is a row you can look at: who was in it, when, for
how long.

It is deliberately not a meetings product. **Not in scope for v1:**

- **Recording and transcription.** Explicit ROADMAP §7 cut. Storage, cost and
  compliance surface out of proportion to v1 value.
- **More than 10 participants.** Enforced in two places (schema CHECK and the
  LiveKit room's `max_participants`), not documented and hoped for.
- **Scheduled meetings, calendars, invites, dial-in/SIP, lobbies, breakout
  rooms, virtual backgrounds, in-call chat.** The channel is the chat.
- **Native mobile join.** See "The mobile story". Mobile sees the huddle, sees
  who is in it, gets the push, and opens on web.
- **Issue-scoped huddles.** The schema is polymorphic so this is a row value and
  not a migration, but work tracking (Phase 2) does not exist yet and nothing
  registers the `issue` scope in v1.
- **End-to-end encryption.** LiveKit supports it via insertable streams; it
  forecloses every server-side feature (including a future recording) and is a
  re-planning trigger, not a backlog item.
- **Search indexing of huddles.** Nothing to index without transcription.

---

## What already exists, and what it actually buys

| Asset | What huddle takes from it | Where |
|---|---|---|
| WS hub, multi-replica via NATS | The entire "who is in the huddle" fan-out. A `huddle.*` domain event carries `channel_id`, so it rides the existing channel-subscription path with **no new fan-out code**. | `internal/ws/hub.go:360` (`publishDomain`), `internal/ws/relay.go:45` (`dispatchEvent`) |
| `internal/authz` | Who may start and who may join. `IsChannelMember` / `CanReadChannel` answer it today. | `internal/authz/authz.go:197`, `:214` |
| Subscription revocation | The pattern for "a permission changed under a live session". Huddle needs the same hook plus a call into LiveKit. | `internal/ws/hub.go:255` (`RevokeChannelSubscription`), `internal/ws/client.go:423` (`recheckSubscriptions`) |
| `cmd/worker` periodic jobs | The reconciler and the webhook-event sweep are two more `start(name, delay, interval, fn)` calls. | `cmd/worker/main.go:261` |
| `internal/notification` + push | "Ana started a huddle in #design" is one new `Type`. | `internal/notification/model.go:8` |
| `internal/mail`'s shape | The template for a deployment-dependent capability: interface, transport chosen by config, validated at boot, safe default, admin test endpoint. Copy it exactly. | `internal/mail/sender.go:33`, `internal/app/config.go:510` |
| `golang-jwt/jwt/v5` | LiveKit join tokens *are* JWTs. Already a direct dependency. | `backend/go.mod:7` |

**Signaling is not the hard part and the hub is not the answer to it.** The
in-flight collaboration work is adding a `RoomHandler` seam to the WS client
(`internal/ws/handler.go:33`) for CRDT relay. Huddle must not use it. LiveKit
carries its own signaling over its own WebSocket; putting media negotiation
through our hub would mean reimplementing the part LiveKit exists to provide.
Our hub carries *notifications about* huddles, nothing else.

---

## Decision: LiveKit, and no Go SDK

LiveKit (Go, Apache-2.0, self-hostable) provides the SFU **and an embedded TURN
server**, which is why ROADMAP §3c's TURN question mostly collapses into this
one choice.

**No new Go dependency.** Every LiveKit surface we need is reachable with what
is already in `go.mod`:

| Surface | How | Stdlib/existing |
|---|---|---|
| Join token | HS256 JWT, `iss`=API key, `sub`=identity, `exp`, `jti`, plus a `video` grant claim | `golang-jwt/jwt/v5` |
| Room admin (create, remove participant, end room, list) | Twirp, which speaks JSON over `POST /twirp/livekit.RoomService/<Method>`, authenticated with the same JWT shape carrying `roomAdmin` | `net/http`, `encoding/json` |
| Webhook verification | JWT signed with the API secret whose `sha256` claim is the base64 SHA-256 of the raw body | `golang-jwt/jwt/v5`, `crypto/sha256` |
| TURN REST credentials (coturn path) | `username = <unix-expiry>:<user-id>`, `credential = base64(HMAC-SHA1(secret, username))` | `crypto/hmac`, `crypto/sha1` |

`github.com/livekit/server-sdk-go/v2` drags in pion/webrtc, protobuf and its own
logging for what is ~200 lines of HTTP. That is a large transitive surface for a
process that never touches a media frame.

**The honest cost:** we own a hand-written client against LiveKit's API and must
pin the server version. Mitigate with a version assertion at boot (`GET /` on the
LiveKit host reports the version) logged loudly, and a compat note in the deploy
docs. **Named fallback:** if the Twirp surface proves annoying, take
`github.com/livekit/protocol` (auth helpers + generated Twirp clients) — far
lighter than `server-sdk-go`. Decide it at implementation time on evidence, not
now.

**Client-side JS dependency `livekit-client` is unavoidable and taken.** The
alternative is writing a WebRTC client against LiveKit's protobuf signaling
protocol.

---

## Data model — migration 017

13 is used; the in-flight work takes 014 (SSO), 015 (collab) and 016. **This plan
takes `017_huddles`.**

The polymorphic scope column follows the precedent set by
`migrations/015_collab.up.sql`, which stores `resource_type TEXT` / `resource_id
UUID` with a shape CHECK and no FK, precisely so a second object type does not
need a migration. Same trade here: no cascade from `channels`, so channel
deletion must end huddles explicitly and the reaper is the backstop.

```sql
-- 017: Huddles — live audio/video rooms attached to a channel.
--
-- The authoritative record of *who is in the call* is LiveKit's, not this
-- schema's. These tables record the huddle's existence and its authorization
-- scope; participant rows are written ONLY by the LiveKit webhook sink and the
-- reconciler, never by a client saying "I joined". See "The hard part".

CREATE TABLE huddles (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- The object the huddle hangs off. 'channel' is the only type v1 registers;
    -- 'issue' is why this is a pair of columns rather than a channel_id FK.
    scope_type TEXT NOT NULL,
    scope_id   UUID NOT NULL,

    -- The LiveKit room name. Derived from this row's id, never from the scope:
    -- a re-started huddle must land in a fresh room, or it inherits the
    -- participants and state of the one that just ended.
    room_name TEXT NOT NULL,

    title      TEXT NOT NULL DEFAULT '',
    started_by UUID REFERENCES users(id) ON DELETE SET NULL,

    -- The v1 cap, in the schema so it cannot drift from the value passed to
    -- LiveKit's CreateRoom. LiveKit is what actually enforces it (see Risks).
    max_participants INT NOT NULL DEFAULT 10,
    peak_participants INT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at   TIMESTAMPTZ,
    -- 'empty' | 'ended_by_user' | 'reconciled' | 'scope_deleted' | 'expired'
    ended_reason TEXT,

    CONSTRAINT huddles_scope_type_valid CHECK (scope_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    CONSTRAINT huddles_room_name_key UNIQUE (room_name),
    CONSTRAINT huddles_max_participants_bounded CHECK (max_participants BETWEEN 2 AND 10),
    CONSTRAINT huddles_ended_consistent CHECK ((ended_at IS NULL) = (ended_reason IS NULL))
);

-- At most one live huddle per scope. This is the whole concurrency story for
-- "two people click Huddle at the same moment": the second INSERT conflicts and
-- the handler falls through to joining the first. Without it the two clients
-- end up in different rooms and neither can hear the other.
CREATE UNIQUE INDEX uniq_huddle_live_scope
    ON huddles (scope_type, scope_id) WHERE ended_at IS NULL;

CREATE INDEX idx_huddles_workspace_time ON huddles (workspace_id, created_at DESC);
CREATE INDEX idx_huddles_live ON huddles (workspace_id) WHERE ended_at IS NULL;

-- One row per LiveKit *session*, not per user: the same person may rejoin, and
-- LiveKit's participant sid is what identifies the session both sides can agree
-- on. joined_at/left_at are LiveKit's timestamps, not ours.
CREATE TABLE huddle_participants (
    huddle_id       UUID NOT NULL REFERENCES huddles(id) ON DELETE CASCADE,
    participant_sid TEXT NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    left_at   TIMESTAMPTZ,
    -- 'client' | 'removed' | 'room_ended' | 'reconciled'
    left_reason TEXT,

    is_screen_sharing BOOLEAN NOT NULL DEFAULT FALSE,
    has_video         BOOLEAN NOT NULL DEFAULT FALSE,

    PRIMARY KEY (huddle_id, participant_sid)
);

-- "Who is in it right now", the only hot read.
CREATE INDEX idx_huddle_participants_live
    ON huddle_participants (huddle_id) WHERE left_at IS NULL;
-- "What calls was this person in", for the history view and for the
-- offboarding question plan 00 exists to answer.
CREATE INDEX idx_huddle_participants_user ON huddle_participants (user_id, joined_at DESC);

-- Webhook idempotency. LiveKit delivers at-least-once and retries; without this
-- a retried participant_joined re-opens a session that already ended.
CREATE TABLE huddle_webhook_events (
    event_id    TEXT PRIMARY KEY,
    event_type  TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_huddle_webhook_events_received ON huddle_webhook_events (received_at);
```

`huddle_webhook_events` is swept by a worker job at 7 days. It is a dedupe
window, not a log.

---

## API surface

Conventions unchanged: `RegisterRoutes(mux, authMw)`, `{data,meta,error}` via
`httputil.JSON`/`JSONList`, keyset cursors via `httputil.ParsePagination`,
`err != nil` → 500 and `!ok` → 403/404 never collapsed.

```
POST   /api/v1/channels/{channel_id}/huddle          start-or-join; idempotent
GET    /api/v1/channels/{channel_id}/huddle          the live huddle, or 404
GET    /api/v1/huddles/{huddle_id}                   huddle + live participants
POST   /api/v1/huddles/{huddle_id}/join              mint a join token (reconnect path)
POST   /api/v1/huddles/{huddle_id}/leave             advisory only; see below
POST   /api/v1/huddles/{huddle_id}/end               end for everyone
GET    /api/v1/huddles/{huddle_id}/ice               ICE servers for this session
GET    /api/v1/workspaces/{workspace_id}/huddles     history, keyset paginated
POST   /api/v1/admin/huddle/test                     operator verification (adminMw + own limiter)
POST   /api/v1/huddles/webhook                       LiveKit event sink — NO authMw
```

Nine routes. Notes on the ones that are not obvious:

**`POST /channels/{id}/huddle`** — the only route the UI's "Huddle" button calls.
It resolves the channel through `authz.Checker.Channel`, requires
`IsChannelMember` (see below), `INSERT ... ON CONFLICT DO NOTHING` against
`uniq_huddle_live_scope`, falls back to `SELECT` on conflict, calls LiveKit
`CreateRoom` (idempotent server-side), and returns `{huddle, token, ws_url,
ice}`. First caller gets 201, everyone after gets 200.

**Join requires `IsChannelMember`, not `CanReadChannel`.** `CanReadChannel`
(`authz.go:214`) returns true for any workspace member on a *public* channel. A
workspace member browsing `#design` should see that a huddle is happening — that
is a read — but joining a live call is a stronger act than reading. v1: seeing
the huddle bar uses `CanReadChannel`; minting a token uses `IsChannelMember`. A
non-member joins the channel first. This is a product decision, stated so it is
reviewable rather than discovered.

**`POST /huddles/{id}/leave` is advisory.** It publishes an optimistic UI event
and nothing else. The participant row closes when LiveKit says the participant
left. A client that crashes never calls it, and a client that lies about it must
not be able to remove someone else from the roster.

**`POST /huddles/webhook`** is registered outside `authMw` — LiveKit has no
SuperOps identity. It gets:
- constant-time JWT verification against `HUDDLE_LIVEKIT_API_SECRET`, including
  the `sha256` body-hash claim (verify the hash before parsing the JSON);
- `DecodeJSONLimit` at 64 KiB;
- its own IP rate limiter chained like `RegisterMailRoutes` does
  (`internal/app/app.go:352`), because the general API limiter buckets by
  authenticated user and every webhook arrives from one unauthenticated IP;
- an optional `HUDDLE_WEBHOOK_ALLOWED_CIDRS` allowlist, off by default.

**`GET /huddles/{id}/ice`** — be clear about what this is:

```json
{"data":{"provider":"livekit","ttl_seconds":300,"relay_configured":true,
  "ice_servers":[{"urls":["turn:rtc.example.com:3478?transport=udp",
                          "turns:rtc.example.com:5349?transport=tcp"],
                  "username":"1753500000:8f3c…","credential":"…"}]}}
```

**When LiveKit is the provider this endpoint is not on the media path.** LiveKit
hands the client its ICE configuration — including credentials for its embedded
TURN, derived from the join token and therefore already short-lived and
per-session — inside the signaling JoinResponse. The endpoint exists for two
real reasons: (1) a client-side **pre-flight** so we can tell a user on a
symmetric-NAT corporate network "this network can't reach a relay" *before*
showing them a Join button that dies, which needs real credentials to test
against; (2) §3c rule 3, an operator needs something to curl. Do not let a
reviewer come away believing the call depends on it.

Credentials are minted per request, HMAC'd, and expire in `HUDDLE_ICE_TTL`
(default 5 min). Static credentials are never in the response and never in the
client bundle — that is the failure this endpoint exists to prevent.

---

## Package layout

```
internal/rtc/          the deployment-dependent capability (modelled on internal/mail)
  provider.go          Provider + ICEProvider interfaces, NewProvider(cfg) with boot validation
  livekit.go           token minting, Twirp room admin, webhook verification
  disabled.go          safe default: every call returns ErrDisabled (→ 503, clear message)
  ice_stun.go          STUN-only ICE provider + the startup warning
  ice_secret.go        coturn/RFC-style HMAC TURN REST credentials
  errors.go            ErrDisabled, permanent-vs-transient classification

internal/huddle/       the domain (modelled on internal/notification)
  model.go             Huddle, Participant, wire shapes
  repository.go        every SQL statement; nothing else touches these tables
  service.go           start/join/end lifecycle, event publishing, authz calls
  handler.go           RegisterRoutes(mux, authMw) + RegisterWebhookRoute(mux, mw)
  webhook.go           LiveKit event → repository, idempotent
  reconcile.go         the drift job; called from cmd/worker
```

**Reused, not rebuilt:** `internal/authz` (join authorization), `internal/ws`
(fan-out — huddle adds `PublishHuddle*` alongside `PublishChannelCreated` in
`hub.go:315` and three cases in `relay.go:45`), `internal/notification` (a new
`TypeHuddleStarted`), `internal/audit` (`huddle.started`, `huddle.ended`,
`huddle.participant_removed`), `cmd/worker`'s `start()` loop, `pkg/httputil`,
`pkg/database.WithTx`.

**Explicitly not built:** no new WebSocket message types beyond the outbound
`huddle.started` / `huddle.participant_changed` / `huddle.ended`; no new NATS
subject scheme (`superops.{ws}.huddle.{action}` fits the existing wildcard
`superops.*.>` the relay already subscribes to, `relay.go:24`); no new datastore;
no media code of any kind in this repo.

Config follows `MailConfig` line for line: `HUDDLE_PROVIDER` (`disabled` default
| `livekit`), `HUDDLE_LIVEKIT_URL`, `_API_KEY`, `_API_SECRET`,
`HUDDLE_PUBLIC_WS_URL` (what the *client* dials, which is not what the backend
dials), `HUDDLE_TOKEN_TTL` (60s), `HUDDLE_MAX_PARTICIPANTS` (10),
`HUDDLE_ICE_PROVIDER` (`sfu` | `secret` | `stun`), `HUDDLE_ICE_TTL`,
`HUDDLE_TURN_URLS`, `HUDDLE_TURN_SECRET`. Selecting `livekit` without a key or
secret is a boot failure, like `MAIL_TRANSPORT=smtp` with no host
(`config.go:557`). `HUDDLE_ICE_PROVIDER=stun` with no relay logs a
`Warn` at boot naming the 15–20% figure — a documented default, never a silent
one.

---

## The hard part

**The huddle's state lives in a system we do not control, and the authorization
decision has to survive that.**

Media is bought. What is not bought is consistency, and there are two distinct
failures underneath it.

### 1. Two sources of truth for "who is in the call"

Postgres has `huddle_participants`. LiveKit has the room. The WS hub has
presence. Three views of the same fact, updated by different paths at different
times, and every one of them is what some part of the UI renders.

They drift in ordinary operation, not just under failure: a browser tab closed
without a close frame stays in LiveKit until its timeout; a webhook lost in a
30-second network blip between the backend and LiveKit leaves Postgres claiming
five people are in a call that ended twenty minutes ago; a duplicate webhook
retry re-opens a session that already closed. This is exactly the shape of bug
this codebase has already been burned by — presence had to be refcounted
(`internal/presence/service.go:49`) and subscriptions needed both an active
revoke and a periodic sweep (`ws/client.go:423`) for the same reason.

How to attack it — five rules, in priority order:

1. **Split authority explicitly.** LiveKit is authoritative for *presence in the
   call*. Postgres is authoritative for *the huddle's existence and its
   authorization scope*. Postgres must never assert participant state it did not
   learn from LiveKit.
2. **Webhooks are the only writer of participant rows.** Not the join handler,
   not the leave handler, not the client. The join handler mints a token; that is
   all it does. This one rule removes the entire class of "the client said it
   joined but never did".
3. **Idempotency at the sink.** `INSERT INTO huddle_webhook_events (event_id)
   ... ON CONFLICT DO NOTHING` in the same transaction as the state change,
   `RETURNING` to detect the duplicate. At-least-once delivery is a property of
   the transport; dedupe is ours.
4. **A reconciler, because webhooks are lossy.** In `cmd/worker`, every 30s:
   `ListRooms` for every huddle with `ended_at IS NULL`; a huddle whose room
   LiveKit does not know about is ended with `ended_reason='reconciled'` and its
   open participants closed; for rooms that do exist, `ListParticipants` and
   diff. This is the same backstop-behind-the-fast-path shape as
   `recheckSubscriptions`, and it goes in the same place as `jobRetention` and
   `jobObjectGC` (`cmd/worker/main.go:280`). It runs on the worker, not the API,
   so it runs once per deployment rather than once per replica.
5. **Every roster the UI shows is derived from one query**, `WHERE left_at IS
   NULL`, and the WS event carries the whole roster rather than a delta. A delta
   protocol over a lossy source of truth is a resync bug waiting to be written;
   rosters are ≤10 entries.

### 2. A bearer token that outlives the permission that minted it

A join token is a JWT LiveKit validates on its own. Once the participant is
connected, the token is irrelevant — LiveKit will not re-check it, and it has no
idea what a channel membership is. So *revoking access mid-call is not
expressible as a token policy*. Someone removed from `#design` at 14:03 stays in
the huddle until something actively evicts them.

- **Short TTL bounds the join window, not the session.** 60 seconds, `jti` set,
  minted per call to `/join`. Reconnect re-mints, which re-checks authorization.
  A token stolen from devtools is useless a minute later.
- **Active eviction is the real mechanism.** Wire
  `rtc.Provider.RemoveParticipant` into every place that already calls
  `hub.RevokeChannelSubscription` — channel member removal, channel archive,
  channel delete, workspace removal. Those call sites exist and are already the
  audited path for "this person no longer has access here".
- **A periodic authorization recheck for live participants**, on the same
  worker pass as the reconciler: for every open participant row, re-ask
  `IsChannelMember`; on `false`, remove and close. Same cadence and same
  reasoning as `membershipRecheckPeriod` (`ws/client.go:64`) — the active path
  is primary, the sweep is what catches the case where it did not fire.
- **A database error must never evict.** `(false, nil)` removes; `err != nil`
  logs and leaves the participant alone. The existing rule, and here the failure
  mode is dropping everyone out of every call at once during a Postgres blip.

### Why this is the hard part and not "there is a lot of CRUD"

The CRUD is a day. The five rules above are what stop the product from
confidently telling four people that a fifth is in a call they left an hour ago,
and what stop a fired employee from staying on a call about their firing. Both
are the kind of bug that is invisible in a demo and obvious in production.

---

## Sequencing inside the phase

1. **`internal/rtc` + config + boot validation + `POST /admin/huddle/test`.**
   Nothing user-visible. Fully unit-testable without a LiveKit anywhere: token
   claims, webhook verification, ICE credential HMAC, the `disabled` provider's
   503. Unblocks everything else. **S–M.**
2. **Migration 017, repository, lifecycle handlers, WS events.** A huddle can be
   started, listed and ended, and everyone in the channel sees it. There is no
   media yet and that is fine — the whole roster surface is testable at this
   point. **M.**
3. **Webhook sink + idempotency + reconciler + eviction hooks.** The correctness
   core. **M.** Parallel with (4) once (2) has landed; the interface between them
   is the WS event payloads, which (2) fixes.
4. **Web client** — join UI, participant tiles, mic/camera/device pickers,
   screen share, the in-channel huddle bar, the pre-flight ICE check, reconnect.
   **L, and the long pole.** Everything upstream of it is bounded; this is not.
5. **Deployment**: compose service, Helm, port/firewall documentation, the
   operator runbook. **M.** Parallel from the start — it needs nobody.
6. **Notifications, push, audit.** **S.** Last because it is additive and its
   absence is not a correctness problem.

**Long pole: the web client** (4), with the reconciler (3) as the thing most
likely to still be producing bugs after everything else is green.

Cut from v1 and scheduled separately: the mobile dev-build path.

---

## The mobile story

`react-native-webrtc` — and therefore `@livekit/react-native` — **does not run in
Expo Go.** It needs a config plugin and a development build. The app today is a
clean managed Expo project: `app/package.json` has no `expo-dev-client`, no
native modules requiring prebuild, and `app/app.json` has no plugins array.
Adding LiveKit's native modules changes that for every contributor, permanently:
`npx expo prebuild`, EAS or local native builds, and "clone and `expo start`"
stops being how you run the app.

ROADMAP §3 recommends option (1), web-first, and this plan takes it.

**v1 mobile behaviour:** the huddle bar renders in the channel with the live
roster (it is a WS event and a REST read — nothing platform-specific), the push
notification fires, and the join affordance opens `PUBLIC_BASE_URL` in the
system browser. Mobile is a participant in the *awareness* of the huddle and not
in the media.

**What the follow-on costs, stated so it is not discovered later.** Adding
`expo-dev-client` + `@livekit/react-native` + `@livekit/react-native-webrtc` +
the config plugin, plus `NSMicrophoneUsageDescription` /
`NSCameraUsageDescription` and `RECORD_AUDIO` / `CAMERA`, plus an EAS build
pipeline and a CI change. **M**, and it is a one-way door on the Expo Go
property. Screen share on mobile is a further step (iOS needs a Broadcast Upload
Extension) and should be assumed out of scope even then.

**The escape hatch, if mobile audio is demanded before that:** `react-native-webview`
hosting `livekit-client` — §3's option (2). It still needs the permission strings
in the native manifests, so it still needs a dev build. There is no path to
mobile audio that keeps Expo Go. Say it once, plainly, and let the decision be
made on that basis.

---

## Risks and failure modes

**Bandwidth is the number that surprises operators.** Ten participants at 720p
and ~1.5 Mbps each: the SFU ingests ~15 Mbps and egresses up to ~135 Mbps for
*one room*. On the relayed path (~15–20% of connections, ROADMAP §3c) that
traffic goes through the TURN box on a real public IP with real egress billing.
Mitigate by enabling simulcast and LiveKit's dynacast/adaptive-stream in the
client config — not optional, defaults in our config file — and by putting the
10-person cap in the schema. Document the arithmetic in the deploy guide;
"LiveKit needs bandwidth" is not actionable, "one 10-person call is 135 Mbps
out" is.

**LiveKit is not a normal Kubernetes workload.** It needs a wide UDP port range
(50000–60000), TCP 7881, TURN 3478/5349, a public IP and a TLS certificate for
TURN. That means `hostNetwork` or a per-replica LoadBalancer — a Service with a
ClusterIP in front of a Deployment will not work and will fail in a way that
looks like "some people can't connect". Ship a single-node LiveKit for v1
(sufficient for 10-person rooms) and document multi-node as a separate exercise.
One good seam: LiveKit's multi-node coordination uses Redis, and we already run
one.

**The >10 cap has a race and the schema does not fix it.** Two joins can both
read a live count of 9. Since participant rows are written by webhooks, the count
at mint time is stale by construction. Resolution: enforce softly at mint (fast
rejection, good error message) and **authoritatively in LiveKit** by passing
`max_participants` to `CreateRoom` — which is also why we create rooms
explicitly instead of relying on auto-create on first join.

**The webhook endpoint is public.** Forged events could end a call or fabricate
participants. Verify the body hash *before* parsing, use `hmac.Equal`-style
constant-time comparison, cap the body, rate-limit by IP, and offer the CIDR
allowlist. Treat an unverified request as 401 with no body detail.

**Clock skew invalidates tokens.** A 60-second TTL and a backend 90 seconds
ahead of the LiveKit host mints tokens that are never valid. Set `nbf` 30s in the
past, and have the admin test report LiveKit's clock vs ours.

**LiveKit down.** Huddles must fail closed and *visibly*: `CreateRoom` failure is
a 503 with the transport's real error (redacted, `mail.Redact` pattern), never a
huddle row that exists with no room behind it. Create the room before committing
the huddle insert, or clean up on failure.

**Privacy.** Even with recording cut, `huddle_participants` is a durable record
of who met whom and for how long. Add it to the retention job with its own
window, and to the plan-00 "what did this person have access to" surface.

**Multi-replica is free here, and it is worth noticing why.** A webhook may land
on any API replica: it writes Postgres and publishes to NATS, and the relay
delivers on whichever replica the client happens to be on. No sticky sessions, no
replica affinity, no new coordination — because the hub already solved this.

---

## Verification

The integration suite runs `app.New` against real Postgres/Redis/NATS
(`test/integration/harness_test.go:185`). **Do not put a real SFU in CI.** Test
the seam: a `httptest.Server` implementing the four Twirp methods we call,
injected as the LiveKit base URL, that can be told to fail.

New `test/integration/huddle_test.go`:

- start → 201; a second concurrent start → 200 with the *same* huddle id (the
  partial unique index, driven from two goroutines);
- a channel member who is not in the workspace's other channels cannot join
  theirs — add the case to `tenancy_test.go` next to `TestCrossChannelIDOR`;
- a workspace member who can *read* a public channel but is not a member: sees
  the huddle in `GET`, is refused by `POST /join` (the `CanReadChannel` vs
  `IsChannelMember` decision, asserted rather than assumed);
- the minted token decodes with the configured secret and carries the expected
  `sub`, `room`, `exp` and `video` grant — no participant may be minted with
  `roomAdmin`;
- webhook with a valid signature but a body that does not match the `sha256`
  claim → 401, **and no row written**;
- the same `event_id` twice → exactly one participant row;
- `participant_left` closes the row; `room_finished` ends the huddle and closes
  every open row;
- the reconciler ends a huddle the fake LiveKit no longer lists, and does *not*
  end one it does;
- removing a channel member during a live huddle closes their participant row
  and issues a `RemoveParticipant` call to the fake (assert the call, not just
  the row);
- LiveKit unreachable → `POST /channels/{id}/huddle` is 503 and leaves no
  orphan `huddles` row.

Unit tests in `internal/rtc`: token claim shape and TTL; webhook JWT
verification including expiry and a wrong secret; TURN HMAC credential vectors;
`NewProvider` boot validation refusing `livekit` with a missing secret (mirroring
`config_mail_test.go`).

Manual/operator: a `livekit` profile in `docker-compose.dev.yml`, and
`POST /api/v1/admin/huddle/test` which reaches LiveKit, reports its version and
clock, mints a throwaway token, fetches ICE and returns the real error on
failure — §3c rule 3.

**The `-race` requirement matters here.** The reconciler, the webhook sink and
the leave handler all mutate the same rows.

---

## Sizing

| Piece | Size |
|---|---|
| `internal/rtc` (token, room admin, webhook verify, ICE providers, config) | S–M |
| Migration 017, repository, lifecycle handlers, WS events | M |
| Webhook sink, idempotency, reconciler, eviction hooks | M |
| **Web client** (join, tiles, devices, screen share, huddle bar, pre-flight, reconnect) | **L — long pole** |
| Deployment: compose, Helm, ports/TLS, runbook | M |
| Notifications, push, audit | S |
| *(cut from v1)* mobile dev-build path | M |

**Overall M**, consistent with ROADMAP §5 — and the M is only true because
LiveKit is chosen. The backend is the smaller half. If this slips, it slips in
the client.

---

## Gaps against plan 00 (object permissions)

Stated as gaps rather than worked around, per the instruction.

1. **A huddle should not become an ACL object type.** It has no independent
   access story — it is exactly as accessible as its scope. When 00 lands,
   `Capability(subject, huddle)` should resolve to `Capability(subject, scope)`
   rather than getting its own `acl_object` row. If it gets one, every huddle
   creation becomes an ACL write and the two can disagree.
2. **00's capability ladder has no rung for "may join a live call".** `read` is
   too weak (a public-channel reader should not join) and `write` is about
   content. v1 sidesteps it with `IsChannelMember`, which is honest but does not
   generalize to an issue-scoped huddle. 00 should either add a rung or accept
   that huddle join is scope-membership rather than a capability — a decision
   worth making deliberately, not by default.
3. **00's revocation section covers WS subscriptions; it does not cover
   revoking a session in a third-party system.** `Checker.Revoke` and
   `Checker.Move` should emit a revocation *event* that subscribers act on,
   rather than calling `hub.RevokeChannelSubscription` directly — otherwise
   huddle (and every future pillar with an external session) has to be
   hand-wired into the checker. This is a small change to 00's API shape and a
   large one to how many packages 00 has to know about.
4. **`Move` on a channel with a live huddle changes who may be in it.** 00 lists
   revocation latency as a risk; here the latency is a person on a call they can
   no longer access. The reconciler's authorization sweep covers it within one
   pass, but the active path needs the hook from (3).
