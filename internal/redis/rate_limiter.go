package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// rateLimitScript is a Lua script that atomically increments a counter and
// sets its TTL on the first increment. Using a single script avoids the race
// between INCR and EXPIRE that would exist with two separate commands.
//
// KEYS[1] — the rate limit counter key
// ARGV[1] — TTL in seconds (window duration)
//
// Returns the new counter value after increment.
var rateLimitScript = goredis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return current
`)

// CheckRate atomically increments and checks a rate limit counter for the
// given scope and id within a fixed time window. It returns true when the
// request is within the limit and false when it is exceeded.
//
// The key encodes the current window boundary so counters self-expire without
// needing an explicit cleanup job. On any Redis error the function fails open
// (returns allowed=true) so that a Redis outage does not block all traffic.
func (c *Client) CheckRate(ctx context.Context, scope, id string, limit int, windowDuration time.Duration) (bool, error) {
	windowSeconds := int(windowDuration.Seconds())
	if windowSeconds <= 0 {
		windowSeconds = 60
	}

	// Embed the window epoch into the key so each window gets its own counter.
	windowStart := time.Now().Unix() / int64(windowSeconds)
	key := c.key("rate", scope, id, fmt.Sprintf("%d", windowStart))

	result, err := rateLimitScript.Run(ctx, c.rdb, []string{key}, windowSeconds).Int64()
	if err != nil {
		return true, fmt.Errorf("redis rate check: %w", err) // fail open
	}

	return result <= int64(limit), nil
}
