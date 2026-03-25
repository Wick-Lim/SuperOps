package presence

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	presenceKeyPrefix = "presence:"
	presenceTTL       = 5 * time.Minute
)

type Service struct {
	redis *redis.Client
}

func NewService(redis *redis.Client) *Service {
	return &Service{redis: redis}
}

func (s *Service) SetOnline(ctx context.Context, userID string) error {
	key := presenceKeyPrefix + userID
	return s.redis.Set(ctx, key, string(StatusOnline), presenceTTL).Err()
}

func (s *Service) SetStatus(ctx context.Context, userID string, status Status) error {
	key := presenceKeyPrefix + userID
	if status == StatusOffline {
		return s.redis.Del(ctx, key).Err()
	}
	return s.redis.Set(ctx, key, string(status), presenceTTL).Err()
}

func (s *Service) GetStatus(ctx context.Context, userID string) Status {
	key := presenceKeyPrefix + userID
	val, err := s.redis.Get(ctx, key).Result()
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
		cmds[uid] = pipe.Get(ctx, presenceKeyPrefix+uid)
	}
	pipe.Exec(ctx)

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

func (s *Service) Heartbeat(ctx context.Context, userID string) error {
	key := presenceKeyPrefix + userID
	exists, _ := s.redis.Exists(ctx, key).Result()
	if exists == 0 {
		return s.redis.Set(ctx, key, string(StatusOnline), presenceTTL).Err()
	}
	return s.redis.Expire(ctx, key, presenceTTL).Err()
}

// Typing indicators
const (
	typingKeyPrefix = "typing:"
	typingTTL       = 3 * time.Second
)

func (s *Service) SetTyping(ctx context.Context, channelID, userID string) error {
	key := fmt.Sprintf("%s%s:%s", typingKeyPrefix, channelID, userID)
	return s.redis.Set(ctx, key, "1", typingTTL).Err()
}

func (s *Service) GetTypingUsers(ctx context.Context, channelID string) []string {
	pattern := fmt.Sprintf("%s%s:*", typingKeyPrefix, channelID)
	keys, _ := s.redis.Keys(ctx, pattern).Result()

	var users []string
	prefixLen := len(fmt.Sprintf("%s%s:", typingKeyPrefix, channelID))
	for _, key := range keys {
		if len(key) > prefixLen {
			users = append(users, key[prefixLen:])
		}
	}
	return users
}
