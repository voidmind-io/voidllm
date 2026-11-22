package license

// White-box tests for heartbeat.go internals.
// Lives in package license to access runHeartbeat, communityLicense, and the
// withTestPublicKey / newTestKeypair / signTestJWT helpers from verify_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// discardLogger returns a slog.Logger that silently drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newCommunityHolder returns a *Holder pre-loaded with a communityLicense.
func newCommunityHolder() *Holder {
	return NewHolder(communityLicense{})
}

// mockVerifyServer starts an httptest.Server that always responds to POST
// /v1/verify with the given verifyResponse JSON.
func mockVerifyServer(t *testing.T, resp verifyResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/verify" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("mock server: encode response: %v", encErr)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHeartbeatActiveStatus verifies that when the server returns "active"
// with no key in the response body, runHeartbeat returns the same rawKey
// and leaves the holder unchanged.
func TestHeartbeatActiveStatus(t *testing.T) {
	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q (holder must not be modified)",
			holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatActiveNoRefreshNeeded verifies that even when the server
// returns a fresh key, the holder is NOT updated if the current license has
// no expiry (community license, zero ExpiresAt = never expires / no threshold met).
func TestHeartbeatActiveNoRefreshNeeded(t *testing.T) {
	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
		Key:    "some-fresh-jwt",
	})

	// Community license: ExpiresAt() == zero time, so refresh threshold is
	// never met (the condition requires !expiresAt.IsZero()).
	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q (no refresh for community license)", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q (holder must not change when refresh not needed)",
			holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatActiveRefreshNeeded verifies that when the current license
// expires within the refresh threshold AND the server returns a valid fresh
// JWT, the holder is updated and the new key is returned.
func TestHeartbeatActiveRefreshNeeded(t *testing.T) {
	// Swap embeddedPublicKey so ValidateKey accepts our test-signed JWT.
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	// Build a fresh JWT that expires far in the future.
	freshClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(60 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_refresh_test",
	}
	freshJWT := signTestJWT(t, priv, freshClaims)

	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
		Key:    freshJWT,
	})

	// Build a holder pre-loaded with an enterprise license that expires within
	// the refresh threshold (e.g. 3 days from now < 7-day threshold).
	expiringSoonClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(3 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_refresh_test",
	}
	expiringSoonJWT := signTestJWT(t, priv, expiringSoonClaims)

	expiringSoon, err := ValidateKey(expiringSoonJWT)
	if err != nil {
		t.Fatalf("ValidateKey() expiring-soon license: %v", err)
	}

	holder := NewHolder(expiringSoon)
	log := discardLogger()

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, "", log, nil)

	if gotKey != freshJWT {
		t.Errorf("runHeartbeat() returned key %q, want fresh JWT", gotKey)
	}
	if holder.Load().Edition() != EditionEnterprise {
		t.Errorf("holder.Load().Edition() = %q, want %q (holder must be updated with refreshed license)",
			holder.Load().Edition(), EditionEnterprise)
	}
	if holder.Load().CustomerID() != "cust_refresh_test" {
		t.Errorf("holder.Load().CustomerID() = %q, want %q", holder.Load().CustomerID(), "cust_refresh_test")
	}
}

// TestHeartbeatActiveRefreshInvalidKey verifies that when the server returns a
// key that fails ValidateKey, the original rawKey is returned unchanged.
func TestHeartbeatActiveRefreshInvalidKey(t *testing.T) {
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	// Build an expiring-soon license so the refresh threshold is met.
	expiringSoonClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(2 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_invalid_refresh",
	}
	expiringSoonJWT := signTestJWT(t, priv, expiringSoonClaims)
	expiringSoon, err := ValidateKey(expiringSoonJWT)
	if err != nil {
		t.Fatalf("ValidateKey() expiring-soon license: %v", err)
	}

	// Server returns garbage as the refresh key.
	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
		Key:    "not-a-valid-jwt",
	})

	holder := NewHolder(expiringSoon)
	log := discardLogger()

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, "", log, nil)

	if gotKey != expiringSoonJWT {
		t.Errorf("runHeartbeat() returned key %q, want original %q", gotKey, expiringSoonJWT)
	}
	// Holder should remain unchanged — still holding the original expiring-soon license.
	if holder.Load().Edition() != EditionEnterprise {
		t.Errorf("holder.Load().Edition() = %q, want %q (invalid refresh must not clobber existing license)",
			holder.Load().Edition(), EditionEnterprise)
	}
}

