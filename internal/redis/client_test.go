package redis

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

// TestNewUnverified verifies that a client can be created while Redis is
// down, reports that through Ping, and works once Redis is up, while New
// keeps refusing a server that does not answer.
func TestNewUnverified(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()
	mr.Close()
	ctx := context.Background()

	if _, err := NewUnverified("not a url", ""); err == nil {
		t.Error("NewUnverified() with an invalid URL error = nil, want an error")
	}
	if c, err := New(url, ""); err == nil {
		_ = c.Close()
		t.Fatal("New() against a server that is down error = nil, want an error")
	}

	c, err := NewUnverified(url, "")
	if err != nil {
		t.Fatalf("NewUnverified() error = %v, want nil although the server is down", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:o"); got != "voidllm:budget:tok:d:2026-03-10:org:o" {
		t.Errorf("default prefix: BudgetKey() = %q", got)
	}
	if err := c.Ping(ctx); err == nil {
		t.Error("Ping() against a server that is down error = nil, want an error")
	}

	if err := mr.Restart(); err != nil {
		t.Fatalf("miniredis Restart: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping() after the server came up error = %v, want nil", err)
	}
}
