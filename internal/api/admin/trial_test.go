package admin_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/licensetest"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func setupTrialApp(t *testing.T, dsn string) (*fiber.App, *admin.Handler, *cache.Cache[string, auth.KeyInfo], *lockedBuffer) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver: "sqlite", DSN: dsn, MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	logs := &lockedBuffer{}
	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", false)),
		Log:        slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, handler, keyCache, logs
}

func postTrial(t *testing.T, app *fiber.App, token string, body any) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/license/trial", bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func validTrialBody() map[string]any {
	return map[string]any{
		"email":             "secret.person@example.com",
		"company":           "Secret Co",
		"consent_trial":     true,
		"consent_marketing": false,
	}
}

func TestRequestTrial_RoleGate(t *testing.T) {
	app, h, keyCache, _ := setupTrialApp(t, "file:TestRequestTrial_RoleGate?mode=memory&cache=private")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	defer srv.Close()
	admin.SetTrialEndpoint(h, srv.URL, nil)

	tests := []struct {
		role string
		want int
	}{
		{auth.RoleMember, 403},
		{auth.RoleTeamAdmin, 403},
		{auth.RoleOrgAdmin, 403},
		{auth.RoleSystemAdmin, 202},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			token := addTestKey(t, keyCache, tt.role, "org-trial")
			if got, _ := postTrial(t, app, token, validTrialBody()); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
	t.Run("unauthenticated", func(t *testing.T) {
		if got, _ := postTrial(t, app, "", validTrialBody()); got != 401 {
			t.Errorf("status = %d, want 401", got)
		}
	})
}

func TestRequestTrial_Validation(t *testing.T) {
	app, h, keyCache, _ := setupTrialApp(t, "file:TestRequestTrial_Validation?mode=memory&cache=private")
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(202)
	}))
	defer srv.Close()
	admin.SetTrialEndpoint(h, srv.URL, nil)
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-trial")

	with := func(k string, v any) map[string]any {
		b := validTrialBody()
		b[k] = v
		return b
	}
	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"valid", validTrialBody(), 202},
		{"consent_trial false", with("consent_trial", false), 400},
		{"consent_trial missing", map[string]any{"email": "a@b.co"}, 400},
		{"empty email", with("email", ""), 400},
		{"bad email", with("email", "not-an-email"), 400},
		{"display name email", with("email", "Bob <bob@example.com>"), 400},
		{"email too long", with("email", strings.Repeat("a", 250)+"@b.co"), 400},
		{"company 200 ok", with("company", strings.Repeat("c", 200)), 202},
		{"company 201 too long", with("company", strings.Repeat("c", 201)), 400},
		{"company 100 astral runes = 200 utf16 units ok", with("company", strings.Repeat("\U0001F600", 100)), 202},
		{"company 101 astral runes = 202 utf16 units too long", with("company", strings.Repeat("\U0001F600", 101)), 400},
		{"no company ok", map[string]any{"email": "a@b.co", "consent_trial": true}, 202},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			before := hits
			mu.Unlock()
			got, _ := postTrial(t, app, token, tt.body)
			if got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			mu.Lock()
			called := hits > before
			mu.Unlock()
			if called != (tt.want == 202) {
				t.Errorf("upstream called = %v for status %d", called, tt.want)
			}
		})
	}

	t.Run("invalid json", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/license/trial", strings.NewReader("{"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestRequestTrial_UpstreamMapping(t *testing.T) {
	const unavailable = "Could not reach the license server. You can request a trial at https://voidllm.ai/trial"
	tests := []struct {
		name       string
		upstream   func(http.ResponseWriter, *http.Request)
		client     *http.Client
		wantStatus int
		wantCode   string
		wantBody   map[string]any
	}{
		{
			name:       "202",
			upstream:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) },
			wantStatus: 202,
			wantBody:   map[string]any{"status": "check_inbox"},
		},
		{
			name:       "429",
			upstream:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
			wantStatus: 429,
			wantCode:   "rate_limited",
		},
		{
			name:       "500",
			upstream:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
			wantStatus: 502,
			wantCode:   "trial_unavailable",
		},
		{
			name:       "400 from upstream",
			upstream:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) },
			wantStatus: 400,
			wantCode:   "trial_rejected",
		},
		{
			name: "timeout",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(2 * time.Second):
				case <-r.Context().Done():
				}
			},
			client:     &http.Client{Timeout: 100 * time.Millisecond},
			wantStatus: 502,
			wantCode:   "trial_unavailable",
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, h, keyCache, _ := setupTrialApp(t, "file:TestRequestTrial_UpstreamMapping"+string(rune('a'+i))+"?mode=memory&cache=private")
			srv := httptest.NewServer(http.HandlerFunc(tt.upstream))
			defer srv.Close()
			admin.SetTrialEndpoint(h, srv.URL, tt.client)
			token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-trial")

			status, body := postTrial(t, app, token, validTrialBody())
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %v)", status, tt.wantStatus, body)
			}
			if tt.wantBody != nil && body["status"] != tt.wantBody["status"] {
				t.Errorf("body = %v, want %v", body, tt.wantBody)
			}
			if tt.wantStatus == 502 {
				e, _ := body["error"].(map[string]any)
				if e["code"] != tt.wantCode || e["message"] != unavailable {
					t.Errorf("error = %v, want code %q and fixed message", e, tt.wantCode)
				}
			}
			if tt.wantStatus == 400 {
				e, _ := body["error"].(map[string]any)
				if e["code"] != tt.wantCode || e["message"] != "The license server rejected the request. Check the email address." {
					t.Errorf("error = %v, want code %q and fixed message", e, tt.wantCode)
				}
			}
			if tt.wantStatus == 429 {
				e, _ := body["error"].(map[string]any)
				if e["code"] != tt.wantCode {
					t.Errorf("error = %v, want code %q", e, tt.wantCode)
				}
			}
		})
	}
}

