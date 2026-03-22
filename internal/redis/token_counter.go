package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// AddTokens increments token usage counters for the key, team (when non-empty),
// and org scopes in both daily and monthly buckets using a single pipelined
// write. TTL is set on each key so counters are automatically evicted: daily
// keys expire after 48 hours, monthly keys after 35 days. Setting the TTL on
// every write is idempotent — Redis only updates it on an existing key, which
// is the desired behaviour.
func (c *Client) AddTokens(ctx context.Context, keyID, teamID, orgID string, tokens int64) error {
	now := time.Now().UTC()
	dailyBucket := now.Format("2006-01-02")
	monthlyBucket := now.Format("2006-01")

	dailyKeyKey := c.key("token", "daily", "key", keyID, dailyBucket)
	monthlyKeyKey := c.key("token", "monthly", "key", keyID, monthlyBucket)
	dailyOrgKey := c.key("token", "daily", "org", orgID, dailyBucket)
	monthlyOrgKey := c.key("token", "monthly", "org", orgID, monthlyBucket)

	pipe := c.rdb.Pipeline()

	pipe.IncrBy(ctx, dailyKeyKey, tokens)
	pipe.IncrBy(ctx, monthlyKeyKey, tokens)
	pipe.Expire(ctx, dailyKeyKey, 48*time.Hour)
	pipe.Expire(ctx, monthlyKeyKey, 35*24*time.Hour)

	if teamID != "" {
		dailyTeamKey := c.key("token", "daily", "team", teamID, dailyBucket)
		monthlyTeamKey := c.key("token", "monthly", "team", teamID, monthlyBucket)
		pipe.IncrBy(ctx, dailyTeamKey, tokens)
		pipe.IncrBy(ctx, monthlyTeamKey, tokens)
		pipe.Expire(ctx, dailyTeamKey, 48*time.Hour)
		pipe.Expire(ctx, monthlyTeamKey, 35*24*time.Hour)
	}

	pipe.IncrBy(ctx, dailyOrgKey, tokens)
	pipe.IncrBy(ctx, monthlyOrgKey, tokens)
	pipe.Expire(ctx, dailyOrgKey, 48*time.Hour)
	pipe.Expire(ctx, monthlyOrgKey, 35*24*time.Hour)

	_, err := pipe.Exec(ctx)
	return err
}

// GetTokenUsage returns the current token count for a scope and window.
// scope must be "key", "team", or "org". window must be "daily" or "monthly".
// Returns 0 when no usage has been recorded yet (key does not exist in Redis).
func (c *Client) GetTokenUsage(ctx context.Context, scope, id, window string) (int64, error) {
	now := time.Now().UTC()
	var bucket string
	switch window {
	case "daily":
		bucket = now.Format("2006-01-02")
	case "monthly":
		bucket = now.Format("2006-01")
	default:
		return 0, fmt.Errorf("unknown window: %s", window)
	}

	key := c.key("token", window, scope, id, bucket)
	val, err := c.rdb.Get(ctx, key).Int64()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return 0, nil // no usage recorded yet
		}
		return 0, fmt.Errorf("redis get token usage: %w", err)
	}
	return val, nil
}