// TestHeartbeatActiveRefreshCustomerMismatch verifies that a refreshed JWT
// with a different customer_id is rejected.
func TestHeartbeatActiveRefreshCustomerMismatch(t *testing.T) {
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	// Fresh JWT with a DIFFERENT customer_id.
	freshClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(60 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "attacker_customer",
	}
	freshJWT := signTestJWT(t, priv, freshClaims)

	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
		Key:    freshJWT,
	})

	// Current license expires soon with a different customer_id.
	expiringSoonClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(3 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "legit_customer",
	}
	expiringSoonJWT := signTestJWT(t, priv, expiringSoonClaims)
	expiringSoon, err := ValidateKey(expiringSoonJWT)
	if err != nil {
		t.Fatalf("ValidateKey() error: %v", err)
	}

	holder := NewHolder(expiringSoon)
	log := discardLogger()

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, "", log, nil)

	if gotKey != expiringSoonJWT {
		t.Errorf("runHeartbeat() returned key %q, want original (mismatch should be rejected)", gotKey)
	}
	if holder.Load().CustomerID() != "legit_customer" {
		t.Errorf("holder.Load().CustomerID() = %q, want %q (mismatched refresh must be rejected)",
			holder.Load().CustomerID(), "legit_customer")
	}
}

// TestHeartbeatRevokedStatus verifies that a "revoked" response leaves the
// holder unchanged and returns the same rawKey.
func TestHeartbeatRevokedStatus(t *testing.T) {
	srv := mockVerifyServer(t, verifyResponse{
		Status:    "revoked",
		RevokedAt: "2026-01-01T00:00:00Z",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q (revoked must not modify holder)",
			holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatExpiredStatus verifies that an "expired" response from the
// server leaves the holder unchanged.
func TestHeartbeatExpiredStatus(t *testing.T) {
	srv := mockVerifyServer(t, verifyResponse{
		Status: "expired",
		Plan:   "enterprise",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q", holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatNetworkError verifies that a network failure is handled
// gracefully: the same rawKey is returned and the holder is not modified.
func TestHeartbeatNetworkError(t *testing.T) {
	t.Parallel()

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	// Use a URL that is guaranteed to fail connection.
	got := runHeartbeat(context.Background(), holder, rawKey, "http://127.0.0.1:1", "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q (network error must not modify holder)",
			holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatUnknownStatus verifies that an unrecognised status string is
// handled gracefully.
func TestHeartbeatUnknownStatus(t *testing.T) {
	t.Parallel()

	srv := mockVerifyServer(t, verifyResponse{
		Status: "unknown",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
}

// TestHeartbeatErrorStatus verifies that an "error" response from the server
// is handled gracefully.
func TestHeartbeatErrorStatus(t *testing.T) {
	t.Parallel()

	srv := mockVerifyServer(t, verifyResponse{
		Status:  "error",
		Message: "bad key",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q", holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatDefaultStatus verifies that an unexpected/default status from
// the server is handled gracefully (falls through to the default case).
func TestHeartbeatDefaultStatus(t *testing.T) {
	t.Parallel()

	srv := mockVerifyServer(t, verifyResponse{
		Status: "maintenance",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
}

// mockSettingsRW implements SettingsReadWriter for testing. It records every
// SetSetting call and returns empty strings from GetSetting by default.
type mockSettingsRW struct {
	calls []struct{ key, value string }
}

// GetSetting always returns an empty string (no pre-existing settings).
func (m *mockSettingsRW) GetSetting(_ context.Context, _ string) (string, error) {
	return "", nil
}

// SetSetting records the call and returns nil.
func (m *mockSettingsRW) SetSetting(_ context.Context, key, value string) error {
	m.calls = append(m.calls, struct{ key, value string }{key, value})
	return nil
}

// SetSettingIfNotExists is a no-op for test purposes.
func (m *mockSettingsRW) SetSettingIfNotExists(_ context.Context, _ string, _ string) error {
	return nil
}

// TestHeartbeatActiveRefreshPersistsToDB verifies that when a license refresh
// succeeds, runHeartbeat calls SetSetting with key "license_jwt" and the fresh
// JWT value so that the new key survives a container restart.
func TestHeartbeatActiveRefreshPersistsToDB(t *testing.T) {
	// Mutates embeddedPublicKey — must not run in parallel with other key-swapping tests.
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	// Build a fresh JWT that expires well beyond the refresh threshold.
	freshClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(60 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_persist_test",
	}
	freshJWT := signTestJWT(t, priv, freshClaims)

	srv := mockVerifyServer(t, verifyResponse{
		Status: "active",
		Plan:   "enterprise",
		Key:    freshJWT,
	})

	// Build an expiring-soon license so the refresh threshold is met.
	expiringSoonClaims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(3 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_persist_test",
	}
	expiringSoonJWT := signTestJWT(t, priv, expiringSoonClaims)

	expiringSoon, err := ValidateKey(expiringSoonJWT)
	if err != nil {
		t.Fatalf("ValidateKey() expiring-soon license: %v", err)
	}

	holder := NewHolder(expiringSoon)
	log := discardLogger()
	sw := &mockSettingsRW{}

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, "", log, sw)

	// The returned key must be the fresh JWT.
	if gotKey != freshJWT {
		t.Errorf("runHeartbeat() returned key %q, want fresh JWT", gotKey)
	}

	// SetSetting must have been called at least once with key "license_jwt".
	var licenseJWTCall *struct{ key, value string }
	for i := range sw.calls {
		if sw.calls[i].key == "license_jwt" {
			licenseJWTCall = &sw.calls[i]
			break
		}
	}
	if licenseJWTCall == nil {
		t.Fatalf("SetSetting was never called with key %q; all calls: %v", "license_jwt", sw.calls)
	}

	// The value must be the fresh JWT as returned by the server.
	if licenseJWTCall.value != freshJWT {
		t.Errorf("SetSetting value = %q, want fresh JWT", licenseJWTCall.value)
	}
}

// TestHeartbeatPaymentWarning verifies that a payment warning in an "active"
// response is handled without modifying the holder or the rawKey.
func TestHeartbeatPaymentWarning(t *testing.T) {
	t.Parallel()

	srv := mockVerifyServer(t, verifyResponse{
		Status:         "active",
		Plan:           "enterprise",
		PaymentWarning: "your card expires soon",
	})

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
	if holder.Load().Edition() != EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q", holder.Load().Edition(), EditionCommunity)
	}
}

// TestHeartbeatHTTPErrorStatus verifies that a non-200 HTTP status code from
// the server is treated as a network error (graceful handling).
func TestHeartbeatHTTPErrorStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	t.Cleanup(srv.Close)

	holder := newCommunityHolder()
	const rawKey = "original-key"
	log := discardLogger()

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, "", log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
}

// TestStartHeartbeatStops verifies that StartHeartbeat returns a stop function
// that shuts down the background goroutine cleanly and without panicking.
// The initial 1-minute delay means the heartbeat never actually fires in this test.
func TestStartHeartbeatStops(t *testing.T) {
	t.Parallel()

	srv := mockVerifyServer(t, verifyResponse{Status: "active", Plan: "enterprise"})

	holder := newCommunityHolder()

	cfg := HeartbeatConfig{
		ServerURL: srv.URL,
		Interval:  24 * time.Hour,
		Log:       discardLogger(),
	}

	stop := StartHeartbeat(holder, "test-key", cfg)

	// Stop immediately — the goroutine is still in the initialDelay select,
	// so the done channel fires before the first runHeartbeat call.
	stop()

	// If we reach here the goroutine exited cleanly (wg.Wait() returned).
}

// recordingVerifyServer records the key of every verify request and answers
// with respFor(key).
func recordingVerifyServer(t *testing.T, respFor func(key string) verifyResponse) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req verifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode verify request: %v", err)
		}
		mu.Lock()
		keys = append(keys, req.Key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(respFor(req.Key)); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), keys...)
	}
}

func waitForKey(t *testing.T, get func() []string, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, k := range get() {
			if k == want {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("heartbeat never sent key %q; sent %v", want, get())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func shortenInitialDelay(t *testing.T) {
	t.Helper()
	orig := initialDelay
	initialDelay = time.Millisecond
	t.Cleanup(func() { initialDelay = orig })
}

// fakeSettings is an in-memory SettingsReadWriter.
type fakeSettings struct {
	mu sync.Mutex
	m  map[string]string
	// ignoreDedup drops writes of the heartbeat dedup timestamp so every tick
	// sends.
	ignoreDedup bool
}

func newFakeSettings() *fakeSettings { return &fakeSettings{m: map[string]string{}} }

func (f *fakeSettings) GetSetting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[k], nil
}

func (f *fakeSettings) SetSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ignoreDedup && k == "heartbeat_last_sent" {
		return nil
	}
	f.m[k] = v
	return nil
}

func (f *fakeSettings) SetSettingIfNotExists(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[k]; !ok {
		f.m[k] = v
	}
	return nil
}

func testEnterprise(customer string, trial bool, in time.Duration) *enterpriseLicense {
	l := newExpiringLicense(time.Now().Add(in), trial)
	l.customerID = customer
	return l
}

// TestHeartbeatManager_SendsCurrentKey verifies that a key activated after the
// heartbeat started is the one sent on the next tick.
func TestHeartbeatManager_SendsCurrentKey(t *testing.T) {
	shortenInitialDelay(t)
	srv, sent := recordingVerifyServer(t, func(string) verifyResponse {
		return verifyResponse{Status: "active"}
	})
	holder := NewHolder(communityLicense{})
	m := NewHeartbeatManager(holder, HeartbeatConfig{
		ServerURL: srv.URL, Interval: 10 * time.Millisecond, Log: discardLogger(),
	})
	defer m.Stop()

	if !m.Ensure(context.Background(), "key-A") {
		t.Fatal("first Ensure did not start the heartbeat")
	}
	waitForKey(t, sent, "key-A")
	if err := holder.Activate(testEnterprise("c", false, time.Hour), "key-B", nil); err != nil {
		t.Fatal(err)
	}
	if m.Ensure(context.Background(), "key-B") {
		t.Fatal("second Ensure started a second heartbeat")
	}
	waitForKey(t, sent, "key-B")
}

// TestHeartbeatManager_RefreshedKeySentNext verifies that a key refreshed by the
// license server updates the holder, the stored setting and is the one sent on
// the following tick.
func TestHeartbeatManager_RefreshedKeySentNext(t *testing.T) {
	shortenInitialDelay(t)
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	mk := func(in time.Duration) string {
		return signTestJWT(t, priv, LicenseClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(in)),
				IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
				Issuer:    "voidllm.ai",
			},
			Plan: "enterprise", Features: []string{FeatureAuditLogs},
			MaxOrgs: -1, MaxTeams: -1, CustomerID: "cust_x",
		})
	}
	expiring := mk(3 * 24 * time.Hour)
	fresh := mk(60 * 24 * time.Hour)

	srv, sent := recordingVerifyServer(t, func(key string) verifyResponse {
		if key == expiring {
			return verifyResponse{Status: "active", Plan: "enterprise", Key: fresh}
		}
		return verifyResponse{Status: "active"}
	})
	lic, err := ValidateKey(expiring)
	if err != nil {
		t.Fatalf("ValidateKey: %v", err)
	}
	holder := NewHolder(lic)
	db := newFakeSettings()
	db.ignoreDedup = true
	m := NewHeartbeatManager(holder, HeartbeatConfig{
		ServerURL: srv.URL, Interval: 10 * time.Millisecond, Log: discardLogger(), DB: db, InstanceID: "i",
	})
	defer m.Stop()

	m.Ensure(context.Background(), expiring)
	waitForKey(t, sent, fresh)
	if holder.Key() != fresh {
		t.Error("holder key was not updated to the refreshed key")
	}
	if got, _ := db.GetSetting(context.Background(), "license_jwt"); got != fresh {
		t.Error("refreshed key was not persisted")
	}
	if got := holder.Load().ExpiresAt(); time.Until(got) < 30*24*time.Hour {
		t.Errorf("holder not updated with refreshed license, expires %v", got)
	}
}

// TestHolder_StaleRefreshDropped verifies that a refresh for a key that was
// replaced by an activation is written nowhere.
func TestHolder_StaleRefreshDropped(t *testing.T) {
	holder := NewHolder(communityLicense{})
	db := newFakeSettings()
	activate := func(lic License, key string) {
		t.Helper()
		err := holder.Activate(lic, key, func() error { return db.SetSetting(context.Background(), "license_jwt", key) })
		if err != nil {
			t.Fatal(err)
		}
	}
	activate(testEnterprise("A", false, time.Hour), "key-A")
	activate(testEnterprise("B", false, time.Hour), "key-B")

	applied, err := holder.refresh("key-A", "key-A2", testEnterprise("A2", false, time.Hour),
		func() error { return db.SetSetting(context.Background(), "license_jwt", "key-A2") })
	if err != nil || applied {
		t.Fatalf("stale refresh applied=%v err=%v, want dropped", applied, err)
	}
	if holder.Key() != "key-B" || holder.Load().CustomerID() != "B" {
		t.Errorf("holder = %q/%q, want key-B/B", holder.Key(), holder.Load().CustomerID())
	}
	if got, _ := db.GetSetting(context.Background(), "license_jwt"); got != "key-B" {
		t.Errorf("db key = %q, want key-B", got)
	}

	applied, err = holder.refresh("key-B", "key-B2", testEnterprise("B", false, 2*time.Hour),
		func() error { return db.SetSetting(context.Background(), "license_jwt", "key-B2") })
	if err != nil || !applied || holder.Key() != "key-B2" {
		t.Fatalf("current refresh applied=%v err=%v key=%q", applied, err, holder.Key())
	}
}

// TestHolder_ConcurrentActivateAndRefresh verifies, under -race, that however
// activations and refreshes interleave, the holder, the persisted key and the
// recorded key end on the same value.
func TestHolder_ConcurrentActivateAndRefresh(t *testing.T) {
	for round := 0; round < 20; round++ {
		holder := NewHolder(communityLicense{})
		db := newFakeSettings()
		persist := func(key string) func() error {
			return func() error { return db.SetSetting(context.Background(), "license_jwt", key) }
		}
		if err := holder.Activate(testEnterprise("k0", false, time.Hour), "k0", persist("k0")); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		for i := 1; i <= 8; i++ {
			key := "k" + string(rune('0'+i))
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := holder.Activate(testEnterprise(key, false, time.Hour), key, persist(key)); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				refreshed := key + "-refreshed"
				if _, err := holder.refresh("k0", refreshed, testEnterprise(refreshed, false, time.Hour), persist(refreshed)); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()

		got, _ := db.GetSetting(context.Background(), "license_jwt")
		if holder.Key() != got || holder.Load().CustomerID() != got {
			t.Fatalf("round %d diverged: key=%q db=%q customer=%q", round, holder.Key(), got, holder.Load().CustomerID())
		}
	}
}

// TestHeartbeatManager_ActivatedTriggersSoon verifies that a newly activated
// key is verified shortly after activation, even when the multi-pod dedup
// marker says a heartbeat was sent recently and the daily tick is far away.
func TestHeartbeatManager_ActivatedTriggersSoon(t *testing.T) {
	shortenInitialDelay(t)
	origDelay := activationDelay
	activationDelay = 20 * time.Millisecond
	t.Cleanup(func() { activationDelay = origDelay })

	srv, sent := recordingVerifyServer(t, func(string) verifyResponse { return verifyResponse{Status: "active"} })
	db := newFakeSettings()
	holder := NewHolder(communityLicense{})
	m := NewHeartbeatManager(holder, HeartbeatConfig{
		ServerURL: srv.URL, Interval: time.Hour, Log: discardLogger(), DB: db, InstanceID: "i",
	})
	defer m.Stop()

	m.Ensure(context.Background(), "key-A")
	waitForKey(t, sent, "key-A")
	// The first beat recorded a fresh dedup timestamp; B must still go out.
	// The server records the key before the client writes the marker, so poll.
	markerDeadline := time.Now().Add(2 * time.Second)
	for {
		if last, _ := db.GetSetting(context.Background(), "heartbeat_last_sent"); last != "" {
			break
		}
		if time.Now().After(markerDeadline) {
			t.Fatal("dedup marker not set after first heartbeat")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := holder.Activate(testEnterprise("B", false, time.Hour), "key-B", nil); err != nil {
		t.Fatal(err)
	}
	m.Activated(context.Background(), "key-B")
	waitForKey(t, sent, "key-B")
}

// TestRunHeartbeat_TrialTermination verifies that trial licenses are swapped to
// community when the server reports the trial ended, while paid licenses are
// left alone.
func TestRunHeartbeat_TrialTermination(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		trial    bool
		wantSwap bool
	}{
		{"trial expired", "expired", true, true},
		{"trial revoked", "revoked", true, true},
		{"trial instance mismatch", "instance_mismatch", true, true},
		{"trial active", "active", true, false},
		{"trial unknown", "unknown", true, false},
		{"paid expired", "expired", false, false},
		{"paid revoked", "revoked", false, false},
		{"paid instance mismatch", "instance_mismatch", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := mockVerifyServer(t, verifyResponse{Status: tt.status})
			holder := NewHolder(testEnterprise("c", tt.trial, time.Hour))
			runHeartbeat(context.Background(), holder, "k", srv.URL, "i", discardLogger(), nil)
			swapped := holder.Load().Edition() == EditionCommunity
			if swapped != tt.wantSwap {
				t.Errorf("swapped = %v, want %v", swapped, tt.wantSwap)
			}
		})
	}
}

// TestHeartbeatManager_TrialEndedRecordsState verifies that the heartbeat flags
// the expiry state with was_trial and runs the downgrade hook.
func TestHeartbeatManager_TrialEndedRecordsState(t *testing.T) {
	shortenInitialDelay(t)
	srv, _ := recordingVerifyServer(t, func(string) verifyResponse { return verifyResponse{Status: "expired"} })
	holder := NewHolder(testEnterprise("c", true, time.Hour))
	state := NewExpiryState(true)
	var downgraded atomic.Int32
	m := NewHeartbeatManager(holder, HeartbeatConfig{
		ServerURL: srv.URL, Interval: time.Hour, Log: discardLogger(),
		State: state, OnDowngrade: func(context.Context) { downgraded.Add(1) },
	})
	defer m.Stop()
	m.Ensure(context.Background(), "trial-key")

	deadline := time.After(5 * time.Second)
	for !state.Expired() {
		select {
		case <-deadline:
			t.Fatal("state never flagged")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !state.WasTrial() || !state.RestartRequired() {
		t.Errorf("wasTrial=%v restart=%v, want true/true", state.WasTrial(), state.RestartRequired())
	}
	if downgraded.Load() != 1 {
		t.Errorf("OnDowngrade calls = %d, want 1", downgraded.Load())
	}
}

// TestValidateKeyForInstance verifies trial instance binding.
func TestValidateKeyForInstance(t *testing.T) {
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	mk := func(trial bool, instance string) string {
		return signTestJWT(t, priv, LicenseClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(time.Hour)),
				Issuer:    "voidllm.ai",
			},
			Plan: "enterprise", Features: []string{FeatureAuditLogs},
			MaxOrgs: -1, MaxTeams: -1, CustomerID: "c", Trial: trial, InstanceID: instance,
		})
	}
	tests := []struct {
		name     string
		key      string
		instance string
		wantErr  bool
	}{
		{"bound trial, same instance", mk(true, "inst-1"), "inst-1", false},
		{"bound trial, other instance", mk(true, "inst-1"), "inst-2", true},
		{"bound trial, unknown instance", mk(true, "inst-1"), "", true},
		{"unbound trial", mk(true, ""), "inst-2", false},
		{"paid with instance claim is not bound", mk(false, "inst-1"), "inst-2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lic, err := ValidateKeyForInstance(tt.key, tt.instance)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidKey) || !errors.Is(err, ErrTrialWrongInstance) {
					t.Errorf("err = %v, want ErrInvalidKey and ErrTrialWrongInstance", err)
				}
				if !strings.Contains(err.Error(), "this trial key belongs to another VoidLLM instance") {
					t.Errorf("err message = %q", err)
				}
			}
			verified := VerifyForInstance(tt.key, false, tt.instance)
			if tt.wantErr == (verified.Edition() == EditionEnterprise) {
				t.Errorf("VerifyForInstance edition = %v, wantErr %v", verified.Edition(), tt.wantErr)
			}
			if !tt.wantErr && lic == nil {
				t.Error("nil license")
			}
		})
	}
	// Legacy entry points do not enforce binding (offline CLI use).
	if _, err := ValidateKey(mk(true, "inst-1")); err != nil {
		t.Errorf("ValidateKey must not enforce binding: %v", err)
	}
}

// TestRunHeartbeat_TerminalVerdictForReplacedKeyIgnored verifies that an
// "expired" verdict for key A, still in flight when trial B was activated,
// does not end B.
func TestRunHeartbeat_TerminalVerdictForReplacedKeyIgnored(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		<-release
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(verifyResponse{Status: "expired"}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	holder := NewHolder(communityLicense{})
	if err := holder.Activate(testEnterprise("A", true, time.Hour), "key-A", nil); err != nil {
		t.Fatal(err)
	}
	db := newFakeSettings()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHeartbeat(context.Background(), holder, "key-A", srv.URL, "i", discardLogger(), db)
	}()

	<-inFlight
	b := testEnterprise("B", true, time.Hour)
	b.id = "jti-B"
	if err := holder.Activate(b, "key-B", nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	if holder.Key() != "key-B" || holder.Load().Edition() != EditionEnterprise || holder.Load().CustomerID() != "B" {
		t.Errorf("trial B was ended by a verdict for key A: key=%q edition=%v", holder.Key(), holder.Load().Edition())
	}
	if got, _ := db.GetSetting(context.Background(), endedTrialsSetting); got != "" {
		t.Errorf("ended trials = %q, want none", got)
	}
}

// TestEndedTrial_PersistedAcrossRestart verifies that a trial ended by the
// server (or by local expiry) is refused after a restart, while paid keys are
// never affected.
func TestEndedTrial_PersistedAcrossRestart(t *testing.T) {
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)
	mk := func(trial bool, id string) string {
		return signTestJWT(t, priv, LicenseClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(time.Hour)),
				Issuer:    "voidllm.ai",
				ID:        id,
			},
			Plan: "enterprise", Features: []string{FeatureAuditLogs},
			MaxOrgs: -1, MaxTeams: -1, CustomerID: "c", Trial: trial,
		})
	}
	ctx := context.Background()

	for _, status := range []string{"expired", "revoked", "instance_mismatch"} {
		t.Run("server "+status, func(t *testing.T) {
			db := newFakeSettings()
			trialKey := mk(true, "jti-trial-"+status)
			paidKey := mk(false, "jti-paid-"+status)

			lic, err := LoadForStartup(ctx, db, trialKey, false, "inst")
			if err != nil || lic.Edition() != EditionEnterprise {
				t.Fatalf("fresh trial: edition=%v err=%v", lic.Edition(), err)
			}
			holder := NewHolder(lic)
			holder.seedKey(trialKey)
			srv := mockVerifyServer(t, verifyResponse{Status: status})
			runHeartbeat(ctx, holder, trialKey, srv.URL, "inst", discardLogger(), db)
			if holder.Load().Edition() != EditionCommunity {
				t.Fatal("trial not ended by server verdict")
			}

			// Restart: a new holder is built from the same stored key.
			lic, err = LoadForStartup(ctx, db, trialKey, false, "inst")
			if !errors.Is(err, ErrTrialEnded) || lic.Edition() != EditionCommunity {
				t.Errorf("after restart: edition=%v err=%v, want community/ErrTrialEnded", lic.Edition(), err)
			}
			if err := CheckTrialNotEnded(ctx, db, mustValidate(t, trialKey)); !errors.Is(err, ErrTrialEnded) ||
				!errors.Is(err, ErrInvalidKey) || !strings.Contains(err.Error(), "this trial has ended") {
				t.Errorf("CheckTrialNotEnded = %v", err)
			}

			// Paid keys are never affected, even if one was passed to Record.
			paid := mustValidate(t, paidKey)
			if err := RecordEndedTrial(ctx, db, paid); err != nil {
				t.Fatal(err)
			}
			if lic, err := LoadForStartup(ctx, db, paidKey, false, "inst"); err != nil || lic.Edition() != EditionEnterprise {
				t.Errorf("paid key: edition=%v err=%v", lic.Edition(), err)
			}
		})
	}

	t.Run("local expiry", func(t *testing.T) {
		db := newFakeSettings()
		ent := testEnterprise("c", true, -time.Minute)
		ent.id = "jti-local"
		holder := NewHolder(ent)
		if !CheckExpiry(ctx, holder, ExpiryWatcherConfig{DB: db, State: NewExpiryState(false), Log: discardLogger()}) {
			t.Fatal("no swap")
		}
		if err := CheckTrialNotEnded(ctx, db, ent); !errors.Is(err, ErrTrialEnded) {
			t.Errorf("CheckTrialNotEnded = %v, want ErrTrialEnded", err)
		}
	})

	t.Run("list is capped", func(t *testing.T) {
		db := newFakeSettings()
		for i := 0; i < maxEndedTrials+5; i++ {
			l := testEnterprise("c", true, time.Hour)
			l.id = "id-" + strconv.Itoa(i)
			if err := RecordEndedTrial(ctx, db, l); err != nil {
				t.Fatal(err)
			}
		}
		ids, err := loadEndedTrials(ctx, db)
		if err != nil || len(ids) != maxEndedTrials || ids[len(ids)-1] != "id-"+strconv.Itoa(maxEndedTrials+4) {
			t.Errorf("len=%d err=%v", len(ids), err)
		}
	})
}

func mustValidate(t *testing.T, key string) License {
	t.Helper()
	lic, err := ValidateKey(key)
	if err != nil {
		t.Fatalf("ValidateKey: %v", err)
	}
	return lic
}
