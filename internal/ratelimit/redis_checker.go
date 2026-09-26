package ratelimit

import (
	"context"
	"log/slog"
	"time"

	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// Compile-time assertion: RedisChecker must implement Checker.
var _ Checker = (*RedisChecker)(nil)

// rateCheck describes a single scope/window combination to evaluate against Redis.
type rateCheck struct {
	scope  string
	id     string
	limit  int
	window time.Duration
}

// RedisChecker evaluates rate limits using atomic Redis counters, enabling
// distributed enforcement across multiple VoidLLM instances. On any Redis
// error it fails open — the request is allowed and a warning is logged — so
// that a Redis outage never blocks traffic.
type RedisChecker struct {
	client *voidredis.Client
	log    *slog.Logger
}

// NewRedisChecker returns a RedisChecker backed by the given Redis client.
func NewRedisChecker(client *voidredis.Client, log *slog.Logger) *RedisChecker {
	return &RedisChecker{client: client, log: log}
}

// CheckRate verifies rate limits for the key, user (if non-empty), team (if
// non-empty), and org scopes against Redis. Each scope/window combination is
// checked individually; the first exceeded limit causes ErrRateLimitExceeded
// to be returned. Checks with a zero limit are skipped (unlimited). The user
// scope uses id OrgID+":"+UserID because per-user limits are defined on the
// org membership. On Redis error the individual check is skipped and the
// request is allowed (fail-open).
func (r *RedisChecker) CheckRate(s Scopes, l ScopeLimits) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	checks := []rateCheck{
		{"key", s.KeyID, l.Key.RequestsPerMinute, time.Minute},
		{"key", s.KeyID, l.Key.RequestsPerDay, 24 * time.Hour},
	}

	if s.UserID != "" {
		userScopeID := s.OrgID + ":" + s.UserID
		checks = append(checks,
			rateCheck{"user", userScopeID, l.User.RequestsPerMinute, time.Minute},
			rateCheck{"user", userScopeID, l.User.RequestsPerDay, 24 * time.Hour},
		)
	}

	if s.TeamID != "" {
		checks = append(checks,
			rateCheck{"team", s.TeamID, l.Team.RequestsPerMinute, time.Minute},
			rateCheck{"team", s.TeamID, l.Team.RequestsPerDay, 24 * time.Hour},
		)
	}

	checks = append(checks,
		rateCheck{"org", s.OrgID, l.Org.RequestsPerMinute, time.Minute},
		rateCheck{"org", s.OrgID, l.Org.RequestsPerDay, 24 * time.Hour},
	)

	for _, c := range checks {
		if c.limit <= 0 {
			continue
		}
		allowed, err := r.client.CheckRate(ctx, c.scope, c.id, c.limit, c.window)
		if err != nil {
			r.log.Warn("redis rate check failed, allowing request",
				slog.String("scope", c.scope),
				slog.String("error", err.Error()),
			)
			continue // fail open
		}
		if !allowed {
			return ErrRateLimitExceeded
		}
	}

	return nil
}
