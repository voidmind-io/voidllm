package health_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// newRegistry builds a one-model Registry pointing at the supplied base URL.
// It uses config.ModelConfig so the full NewRegistry path is exercised.
func newRegistry(t *testing.T, baseURL string) *proxy.Registry {
	t.Helper()
	reg, err := proxy.NewRegistry([]config.ModelConfig{
		{
			Name:     "test-model",
			Provider: "openai",
			BaseURL:  baseURL,
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// newRegistryWithConfig builds a one-model Registry from mc, defaulting Name
// to "test-model" when unset. Unlike newRegistry (hardcoded to provider
// "openai" with no deployments), this lets tests set Provider, Type, and the
// Azure/GCP fields needed for provider-specific probe-applicability tests.
func newRegistryWithConfig(t *testing.T, mc config.ModelConfig) *proxy.Registry {
	t.Helper()
	if mc.Name == "" {
		mc.Name = "test-model"
	}
	reg, err := proxy.NewRegistry([]config.ModelConfig{mc})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// newLogger returns a discard slog.Logger so probe debug lines don't clutter
// test output.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// boolPtr is a small helper so tests can write boolPtr(true) instead of &v.
func boolPtr(v bool) *bool { return &v }

// cfg constructs a HealthCheckConfig with only the requested level enabled and
// a long tick interval so the ticker never fires during the test.
func cfg(health, models, functional bool) config.HealthCheckConfig {
	const neverTick = 24 * time.Hour
	return config.HealthCheckConfig{
		Health:     config.HealthProbeConfig{Enabled: health, Interval: neverTick},
		Models:     config.HealthProbeConfig{Enabled: models, Interval: neverTick},
		Functional: config.HealthProbeConfig{Enabled: functional, Interval: neverTick},
	}
}

// TestProbeHealth_Success verifies that a 200 response from the upstream marks
// HealthOK=true and status=healthy.
func TestProbeHealth_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.HealthOK == nil || !*mh.HealthOK {
		t.Errorf("HealthOK = %v, want true", mh.HealthOK)
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "healthy")
	}
}

// TestProbeHealth_AnyHTTPResponseIsHealthy verifies that any HTTP response —
// including 500 — is treated as healthy by the health probe, because receiving
// a response means the server is reachable. Only connection errors are unhealthy.
func TestProbeHealth_AnyHTTPResponseIsHealthy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	// A 500 response still means the server is reachable — health probe passes.
	if mh.HealthOK == nil || !*mh.HealthOK {
		t.Errorf("HealthOK = %v, want true (any HTTP response = reachable)", mh.HealthOK)
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "healthy")
	}
}

// TestProbeHealth_Unreachable verifies that an unreachable URL marks
// HealthOK=false and status=unhealthy.
func TestProbeHealth_Unreachable(t *testing.T) {
	t.Parallel()

	// Use a URL that refuses connections immediately.
	reg := newRegistry(t, "http://127.0.0.1:1")
	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.HealthOK == nil || *mh.HealthOK {
		t.Errorf("HealthOK = %v, want false", mh.HealthOK)
	}
	if mh.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "unhealthy")
	}
}

// TestProbeModels_Success verifies that a 200 from GET /models marks
// ModelsOK=true and status=healthy.
func TestProbeModels_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(false, true, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.ModelsOK == nil || !*mh.ModelsOK {
		t.Errorf("ModelsOK = %v, want true", mh.ModelsOK)
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "healthy")
	}
}

// TestProbeModels_AuthFailure verifies that a 401 from GET /models marks
// ModelsOK=false and status=degraded (health probe is not run, so the
// upstream is still considered reachable).
func TestProbeModels_AuthFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	// Only the models probe is enabled; the health probe is off, so HealthOK
	// stays nil. A failing models probe should produce "degraded", not "unhealthy".
	c := health.NewChecker(reg, cfg(false, true, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.ModelsOK == nil || *mh.ModelsOK {
		t.Errorf("ModelsOK = %v, want false", mh.ModelsOK)
	}
	if mh.HealthOK != nil {
		t.Errorf("HealthOK = %v, want nil (probe disabled)", mh.HealthOK)
	}
	if mh.Status != "degraded" {
		t.Errorf("Status = %q, want %q", mh.Status, "degraded")
	}
}

// TestProbeFunctional_Success verifies that a 200 from POST /chat/completions
// marks FunctionalOK=true and status=healthy.
func TestProbeFunctional_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(false, false, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.FunctionalOK == nil || !*mh.FunctionalOK {
		t.Errorf("FunctionalOK = %v, want true", mh.FunctionalOK)
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "healthy")
	}
}

