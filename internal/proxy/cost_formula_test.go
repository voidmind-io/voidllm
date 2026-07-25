package proxy

// cost_formula_test.go tests the cost-estimate formula inside logUsageEvent
// (handler.go). Before #179 this formula had NO test coverage at all — it
// only priced PromptTokens and CompletionTokens; cached-read and cache-write
// buckets did not exist. This file exercises the four-term formula:
//
//	cost = fresh*InputPer1M + cachedRead*cachedRate + cacheWrite*writeRate + completion*OutputPer1M
//
// where fresh = PromptTokens - CachedReadTokens - CacheWriteTokens (clamped
// at zero), and each cache rate falls back to InputPer1M when its dedicated
// price is left at its zero-value "unconfigured" sentinel.
//
// logUsageEvent is unexported but this file lives in package proxy (white-box
// testing), so it is called directly — no HTTP round trip is required. The
// resulting cost_estimate is read back from a real in-memory SQLite DB via a
// real *usage.Logger, matching the project's "no storage mocks" convention.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// openCostFormulaDB opens an isolated in-memory SQLite DB with all migrations
// applied, uniquely named per call so parallel subtests never collide.
func openCostFormulaDB(t *testing.T) *db.DB {
	t.Helper()
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             fmt.Sprintf("file:costformula_%d?mode=memory&cache=private", time.Now().UnixNano()),
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{},
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("db.RunMigrations: %v", err)
	}
	return d
}

