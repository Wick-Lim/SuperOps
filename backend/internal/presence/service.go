package presence

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	presenceKeyPrefix = "presence:"
	// connsKeyPrefix holds the number of live WebSocket connections a user has.
	// Presence is refcounted: without it, one device disconnecting (or a
	// reconnect racing its predecessor's teardown) marked a still-connected
	// user offline.
	connsKeyPrefix = "presence-conns:"
	presenceTTL    = 5 * time.Minute
)

// evalFunc runs a Lua script and returns its integer result. It is a seam for
// tests; production always goes to Redis.
type evalFunc func(ctx context.Context, script *redis.Script, keys []string, args ...interface{}) (int64, error)

type Service struct {
	redis *redis.Client
	eval  evalFunc
}

func NewService(redis *redis.Client) *Service {
	return &Service{redis: redis}
}

func (s *Service) evalInt(ctx context.Context, script *redis.Script, keys []string, args ...interface{}) (int64, error) {
	if s.eval != nil {
		return s.eval(ctx, script, keys, args...)
	}
	return script.Run(ctx, s.redis, keys, args...).Int64()
}

func statusKey(userID string) string { return presenceKeyPrefix + userID }
func connsKey(userID string) string  { return connsKeyPrefix + userID }

// connectScript increments the connection refcount and, only if no status is
// stored yet, marks the user online — a second device must not reset an
// explicit away/dnd back to online. KEYS: conns, status. ARGV: ttl, status.
var connectScript = redis.NewScript(`
local conns = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
if redis.call('EXISTS', KEYS[2]) == 0 then
  redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])
end
return conns
`)

// disconnectScript decrements the refcount and clears presence only when the
// last connection goes away. Returns 1 when the user went offline.
// KEYS: conns, status. ARGV: ttl.
var disconnectScript = redis.NewScript(`
local conns = redis.call('DECR', KEYS[1])
if conns <= 0 then
  redis.call('DEL', KEYS[1])
  redis.call('DEL', KEYS[2])
  return 1
end
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 0
`)

// heartbeatScript refreshes both TTLs. The status key is re-created as online
// if it expired, but the refcount key is only extended if it still exists —
// resurrecting it would invent a connection.
// KEYS: conns, status. ARGV: ttl, online.
var heartbeatScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('EXPIRE', KEYS[2], ARGV[1])
else
  redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[1])
end
return 1
`)

// Connect registers one live connection for the user and returns the resulting
// connection count. A return of 1 means the user just came online.
func (s *Service) Connect(ctx context.Context, userID string) (int64, error) {
	conns, err := s.evalInt(ctx, connectScript,
		[]string{connsKey(userID), statusKey(userID)},
		int(presenceTTL.Seconds()), string(StatusOnline),
	)
	if err != nil {
		return 0, fmt.Errorf("register presence connection: %w", err)
	}
	return conns, nil
}

// Disconnect releases one connection and reports whether it was the last one,
// i.e. whether the user actually went offline.
func (s *Service) Disconnect(ctx context.Context, userID string) (bool, error) {
	offline, err := s.evalInt(ctx, disconnectScript,
		[]string{connsKey(userID), statusKey(userID)},
		int(presenceTTL.Seconds()),
	)
	if err != nil {
		return false, fmt.Errorf("release presence connection: %w", err)
	}
	return offline == 1, nil
}

// Heartbeat extends the presence TTL under a live socket. It preserves an
// away/dnd status rather than clobbering it back to online.
func (s *Service) Heartbeat(ctx context.Context, userID string) error {
	_, err := s.evalInt(ctx, heartbeatScript,
		[]string{connsKey(userID), statusKey(userID)},
		int(presenceTTL.Seconds()), string(StatusOnline),
	)
	if err != nil {
		return fmt.Errorf("refresh presence: %w", err)
	}
	return nil
}

// SetStatus records an explicitly chosen status. Offline is stored rather than
// deleted so that a user who chose to appear offline is not resurrected by the
// next heartbeat; only Disconnect clears the key.
func (s *Service) SetStatus(ctx context.Context, userID string, status Status) error {
	if err := s.redis.Set(ctx, statusKey(userID), string(status), presenceTTL).Err(); err != nil {
		return fmt.Errorf("set presence status: %w", err)
	}
	return nil
}

func (s *Service) GetStatus(ctx context.Context, userID string) Status {
	val, err := s.redis.Get(ctx, statusKey(userID)).Result()
	if err != nil {
		return StatusOffline
	}
	return Status(val)
}

func (s *Service) GetBulkStatus(ctx context.Context, userIDs []string) map[string]Status {
	result := make(map[string]Status, len(userIDs))
	if len(userIDs) == 0 {
		return result
	}

	pipe := s.redis.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(userIDs))
	for _, uid := range userIDs {
		cmds[uid] = pipe.Get(ctx, statusKey(uid))
	}
	// Exec's error is intentionally discarded: it reports only the first
	// failure, and a GET on a key that does not exist — the normal "user is
	// offline" case — surfaces as redis.Nil. Every command's own result is
	// inspected below, where an error correctly maps to StatusOffline.
	_, _ = pipe.Exec(ctx)

	for uid, cmd := range cmds {
		val, err := cmd.Result()
		if err != nil {
			result[uid] = StatusOffline
		} else {
			result[uid] = Status(val)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Typing indicators
// ---------------------------------------------------------------------------

// Typing state is one sorted set per channel — member = user id, score = the
// instant the entry expires. The previous per-user key layout could only be
// read back with `KEYS typing:{channel}:*`, an O(keyspace) command that blocks
// the single-threaded Redis shared with rate limiting and presence.
const (
	typingKeyPrefix = "typing:"
	typingTTL       = 3 * time.Second
)

func typingKey(channelID string) string { return typingKeyPrefix + channelID }

// typingScore is the score stored for an entry written at now: the instant it
// stops counting as typing.
func typingScore(now time.Time) int64 { return now.Add(typingTTL).UnixMilli() }

// typingCutoff is the highest score that has already expired at now. Entries
// scoring at or below it are pruned.
func typingCutoff(now time.Time) int64 { return now.UnixMilli() }

func (s *Service) SetTyping(ctx context.Context, channelID, userID string) error {
	key := typingKey(channelID)
	now := time.Now()

	pipe := s.redis.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(typingCutoff(now), 10))
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(typingScore(now)), Member: userID})
	// The newest entry expires exactly typingTTL from now, so the whole set can
	// go at the same moment and idle channels leave nothing behind.
	pipe.PExpire(ctx, key, typingTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("set typing: %w", err)
	}
	return nil
}

func (s *Service) ClearTyping(ctx context.Context, channelID, userID string) error {
	if err := s.redis.ZRem(ctx, typingKey(channelID), userID).Err(); err != nil {
		return fmt.Errorf("clear typing: %w", err)
	}
	return nil
}

func (s *Service) GetTypingUsers(ctx context.Context, channelID string) ([]string, error) {
	key := typingKey(channelID)

	pipe := s.redis.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(typingCutoff(time.Now()), 10))
	members := pipe.ZRange(ctx, key, 0, -1)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("get typing users: %w", err)
	}

	users, err := members.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("get typing users: %w", err)
	}
	if users == nil {
		users = []string{}
	}
	return users, nil
}