// TestProbeFunctional_Failure verifies that a 500 from POST /chat/completions
// marks FunctionalOK=false and status=degraded.
func TestProbeFunctional_Failure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(false, false, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.FunctionalOK == nil || *mh.FunctionalOK {
		t.Errorf("FunctionalOK = %v, want false", mh.FunctionalOK)
	}
	if mh.Status != "degraded" {
		t.Errorf("Status = %q, want %q", mh.Status, "degraded")
	}
}

// TestDeriveStatus_AllHealthy exercises the Checker with all three probes
// enabled against a server that succeeds everywhere, expecting status=healthy.
func TestDeriveStatus_AllHealthy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	allEnabled := config.HealthCheckConfig{
		Health:     config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
		Models:     config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
		Functional: config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
	}
	c := health.NewChecker(reg, allEnabled, newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "healthy")
	}
	if mh.HealthOK == nil || !*mh.HealthOK {
		t.Errorf("HealthOK = %v, want true", mh.HealthOK)
	}
	if mh.ModelsOK == nil || !*mh.ModelsOK {
		t.Errorf("ModelsOK = %v, want true", mh.ModelsOK)
	}
	if mh.FunctionalOK == nil || !*mh.FunctionalOK {
		t.Errorf("FunctionalOK = %v, want true", mh.FunctionalOK)
	}
}

// TestDeriveStatus_HealthFailed verifies that when the health probe cannot
// reach the server (connection refused) the status is unhealthy regardless of
// other probe results. The functional probe is configured but never fires
// because both probe levels use the same (unreachable) base URL.
func TestDeriveStatus_HealthFailed(t *testing.T) {
	t.Parallel()

	// Port 1 refuses connections immediately — this triggers a connection-level
	// error, which is the only way to make the health probe report HealthOK=false
	// after Fix 2 (any HTTP response = reachable).
	reg := newRegistry(t, "http://127.0.0.1:1")
	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.HealthOK == nil || *mh.HealthOK {
		t.Errorf("HealthOK = %v, want false", mh.HealthOK)
	}
	if mh.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", mh.Status, "unhealthy")
	}
}

// TestDeriveStatus_Degraded verifies that when health passes but the models
// probe fails the status is degraded.
func TestDeriveStatus_Degraded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, cfg(true, true, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.HealthOK == nil || !*mh.HealthOK {
		t.Errorf("HealthOK = %v, want true", mh.HealthOK)
	}
	if mh.ModelsOK == nil || *mh.ModelsOK {
		t.Errorf("ModelsOK = %v, want false", mh.ModelsOK)
	}
	if mh.Status != "degraded" {
		t.Errorf("Status = %q, want %q", mh.Status, "degraded")
	}
}

// TestDeriveStatus_Unknown verifies that when no probe has been run for a
// model, GetHealth returns false (no result stored yet).
func TestDeriveStatus_Unknown(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	// All probes disabled — Start() runs no initial probe cycle.
	noneEnabled := config.HealthCheckConfig{}
	c := health.NewChecker(reg, noneEnabled, newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	_, ok := c.GetHealth("test-model")
	if ok {
		t.Error("GetHealth returned true; expected false because no probe was run")
	}
}

// TestStartStop verifies that calling Start then immediately calling the
// returned stop function does not panic or deadlock.
func TestStartStop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	allEnabled := config.HealthCheckConfig{
		Health:     config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
		Models:     config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
		Functional: config.HealthProbeConfig{Enabled: true, Interval: 24 * time.Hour},
	}
	c := health.NewChecker(reg, allEnabled, newLogger())

	stop := c.Start()
	stop() // must not panic, race, or deadlock
}

