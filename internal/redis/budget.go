package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Metric segments of budget counter keys.
const (
	// BudgetMetricTokens is the key segment of token counters.
	BudgetMetricTokens = "tok"
	// BudgetMetricCost is the key segment of cost counters.
	BudgetMetricCost = "cost"
)

// budgetSeedDone is the value of a seed marker whose bucket has been
// rehydrated. Any other value is the claim token of a rehydration in progress.
const budgetSeedDone = "done"

// BudgetIncrement is one counter increment together with the instant the
// counter may be discarded.
type BudgetIncrement struct {
	// Key is the full counter key, as returned by BudgetKey.
	Key string
	// Delta is the amount to add.
	Delta int64
	// ExpireAt is when Redis evicts the counter.
	ExpireAt time.Time
}

// BudgetDayBucket returns the bucket segment for the UTC day containing t,
// e.g. "d:2026-11-23".
func BudgetDayBucket(t time.Time) string {
	return "d:" + t.UTC().Format("2006-01-02")
}

// BudgetMonthBucket returns the bucket segment for the UTC month containing
// t, e.g. "m:2026-11".
func BudgetMonthBucket(t time.Time) string {
	return "m:" + t.UTC().Format("2006-01")
}

// BudgetKey returns the key of the budget counter for a metric
// (BudgetMetricTokens or BudgetMetricCost), a bucket (BudgetDayBucket or
// BudgetMonthBucket) and a counter scope such as "team:<id>":
// "<prefix>budget:<metric>:<bucket>:<scope>".
func (c *Client) BudgetKey(metric, bucket, scope string) string {
	return c.key("budget", metric, bucket, scope)
}

// budgetSeedKey returns the key of the seed marker of a bucket:
// "<prefix>budget:seeded:<bucket>".
func (c *Client) budgetSeedKey(bucket string) string {
	return c.key("budget", "seeded", bucket)
}

// IncrBudgets applies all increments in one pipelined round trip. Each
// counter gets its expiry (re)set, which is idempotent because the expiry of
// a bucket never changes.
func (c *Client) IncrBudgets(ctx context.Context, incs []BudgetIncrement) error {
	if len(incs) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for _, inc := range incs {
		pipe.IncrBy(ctx, inc.Key, inc.Delta)
		pipe.ExpireAt(ctx, inc.Key, inc.ExpireAt)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis incr budgets: %w", err)
	}
	return nil
}

// GetBudgets returns the values of the given counters in one MGET, in the
// order of keys. Counters that do not exist read as 0.
func (c *Client) GetBudgets(ctx context.Context, keys []string) ([]int64, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	raw, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis get budgets: %w", err)
	}
	if len(raw) != len(keys) {
		return nil, fmt.Errorf("redis get budgets: got %d values for %d keys", len(raw), len(keys))
	}
	vals := make([]int64, len(raw))
	for i, v := range raw {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("redis get budgets: unexpected value type %T", v)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis get budgets: parse counter: %w", err)
		}
		vals[i] = n
	}
	return vals, nil
}

// ClaimBudgetSeed tries to become the instance that rehydrates a bucket. It
// sets the seed marker of the bucket to token if no marker exists and reports
// whether it did. The claim expires after ttl, so a claimant that dies before
// CommitBudgetSeed does not block the bucket forever. token must be unique
// per attempt.
func (c *Client) ClaimBudgetSeed(ctx context.Context, bucket, token string, ttl time.Duration) (bool, error) {
	claimed, err := c.rdb.SetNX(ctx, c.budgetSeedKey(bucket), token, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis claim budget seed: %w", err)
	}
	return claimed, nil
}

// BudgetSeedDone reports whether the bucket has been rehydrated. It is false
// while the marker is missing or still holds a claim.
func (c *Client) BudgetSeedDone(ctx context.Context, bucket string) (bool, error) {
	v, err := c.rdb.Get(ctx, c.budgetSeedKey(bucket)).Result()
	if errors.Is(err, goredis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("redis get budget seed marker: %w", err)
	}
	return v == budgetSeedDone, nil
}

// CommitBudgetSeed applies the rehydration increments of a bucket and marks
// the bucket as seeded until markerExpireAt, all in one transaction that only
// runs while the marker still holds token. It reports false, without applying
// anything, when the claim was lost (expired, or taken over by another
// instance), so a bucket is never rehydrated twice.
func (c *Client) CommitBudgetSeed(ctx context.Context, bucket, token string, incs []BudgetIncrement, markerExpireAt time.Time) (bool, error) {
	marker := c.budgetSeedKey(bucket)
	errClaimLost := errors.New("claim lost")

	err := c.rdb.Watch(ctx, func(tx *goredis.Tx) error {
		v, err := tx.Get(ctx, marker).Result()
		if errors.Is(err, goredis.Nil) || (err == nil && v != token) {
			return errClaimLost
		}
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, inc := range incs {
				pipe.IncrBy(ctx, inc.Key, inc.Delta)
				pipe.ExpireAt(ctx, inc.Key, inc.ExpireAt)
			}
			pipe.Set(ctx, marker, budgetSeedDone, 0)
			pipe.ExpireAt(ctx, marker, markerExpireAt)
			return nil
		})
		return err
	}, marker)
	if errors.Is(err, errClaimLost) || errors.Is(err, goredis.TxFailedErr) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("redis commit budget seed: %w", err)
	}
	return true, nil
}
