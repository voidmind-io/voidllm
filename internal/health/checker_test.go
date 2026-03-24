package health_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