// TestGetAllHealth verifies that GetAllHealth returns results for every
// model that has been probed at least once.
func TestGetAllHealth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg, err := proxy.NewRegistry([]config.ModelConfig{
		{Name: "alpha", Provider: "openai", BaseURL: srv.URL},
		{Name: "beta", Provider: "openai", BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	all := c.GetAllHealth()
	if len(all) != 2 {
		t.Fatalf("GetAllHealth returned %d results, want 2", len(all))
	}
	for _, mh := range all {
		if mh.Status != "healthy" {
			t.Errorf("model %q: Status = %q, want %q", mh.ModelName, mh.Status, "healthy")
		}
	}
}

// TestGetAllHealth_NoneProbed verifies that GetAllHealth returns an empty slice
// when no probe has run yet.
func TestGetAllHealth_NoneProbed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistry(t, srv.URL)
	c := health.NewChecker(reg, config.HealthCheckConfig{}, newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	all := c.GetAllHealth()
	if len(all) != 0 {
		t.Errorf("GetAllHealth = %d entries, want 0", len(all))
	}
}

// TestSanitizeError_ConnectionRefused verifies that the sanitizer maps a
// connection-refused error (which contains the upstream IP) to a safe string.
func TestSanitizeError_ConnectionRefused(t *testing.T) {
	t.Parallel()

	// A connection-refused error will contain "connection refused" and the
	// URL — the sanitized output must not leak the URL.
	reg := newRegistry(t, "http://127.0.0.1:1")
	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe did not run")
	}
	if mh.LastError != "connection refused" {
		t.Errorf("LastError = %q, want %q", mh.LastError, "connection refused")
	}
}

// TestChecker_ModelsProbe_NotApplicable_LeavesNilNotDegraded verifies that
// when the models-list probe has no meaningful equivalent for the target's
// provider (azure, vertex, gemini), the Checker leaves ModelsOK nil and
// derives status "unknown" rather than contacting the upstream at all or
// recording a false failure.
func TestChecker_ModelsProbe_NotApplicable_LeavesNilNotDegraded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
	}{
		{name: "azure", provider: "azure"},
		{name: "vertex", provider: "vertex"},
		{name: "gemini", provider: "gemini"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			reg := newRegistryWithConfig(t, config.ModelConfig{
				Provider:        tc.provider,
				BaseURL:         srv.URL,
				AzureDeployment: "dep",
				GCPProject:      "proj",
				GCPLocation:     "us-central1",
			})
			// Only the models probe is enabled.
			c := health.NewChecker(reg, cfg(false, true, false), newLogger())
			stop := c.Start()
			t.Cleanup(stop)

			mh, ok := c.GetHealth("test-model")
			if !ok {
				t.Fatal("GetHealth returned false; probe cycle did not run")
			}
			if mh.ModelsOK != nil {
				t.Errorf("ModelsOK = %v, want nil (not applicable for provider %s)", mh.ModelsOK, tc.provider)
			}
			if mh.Status != "unknown" {
				t.Errorf("Status = %q, want %q (not-applicable must not degrade)", mh.Status, "unknown")
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("upstream received %d requests, want 0 (models-list is not applicable for %s)", got, tc.provider)
			}
		})
	}
}

// TestChecker_EmbeddingsProbe_NotApplicable_LeavesNilNotDegraded verifies
// that a functional probe against an embedding model whose provider has no
// OpenAI-compatible embeddings endpoint (anthropic, gemini, vertex) leaves
// FunctionalOK nil and derives status "unknown".
func TestChecker_EmbeddingsProbe_NotApplicable_LeavesNilNotDegraded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
	}{
		{name: "anthropic", provider: "anthropic"},
		{name: "gemini", provider: "gemini"},
		{name: "vertex", provider: "vertex"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			reg := newRegistryWithConfig(t, config.ModelConfig{
				Provider:    tc.provider,
				BaseURL:     srv.URL,
				Type:        "embedding",
				GCPProject:  "proj",
				GCPLocation: "us-central1",
			})
			c := health.NewChecker(reg, cfg(false, false, true), newLogger())
			stop := c.Start()
			t.Cleanup(stop)

			mh, ok := c.GetHealth("test-model")
			if !ok {
				t.Fatal("GetHealth returned false; probe cycle did not run")
			}
			if mh.FunctionalOK != nil {
				t.Errorf("FunctionalOK = %v, want nil (embeddings not applicable for provider %s)", mh.FunctionalOK, tc.provider)
			}
			if mh.Status != "unknown" {
				t.Errorf("Status = %q, want %q (not-applicable must not degrade)", mh.Status, "unknown")
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("upstream received %d requests, want 0 (embeddings is not applicable for %s)", got, tc.provider)
			}
		})
	}
}