// TestRequestTrial_RedirectNotFollowed fails if the client follows redirects:
// the redirect target would be hit.
func TestRequestTrial_RedirectNotFollowed(t *testing.T) {
	app, h, keyCache, _ := setupTrialApp(t, "file:TestRequestTrial_Redirect?mode=memory&cache=private")
	var targetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(license.TrialRequestPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(202)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	admin.SetTrialEndpoint(h, srv.URL, nil)
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-trial")

	status, _ := postTrial(t, app, token, validTrialBody())
	if status != 502 {
		t.Errorf("status = %d, want 502", status)
	}
	if got := targetHits.Load(); got != 0 {
		t.Errorf("redirect target hit %d times, want 0 (redirects must not be followed)", got)
	}
}

func TestRequestTrial_UpstreamPayloadAndLogs(t *testing.T) {
	app, h, keyCache, logs := setupTrialApp(t, "file:TestRequestTrial_Payload?mode=memory&cache=private")

	var gotBody []byte
	var gotPath, gotMethod string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotPath, gotMethod = b, r.URL.Path, r.Method
		mu.Unlock()
		w.WriteHeader(202)
	}))
	defer srv.Close()
	admin.SetTrialEndpoint(h, srv.URL, nil)
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-trial")

	body := validTrialBody()
	body["consent_marketing"] = true
	body["unexpected_extra"] = "must not be forwarded"
	if status, _ := postTrial(t, app, token, body); status != 202 {
		t.Fatalf("status = %d, want 202", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "POST" || gotPath != license.TrialRequestPath {
		t.Errorf("upstream %s %s, want POST %s", gotMethod, gotPath, license.TrialRequestPath)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	wantKeys := []string{"email", "company", "consent_trial", "consent_marketing",
		"consent_text_version", "source", "instance_id", "product_version"}
	if len(sent) != len(wantKeys) {
		t.Errorf("upstream body has %d fields, want %d: %v", len(sent), len(wantKeys), sent)
	}
	for _, k := range wantKeys {
		if _, ok := sent[k]; !ok {
			t.Errorf("upstream body missing %q", k)
		}
	}
	if sent["email"] != "secret.person@example.com" || sent["company"] != "Secret Co" ||
		sent["consent_trial"] != true || sent["consent_marketing"] != true ||
		sent["consent_text_version"] != "2026-11-20" || sent["source"] != "product" {
		t.Errorf("unexpected upstream values: %v", sent)
	}
	if id, _ := sent["instance_id"].(string); id == "" {
		t.Error("instance_id is empty")
	}

	out := logs.String()
	if !strings.Contains(out, "trial requested") {
		t.Errorf("expected 'trial requested' log, got %q", out)
	}
	for _, secret := range []string{"secret.person", "example.com", "Secret Co"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain PII %q: %s", secret, out)
		}
	}
}

func TestRequestTrial_FailureLogsNoPII(t *testing.T) {
	app, h, keyCache, logs := setupTrialApp(t, "file:TestRequestTrial_FailLogs?mode=memory&cache=private")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	srv.Close() // unreachable: connection refused
	admin.SetTrialEndpoint(h, srv.URL, nil)
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-trial")

	if status, _ := postTrial(t, app, token, validTrialBody()); status != 502 {
		t.Fatalf("status = %d, want 502", status)
	}
	out := logs.String()
	if !strings.Contains(out, "trial request failed") {
		t.Errorf("expected failure log, got %q", out)
	}
	for _, secret := range []string{"secret.person", "example.com", "Secret Co"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain PII %q: %s", secret, out)
		}
	}
}