// loggedCostEstimate calls logUsageEvent directly with the given model
// pricing and usage figures, flushes the logger synchronously, and returns
// the cost_estimate column written to usage_events (nil when the column is
// SQL NULL).
func loggedCostEstimate(t *testing.T, pricing config.PricingConfig, ui UsageInfo) *float64 {
	t.Helper()

	d := openCostFormulaDB(t)
	usageCfg := config.UsageConfig{BufferSize: 8, FlushInterval: time.Hour}
	ul := usage.NewLogger(d, usageCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()

	h := NewProxyHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul

	keyInfo := &auth.KeyInfo{ID: "cost-key", KeyType: "user_key", OrgID: "cost-org"}
	model := Model{Name: "cost-model", Pricing: pricing}

	h.logUsageEvent(keyInfo, model, ui, 100, nil, 200, "req-cost", model.Name)

	// Stop() drains and flushes synchronously before returning.
	ul.Stop()

	var cost sql.NullFloat64
	row := d.SQL().QueryRowContext(context.Background(),
		"SELECT cost_estimate FROM usage_events LIMIT 1")
	if err := row.Scan(&cost); err != nil {
		t.Fatalf("scan cost_estimate: %v", err)
	}
	if !cost.Valid {
		return nil
	}
	c := cost.Float64
	return &c
}

// TestLogUsageEvent_CostFormula is table-driven over the four-term cost
// formula in logUsageEvent.
func TestLogUsageEvent_CostFormula(t *testing.T) {
	t.Parallel()

	const tolerance = 1e-9

	tests := []struct {
		name     string
		pricing  config.PricingConfig
		ui       UsageInfo
		wantNil  bool
		wantCost float64
	}{
		{
			name: "fresh, cached-read, and cache-write each priced at their own configured rate",
			pricing: config.PricingConfig{
				InputPer1M:       10,
				OutputPer1M:      30,
				CachedInputPer1M: 2,
				CacheWritePer1M:  15,
			},
			ui: UsageInfo{
				PromptTokens:     1_000_000, // fresh(500k) + cachedRead(300k) + cacheWrite(200k)
				CachedReadTokens: 300_000,
				CacheWriteTokens: 200_000,
				CompletionTokens: 100_000,
				TotalTokens:      1_100_000,
			},
			// 0.5*10 + 0.3*2 + 0.2*15 + 0.1*30 = 5 + 0.6 + 3 + 3 = 11.6
			wantCost: 11.6,
		},
		{
			name: "unset cached-read rate falls back to the base input rate",
			pricing: config.PricingConfig{
				InputPer1M:      10,
				OutputPer1M:     0,
				CacheWritePer1M: 15,
				// CachedInputPer1M intentionally left unset (0).
			},
			ui: UsageInfo{
				PromptTokens:     1_000_000, // fresh(700k) + cachedRead(300k)
				CachedReadTokens: 300_000,
				CompletionTokens: 0,
				TotalTokens:      1_000_000,
			},
			// 0.7*10 + 0.3*10(fallback) = 7 + 3 = 10.
			// If the fallback did not apply, cached tokens would price at 0
			// and the result would be 7 instead.
			wantCost: 10,
		},
		{
			name: "unset cache-write rate falls back to the base input rate",
			pricing: config.PricingConfig{
				InputPer1M:       10,
				OutputPer1M:      0,
				CachedInputPer1M: 2,
				// CacheWritePer1M intentionally left unset (0).
			},
			ui: UsageInfo{
				PromptTokens:     1_000_000, // fresh(700k) + cacheWrite(300k)
				CacheWriteTokens: 300_000,
				CompletionTokens: 0,
				TotalTokens:      1_000_000,
			},
			// 0.7*10 + 0.3*10(fallback) = 7 + 3 = 10.
			wantCost: 10,
		},
		{
			name: "all four prices zero yields a nil cost",
			pricing: config.PricingConfig{
				InputPer1M:       0,
				OutputPer1M:      0,
				CachedInputPer1M: 0,
				CacheWritePer1M:  0,
			},
			ui: UsageInfo{
				PromptTokens:     1_000_000,
				CachedReadTokens: 300_000,
				CacheWriteTokens: 200_000,
				CompletionTokens: 100_000,
				TotalTokens:      1_100_000,
			},
			wantNil: true,
		},
		{
			name: "cache buckets summing to more than PromptTokens clamp fresh at zero",
			pricing: config.PricingConfig{
				InputPer1M:       10,
				OutputPer1M:      0,
				CachedInputPer1M: 5,
				CacheWritePer1M:  8,
			},
			ui: UsageInfo{
				PromptTokens:     100,
				CachedReadTokens: 80,
				CacheWriteTokens: 50, // 80 + 50 = 130 > PromptTokens(100)
				CompletionTokens: 0,
				TotalTokens:      100,
			},
			// fresh = max(100-80-50, 0) = 0.
			// (80/1e6)*5 + (50/1e6)*8 = 0.0004 + 0.0004 = 0.0008.
			// A naive (unclamped) fresh of -30 would instead subtract
			// (30/1e6)*10 = 0.0003, producing 0.0005 — the clamp is what
			// this test actually distinguishes.
			wantCost: 0.0008,
		},
		{
			name: "only the base input/output rates configured (no cache activity) matches the pre-#179 formula",
			pricing: config.PricingConfig{
				InputPer1M:  10,
				OutputPer1M: 30,
			},
			ui: UsageInfo{
				PromptTokens:     1_000_000,
				CompletionTokens: 500_000,
				TotalTokens:      1_500_000,
			},
			// 1*10 + 0.5*30 = 10 + 15 = 25.
			wantCost: 25,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := loggedCostEstimate(t, tc.pricing, tc.ui)

			if tc.wantNil {
				if got != nil {
					t.Fatalf("cost_estimate = %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("cost_estimate = nil, want non-nil")
			}
			if math.Abs(*got-tc.wantCost) > tolerance {
				t.Errorf("cost_estimate = %v, want %v", *got, tc.wantCost)
			}
		})
	}
}

// TestLogUsageEvent_NilKeyInfoNoOp verifies that logUsageEvent is a safe no-op
// when keyInfo is nil (unauthenticated request / auth middleware not wired),
// so it never dereferences a nil pointer or writes a spurious usage row.
func TestLogUsageEvent_NilKeyInfoNoOp(t *testing.T) {
	t.Parallel()

	d := openCostFormulaDB(t)
	usageCfg := config.UsageConfig{BufferSize: 8, FlushInterval: time.Hour}
	ul := usage.NewLogger(d, usageCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()

	h := NewProxyHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul

	model := Model{Name: "cost-model", Pricing: config.PricingConfig{InputPer1M: 10, OutputPer1M: 30}}
	h.logUsageEvent(nil, model, UsageInfo{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 10, nil, 200, "req-nil", model.Name)

	ul.Stop()

	var n int
	if err := d.SQL().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_events").Scan(&n); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if n != 0 {
		t.Errorf("usage_events row count = %d, want 0 (nil keyInfo must be a no-op)", n)
	}
}