// TestChecker_FunctionalProbe_ModelTypeSkip_LeavesNilNotSuccess verifies
// that model types with no meaningful functional probe (reranking, image,
// audio_transcription, tts) leave FunctionalOK nil rather than recording a
// success that never actually ran. This is a behaviour change: previously
// these types were skipped by recording an implicit success.
func TestChecker_FunctionalProbe_ModelTypeSkip_LeavesNilNotSuccess(t *testing.T) {
	t.Parallel()

	types := []string{"reranking", "image", "audio_transcription", "tts"}

	for _, mt := range types {
		t.Run(mt, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			reg := newRegistryWithConfig(t, config.ModelConfig{
				Provider: "openai",
				BaseURL:  srv.URL,
				Type:     mt,
			})
			c := health.NewChecker(reg, cfg(false, false, true), newLogger())
			stop := c.Start()
			t.Cleanup(stop)

			mh, ok := c.GetHealth("test-model")
			if !ok {
				t.Fatal("GetHealth returned false; probe cycle did not run")
			}
			if mh.FunctionalOK != nil {
				t.Errorf("FunctionalOK = %v, want nil (model type %q is skipped)", mh.FunctionalOK, mt)
			}
			if mh.Status != "unknown" {
				t.Errorf("Status = %q, want %q (a skipped probe must not be recorded as healthy)", mh.Status, "unknown")
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("upstream received %d requests, want 0 (type %q must be skipped before any request is sent)", got, mt)
			}
		})
	}
}

// TestChecker_NotApplicableProbe_DoesNotBlockGenuineDegradation drives a
// combined scenario through the real Checker: the models probe is not
// applicable for azure (stays nil, "not checked"), while the functional
// probe is applicable and genuinely fails. The not-applicable probe must
// not mask the genuine failure — status must still be "degraded".
func TestChecker_NotApplicableProbe_DoesNotBlockGenuineDegradation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Azure's chat probe path always contains "/chat/completions"; fail
		// it so the functional probe genuinely degrades the status.
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistryWithConfig(t, config.ModelConfig{
		Provider:        "azure",
		BaseURL:         srv.URL,
		AzureDeployment: "dep",
	})
	// Models probe (not applicable for azure, stays nil) and functional
	// probe (applicable, fails) are both enabled; health is off.
	c := health.NewChecker(reg, cfg(false, true, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("GetHealth returned false; probe cycle did not run")
	}
	if mh.ModelsOK != nil {
		t.Errorf("ModelsOK = %v, want nil (not applicable for azure)", mh.ModelsOK)
	}
	if mh.FunctionalOK == nil || *mh.FunctionalOK {
		t.Errorf("FunctionalOK = %v, want false (upstream returned 500)", mh.FunctionalOK)
	}
	if mh.Status != "degraded" {
		t.Errorf("Status = %q, want %q — a not-applicable probe must not prevent a genuine failure from degrading status", mh.Status, "degraded")
	}
}

// TestChecker_MultiDeployment_KeepsPerDeploymentKey verifies that a
// multi-deployment model produces one health result per deployment, each
// keyed as "modelName/deploymentName".
func TestChecker_MultiDeployment_KeepsPerDeploymentKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg, err := proxy.NewRegistry([]config.ModelConfig{
		{
			Name:     "multi-model",
			Strategy: "round-robin",
			Deployments: []config.DeploymentConfig{
				{Name: "dep-a", Provider: "openai", BaseURL: srv.URL},
				{Name: "dep-b", Provider: "openai", BaseURL: srv.URL},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	c := health.NewChecker(reg, cfg(true, false, false), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	for _, key := range []string{"multi-model/dep-a", "multi-model/dep-b"} {
		mh, ok := c.GetHealth(key)
		if !ok {
			t.Fatalf("GetHealth(%q) returned false; probe did not run", key)
		}
		if mh.HealthOK == nil || !*mh.HealthOK {
			t.Errorf("key %q: HealthOK = %v, want true", key, mh.HealthOK)
		}
	}

	all := c.GetAllHealth()
	if len(all) != 2 {
		t.Fatalf("GetAllHealth() len = %d, want 2", len(all))
	}
}