func TestGetLicense_TrialAndExpiryFields(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)

	mk := func(trial bool, in time.Duration) license.License {
		claims := licensetest.NewEnterpriseClaims([]string{license.FeatureAuditLogs})
		claims.Trial = trial
		claims.ExpiresAt = jwtDate(time.Now().Add(in))
		lic, err := license.ValidateKey(licensetest.SignTestJWT(t, priv, claims))
		if err != nil {
			t.Fatalf("ValidateKey: %v", err)
		}
		return lic
	}

	tests := []struct {
		name        string
		lic         license.License
		state       *license.ExpiryState
		swapExpired bool
		wantTrial   bool
		wantDays    any
		wantExpired bool
		wantRestart bool
	}{
		{name: "community", lic: license.Verify("", false), wantDays: nil},
		{name: "paid 10d", lic: mk(false, 10*24*time.Hour+time.Hour), wantDays: float64(10)},
		{name: "trial 29d", lic: mk(true, 29*24*time.Hour+time.Hour), wantTrial: true, wantDays: float64(29)},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, h, keyCache, _ := setupTrialApp(t, "file:TestGetLicense_Fields"+string(rune('a'+i))+"?mode=memory&cache=private")
			h.License.Store(tt.lic)
			token := addTestKey(t, keyCache, auth.RoleMember, "org-x")
			body := getLicenseJSON(t, app, token)
			if body["trial"] != tt.wantTrial {
				t.Errorf("trial = %v, want %v", body["trial"], tt.wantTrial)
			}
			if body["days_left"] != tt.wantDays {
				t.Errorf("days_left = %v, want %v", body["days_left"], tt.wantDays)
			}
			if body["expired"] != false || body["restart_required"] != false {
				t.Errorf("expired/restart_required = %v/%v, want false/false", body["expired"], body["restart_required"])
			}
			if tt.wantDays != nil && body["expires_at"] == nil {
				t.Error("expires_at is null")
			}
		})
	}

	// A paid license that expired must not be reported as an ended trial.
	t.Run("expired paid license", func(t *testing.T) {
		app, h, keyCache, _ := setupTrialApp(t, "file:TestGetLicense_ExpiredPaid?mode=memory&cache=private")
		h.License.Store(mk(false, 1500*time.Millisecond))
		state := license.NewExpiryState(false)
		h.LicenseExpiry = state
		time.Sleep(2100 * time.Millisecond)
		if !license.CheckExpiry(context.Background(), h.License, license.ExpiryWatcherConfig{State: state}) {
			t.Fatal("CheckExpiry did not swap")
		}
		body := getLicenseJSON(t, app, addTestKey(t, keyCache, auth.RoleMember, "org-x"))
		if body["expired"] != true || body["expired_trial"] != false {
			t.Errorf("expired/expired_trial = %v/%v, want true/false", body["expired"], body["expired_trial"])
		}
	})

	// Expired at runtime: the watcher swapped the license to community.
	expiryTests := []struct {
		name        string
		gated       bool
		wantRestart bool
	}{
		{"expired with gated features", true, true},
		{"expired without gated features", false, false},
	}
	for i, tt := range expiryTests {
		t.Run(tt.name, func(t *testing.T) {
			app, h, keyCache, _ := setupTrialApp(t, "file:TestGetLicense_Expired"+string(rune('a'+i))+"?mode=memory&cache=private")
			// JWT expiry has one-second resolution, so wait past it.
			h.License.Store(mk(true, 1500*time.Millisecond))
			state := license.NewExpiryState(tt.gated)
			h.LicenseExpiry = state
			time.Sleep(2100 * time.Millisecond)
			if !license.CheckExpiry(context.Background(), h.License, license.ExpiryWatcherConfig{State: state}) {
				t.Fatal("CheckExpiry did not swap")
			}
			token := addTestKey(t, keyCache, auth.RoleMember, "org-x")
			body := getLicenseJSON(t, app, token)
			if body["expired"] != true || body["restart_required"] != tt.wantRestart {
				t.Errorf("expired/restart_required = %v/%v, want true/%v", body["expired"], body["restart_required"], tt.wantRestart)
			}
			if body["expired_trial"] != true {
				t.Errorf("expired_trial = %v, want true for an expired trial", body["expired_trial"])
			}
			if body["edition"] != "community" || body["days_left"] != float64(0) || body["expires_at"] == nil {
				t.Errorf("edition/days_left/expires_at = %v/%v/%v", body["edition"], body["days_left"], body["expires_at"])
			}
		})
	}
}

