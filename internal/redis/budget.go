package redis

import (
	"context"
	"fmt"
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

// BudgetValue is the usage one instance reports for one budget counter,
// together with the instant the counter may be discarded.
type BudgetValue struct {
	// Key is the full counter key, as returned by BudgetKey.
	Key string
	// Value is the absolute usage of the reporting instance.
	Value int64
	// ExpireAt is when Redis evicts the counter.
	ExpireAt time.Time
}

// budgetSumScript returns, for every key, the sum of all fields of the hash
// stored there (0 for a missing key). Summing on the server keeps a budget
// check at one round trip and one small reply regardless of how many
// instances reported into a counter. Sums are exact up to 2^53.
var budgetSumScript = goredis.NewScript(`
local sums = {}
for i, key in ipairs(KEYS) do
    local sum = 0
    for _, v in ipairs(redis.call('HVALS', key)) do
        sum = sum + tonumber(v)
    end
    sums[i] = sum
end
return sums
`)

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
// "<prefix>budget:<metric>:<bucket>:<scope>". The counter is a hash with one
// field per reporting instance.
func (c *Client) BudgetKey(metric, bucket, scope string) string {
	return c.key("budget", metric, bucket, scope)
}

// SetBudgets stores the given values as the usage of one instance, in one
// round trip: each value is written to the field named instanceID of its
// counter hash, and the counter gets its expiry (re)set. All writes of a call
// run in one MULTI/EXEC transaction, so Redis applies either all of them or
// none: a connection lost half way cannot leave a counter without an expiry.
// Values are absolute, so the call is idempotent: repeating it, or sending a
// newer value after a lost one, leaves the counter correct.
func (c *Client) SetBudgets(ctx context.Context, instanceID string, values []BudgetValue) error {
	if len(values) == 0 {
		return nil
	}
	pipe := c.rdb.TxPipeline()
	for _, v := range values {
		pipe.HSet(ctx, v.Key, instanceID, v.Value)
		pipe.ExpireAt(ctx, v.Key, v.ExpireAt)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis set budgets: %w", err)
	}
	return nil
}

// SumBudgets returns, in the order of keys, the usage all instances together
// reported for each counter, in one round trip. Counters that do not exist
// read as 0.
func (c *Client) SumBudgets(ctx context.Context, keys []string) ([]int64, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	sums, err := budgetSumScript.Run(ctx, c.rdb, keys).Int64Slice()
	if err != nil {
		return nil, fmt.Errorf("redis sum budgets: %w", err)
	}
	if len(sums) != len(keys) {
		return nil, fmt.Errorf("redis sum budgets: got %d sums for %d keys", len(sums), len(keys))
	}
	return sums, nil
}
