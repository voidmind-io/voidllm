package health

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
