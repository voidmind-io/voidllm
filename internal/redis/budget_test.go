package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newBudgetTestClient returns a Client with prefix "t:" on a fresh miniredis
// whose clock is fixed at now.
func newBudgetTestClient(t *testing.T, now time.Time) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(now)
	c, err := New("redis://"+mr.Addr(), "t")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestBudgetKeyLayout(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 3, 23, 30, 0, 0, time.FixedZone("west", -5*3600)) // Nov 4th 04:30 UTC
	if got := BudgetDayBucket(at); got != "d:2026-11-04" {
		t.Errorf("BudgetDayBucket() = %q, want d:2026-11-04 (UTC day)", got)
	}
	if got := BudgetMonthBucket(at); got != "m:2026-11" {
		t.Errorf("BudgetMonthBucket() = %q, want m:2026-11", got)
	}

	c := &Client{keyPrefix: "voidllm:"}
	tests := []struct {
		metric, bucket, scope, want string
	}{
		{BudgetMetricTokens, "d:2026-11-04", "key:k1", "voidllm:budget:tok:d:2026-11-04:key:k1"},
		{BudgetMetricCost, "m:2026-11", "user:org1:user1", "voidllm:budget:cost:m:2026-11:user:org1:user1"},
		{BudgetMetricTokens, "m:2026-11", "team:t1", "voidllm:budget:tok:m:2026-11:team:t1"},
		{BudgetMetricCost, "d:2026-11-04", "org:o1", "voidllm:budget:cost:d:2026-11-04:org:o1"},
	}
	for _, tc := range tests {
		if got := c.BudgetKey(tc.metric, tc.bucket, tc.scope); got != tc.want {
			t.Errorf("BudgetKey(%q, %q, %q) = %q, want %q", tc.metric, tc.bucket, tc.scope, got, tc.want)
		}
	}
	if got := c.budgetSeedKey("d:2026-11-04"); got != "voidllm:budget:seeded:d:2026-11-04" {
		t.Errorf("budgetSeedKey() = %q", got)
	}
}

func TestIncrAndGetBudgets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	c, mr := newBudgetTestClient(t, now)
	ctx := context.Background()

	a := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:o")
	b := c.BudgetKey(BudgetMetricCost, "m:2026-03", "org:o")
	missing := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:none")
	expire := now.Add(36 * time.Hour)

	if err := c.IncrBudgets(ctx, nil); err != nil {
		t.Fatalf("IncrBudgets(nil) error = %v", err)
	}
	for range 2 {
		err := c.IncrBudgets(ctx, []BudgetIncrement{
			{Key: a, Delta: 40, ExpireAt: expire},
			{Key: b, Delta: 1_000_000_000_000, ExpireAt: expire},
		})
		if err != nil {
			t.Fatalf("IncrBudgets() error = %v", err)
		}
	}

	got, err := c.GetBudgets(ctx, []string{missing, a, b})
	if err != nil {
		t.Fatalf("GetBudgets() error = %v", err)
	}
	want := []int64{0, 80, 2_000_000_000_000}
	if len(got) != len(want) {
		t.Fatalf("GetBudgets() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GetBudgets()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if ttl := mr.TTL(a); ttl != 36*time.Hour {
		t.Errorf("TTL = %v, want 36h", ttl)
	}

	if vals, err := c.GetBudgets(ctx, nil); err != nil || vals != nil {
		t.Errorf("GetBudgets(nil) = %v, %v, want nil, nil", vals, err)
	}

	// A value that is not an integer is an error, not a silent zero.
	if err := mr.Set(missing, "not-a-number"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}
	if _, err := c.GetBudgets(ctx, []string{missing}); err == nil {
		t.Error("GetBudgets() on a non-integer value error = nil, want an error")
	}
}