func getLicenseJSON(t *testing.T, app *fiber.App, token string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/license", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func jwtDate(t time.Time) *jwt.NumericDate { return jwt.NewNumericDate(t) }

func signedLicense(t *testing.T, priv ed25519.PrivateKey, customer string, mutate func(*license.LicenseClaims)) string {
	t.Helper()
	claims := licensetest.NewEnterpriseClaims([]string{license.FeatureAuditLogs})
	claims.CustomerID = customer
	if mutate != nil {
		mutate(&claims)
	}
	return licensetest.SignTestJWT(t, priv, claims)
}

func putLicense(t *testing.T, app *fiber.App, token, key string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/settings/license", bodyJSON(t, map[string]string{"key": key}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestSetLicense_ConcurrentActivationsStayConsistent verifies, under -race,
// that concurrent PUTs leave the holder, the stored key and the heartbeat key
// on the same request.
func TestSetLicense_ConcurrentActivationsStayConsistent(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)
	app, h, keyCache, _ := setupTrialApp(t, "file:TestSetLicense_Concurrent?mode=memory&cache=private")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-lic")

	keys := map[string]string{} // raw key -> customer id
	var ordered []string
	for i := 0; i < 6; i++ {
		cust := "cust-" + string(rune('a'+i))
		k := signedLicense(t, priv, cust, nil)
		keys[k] = cust
		ordered = append(ordered, k)
	}

	var wg sync.WaitGroup
	for _, k := range ordered {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, body := putLicense(t, app, token, k); status != 200 {
				t.Errorf("PUT status = %d: %v", status, body)
			}
		}()
	}
	wg.Wait()

	stored, err := h.DB.GetSetting(context.Background(), "license_jwt")
	if err != nil {
		t.Fatal(err)
	}
	if h.License.Key() != stored {
		t.Error("holder key and stored license_jwt diverged")
	}
	if got := h.License.Load().CustomerID(); got != keys[stored] {
		t.Errorf("holder customer = %q, stored key belongs to %q", got, keys[stored])
	}
}

// TestSetLicense_TrialInstanceBinding verifies a trial bound to another
// instance is rejected with a clear message and leaves the license untouched.
func TestSetLicense_TrialInstanceBinding(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)
	app, h, keyCache, _ := setupTrialApp(t, "file:TestSetLicense_Binding?mode=memory&cache=private")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-lic")

	own, err := license.GetOrCreateInstanceID(context.Background(), h.DB)
	if err != nil {
		t.Fatal(err)
	}
	bound := func(instance string) string {
		return signedLicense(t, priv, "cust-trial", func(c *license.LicenseClaims) {
			c.Trial = true
			c.InstanceID = instance
		})
	}
	tests := []struct {
		name       string
		key        string
		wantStatus int
	}{
		{"other instance", bound("some-other-instance"), 400},
		{"own instance", bound(own), 200},
		{"unbound trial", bound(""), 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := h.License.Key()
			status, body := putLicense(t, app, token, tt.key)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %v", status, tt.wantStatus, body)
			}
			if tt.wantStatus == 400 {
				e, _ := body["error"].(map[string]any)
				if e["message"] != "this trial key belongs to another VoidLLM instance" {
					t.Errorf("message = %v", e["message"])
				}
				if h.License.Key() != before {
					t.Error("rejected key changed the holder")
				}
			}
		})
	}
}
