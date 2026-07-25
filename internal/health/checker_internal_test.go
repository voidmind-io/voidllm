package health

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// newDiscardLogger returns a discard slog.Logger so probe debug lines don't
// clutter test output. It duplicates checker_test.go's newLogger because this
// file lives in package health (white-box) rather than health_test, and the
// two test binaries do not share unexported helpers.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDeriveLastError_Precedence verifies deriveLastError's priority order —
// health error outranks models error outranks functional error — matching
// deriveStatus's precedence, and that it returns empty when nothing is set.
func TestDeriveLastError_Precedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		h    *ModelHealth
		want string
	}{
		{
			name: "health error wins over models and functional",
			h: &ModelHealth{
				HealthError:     "health failed",
				ModelsError:     "models failed",
				FunctionalError: "functional failed",
			},
			want: "health failed",
		},
		{
			name: "models error wins over functional when health is empty",
			h: &ModelHealth{
				ModelsError:     "models failed",
				FunctionalError: "functional failed",
			},
			want: "models failed",
		},
		{
			name: "functional error surfaces when health and models are empty",
			h: &ModelHealth{
				FunctionalError: "functional failed",
			},
			want: "functional failed",
		},
		{
			name: "empty when nothing is set",
			h:    &ModelHealth{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := deriveLastError(tc.h)
			if got != tc.want {
				t.Errorf("deriveLastError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunOne_NotApplicable_ClearsOnlyOwnError drives the Checker's unexported
// runOne directly (rather than through the registry/Start path) so it can
// express a provider transition for the *same* probe-target key within a
// single test — the registry has no live-update path that would let a
// black-box test change a target's provider between two probe cycles for the
// same key, but runOne itself has no such restriction: it is keyed purely by
// probeTarget.key, exactly as it would be if a model were edited from one
// provider to another between health-check cycles.
//
// It records genuine failures on both the models and functional levels, then
// re-probes the models level with a target whose provider makes the
// models-list probe not-applicable (azure). It asserts that only the models
// level's error is cleared and that the functional level's independently
// recorded failure is left untouched.
func TestRunOne_NotApplicable_ClearsOnlyOwnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	reg, err := proxy.NewRegistry(nil)
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}
	c := NewChecker(reg, config.HealthCheckConfig{}, newDiscardLogger())

	const key = "transition-model"
	openaiTarget := probeTarget{
		key:       key,
		modelName: key,
		provider:  "openai",
		baseURL:   srv.URL,
		modelType: "chat",
	}

	// Record genuine failures on both levels: the server returns 500 for
	// every path, so both the models-list and functional-chat probes fail.
	c.runOne(openaiTarget, levelModels)
	c.runOne(openaiTarget, levelFunctional)

	mh, ok := c.GetHealth(key)
	if !ok {
		t.Fatal("GetHealth returned false after the initial probes")
	}
	if mh.ModelsError == "" {
		t.Fatal("precondition failed: ModelsError must be non-empty before the transition")
	}
	if mh.FunctionalError == "" {
		t.Fatal("precondition failed: FunctionalError must be non-empty before the transition")
	}
	wantFunctionalOK := mh.FunctionalOK
	wantFunctionalError := mh.FunctionalError

	// Simulate the model being edited from openai to azure: the models-list
	// probe has no meaningful equivalent for azure and becomes not-applicable
	// for this same-keyed target on the next cycle.
	azureTarget := openaiTarget
	azureTarget.provider = "azure"
	c.runOne(azureTarget, levelModels)

	mh, ok = c.GetHealth(key)
	if !ok {
		t.Fatal("GetHealth returned false after the transition")
	}
	if mh.ModelsOK != nil {
		t.Errorf("ModelsOK = %v, want nil once the probe becomes not-applicable", mh.ModelsOK)
	}
	if mh.ModelsError != "" {
		t.Errorf("ModelsError = %q, want empty once the probe becomes not-applicable (must not linger)", mh.ModelsError)
	}
	if (mh.FunctionalOK == nil) != (wantFunctionalOK == nil) || (mh.FunctionalOK != nil && *mh.FunctionalOK != *wantFunctionalOK) {
		t.Errorf("FunctionalOK = %v, want %v (must be undisturbed by the models-level transition)", mh.FunctionalOK, wantFunctionalOK)
	}
	if mh.FunctionalError != wantFunctionalError {
		t.Errorf("FunctionalError = %q, want %q (must be unchanged by the models-level transition)", mh.FunctionalError, wantFunctionalError)
	}
}

// TestRunOne_ConcurrentLevels_NoLostUpdate drives all three probe levels for
// the SAME key concurrently, many times, and asserts that every level's
// result survives in the final stored ModelHealth.
//
// runOne's sequence — Load the existing snapshot, copy it, mutate only the
// calling level's field, then Store — is safe against concurrent mutation of
// the same struct (copy-on-write), but without Checker.mu it is not safe
// against a lost update: two levels can both Load the same old snapshot
// before either has Stored, and whichever Store lands second silently
// discards the field the other level just wrote, because it was built from a
// copy that predates that write. That is a lost update, not a data race, so
// -race alone cannot catch it — only asserting on the resulting state can.
//
// A single httptest.Server gives each level a distinct, deterministic
// outcome by request path: the health probe hits the server root and always
// succeeds (any response counts as reachable), "/models" returns 500 so the
// models-list probe fails, and "/chat/completions" returns 200 so the
// functional probe succeeds. That lets the assertions check not merely that
// each field is non-nil but that it holds the *correct* per-level outcome —
// ruling out a bug where one level's result leaks into another's field, not
// just a bug where a field goes missing.
func TestRunOne_ConcurrentLevels_NoLostUpdate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg, err := proxy.NewRegistry(nil)
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}
	c := NewChecker(reg, config.HealthCheckConfig{}, newDiscardLogger())

	const iterations = 300
	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("concurrent-model-%d", i)
		target := probeTarget{
			key:       key,
			modelName: key,
			provider:  "openai",
			baseURL:   srv.URL,
			modelType: "chat",
		}

		// Start all three levels as close to simultaneously as possible so
		// their runOne calls race to update the same key.
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, lvl := range []probeLevel{levelHealth, levelModels, levelFunctional} {
			wg.Add(1)
			go func(lvl probeLevel) {
				defer wg.Done()
				<-start
				c.runOne(target, lvl)
			}(lvl)
		}
		close(start)
		wg.Wait()

		mh, ok := c.GetHealth(key)
		if !ok {
			t.Fatalf("iteration %d: GetHealth(%q) returned false after all three levels ran", i, key)
		}
		if mh.HealthOK == nil || !*mh.HealthOK {
			t.Errorf("iteration %d: HealthOK = %v, want non-nil true (lost update clobbered the health level's result)", i, mh.HealthOK)
		}
		if mh.ModelsOK == nil || *mh.ModelsOK {
			t.Errorf("iteration %d: ModelsOK = %v, want non-nil false (lost update clobbered the models level's result)", i, mh.ModelsOK)
		}
		if mh.FunctionalOK == nil || !*mh.FunctionalOK {
			t.Errorf("iteration %d: FunctionalOK = %v, want non-nil true (lost update clobbered the functional level's result)", i, mh.FunctionalOK)
		}
	}
}