func TestBudgetSeedClaimAndCommit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	c, mr := newBudgetTestClient(t, now)
	ctx := context.Background()

	const bucket = "d:2026-03-10"
	counter := c.BudgetKey(BudgetMetricTokens, bucket, "org:o")
	expire := now.Add(36 * time.Hour)
	incs := []BudgetIncrement{{Key: counter, Delta: 500, ExpireAt: expire}}

	if done, err := c.BudgetSeedDone(ctx, bucket); err != nil || done {
		t.Fatalf("BudgetSeedDone() before any claim = %v, %v, want false, nil", done, err)
	}

	claimed, err := c.ClaimBudgetSeed(ctx, bucket, "pending:a", 2*time.Minute)
	if err != nil || !claimed {
		t.Fatalf("first ClaimBudgetSeed() = %v, %v, want true, nil", claimed, err)
	}
	claimed, err = c.ClaimBudgetSeed(ctx, bucket, "pending:b", 2*time.Minute)
	if err != nil || claimed {
		t.Fatalf("second ClaimBudgetSeed() = %v, %v, want false, nil", claimed, err)
	}
	if done, err := c.BudgetSeedDone(ctx, bucket); err != nil || done {
		t.Fatalf("BudgetSeedDone() while claimed = %v, %v, want false, nil", done, err)
	}

	// The loser must not be able to commit.
	committed, err := c.CommitBudgetSeed(ctx, bucket, "pending:b", incs, expire)
	if err != nil || committed {
		t.Fatalf("CommitBudgetSeed() with a foreign token = %v, %v, want false, nil", committed, err)
	}
	if mr.Exists(counter) {
		t.Fatal("a rejected commit must not touch the counters")
	}

	committed, err = c.CommitBudgetSeed(ctx, bucket, "pending:a", incs, expire)
	if err != nil || !committed {
		t.Fatalf("CommitBudgetSeed() = %v, %v, want true, nil", committed, err)
	}
	if got, _ := mr.Get(counter); got != "500" {
		t.Errorf("counter = %q, want 500", got)
	}
	if done, err := c.BudgetSeedDone(ctx, bucket); err != nil || !done {
		t.Errorf("BudgetSeedDone() after commit = %v, %v, want true, nil", done, err)
	}
	if ttl := mr.TTL(c.budgetSeedKey(bucket)); ttl != 36*time.Hour {
		t.Errorf("marker TTL = %v, want 36h (expires with the bucket)", ttl)
	}

	// Committing again, or claiming again, changes nothing.
	committed, err = c.CommitBudgetSeed(ctx, bucket, "pending:a", incs, expire)
	if err != nil || committed {
		t.Errorf("second CommitBudgetSeed() = %v, %v, want false, nil", committed, err)
	}
	claimed, err = c.ClaimBudgetSeed(ctx, bucket, "pending:c", 2*time.Minute)
	if err != nil || claimed {
		t.Errorf("ClaimBudgetSeed() on a seeded bucket = %v, %v, want false, nil", claimed, err)
	}
	if got, _ := mr.Get(counter); got != "500" {
		t.Errorf("counter = %q after repeated commit, want 500", got)
	}
}

func TestBudgetSeedClaimExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	c, mr := newBudgetTestClient(t, now)
	ctx := context.Background()

	const bucket = "m:2026-03"
	counter := c.BudgetKey(BudgetMetricTokens, bucket, "org:o")
	expire := now.Add(30 * 24 * time.Hour)
	incs := []BudgetIncrement{{Key: counter, Delta: 9, ExpireAt: expire}}

	if claimed, err := c.ClaimBudgetSeed(ctx, bucket, "pending:dead", time.Minute); err != nil || !claimed {
		t.Fatalf("ClaimBudgetSeed() = %v, %v, want true, nil", claimed, err)
	}
	mr.FastForward(61 * time.Second)

	// The original claimant comes back too late.
	if committed, err := c.CommitBudgetSeed(ctx, bucket, "pending:dead", incs, expire); err != nil || committed {
		t.Fatalf("CommitBudgetSeed() after expiry = %v, %v, want false, nil", committed, err)
	}
	if claimed, err := c.ClaimBudgetSeed(ctx, bucket, "pending:next", time.Minute); err != nil || !claimed {
		t.Fatalf("ClaimBudgetSeed() after expiry = %v, %v, want true, nil", claimed, err)
	}
	if committed, err := c.CommitBudgetSeed(ctx, bucket, "pending:next", incs, expire); err != nil || !committed {
		t.Fatalf("CommitBudgetSeed() by the new claimant = %v, %v, want true, nil", committed, err)
	}
	if got, _ := mr.Get(counter); got != "9" {
		t.Errorf("counter = %q, want 9", got)
	}
}
