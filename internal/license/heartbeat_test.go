package license

// White-box tests for heartbeat.go internals.
// Lives in package license to access runHeartbeat, communityLicense, and the
// withTestPublicKey / newTestKeypair / signTestJWT helpers from verify_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, log, nil)

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

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, log, nil)

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

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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
	got := runHeartbeat(context.Background(), holder, rawKey, "http://127.0.0.1:1", log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

	if got != rawKey {
		t.Errorf("runHeartbeat() returned key %q, want %q", got, rawKey)
	}
}

// mockSettingsWriter records every SetSetting call for later inspection.
type mockSettingsWriter struct {
	calls []struct{ key, value string }
}

// SetSetting records the call and returns nil.
func (m *mockSettingsWriter) SetSetting(_ context.Context, key, value string) error {
	m.calls = append(m.calls, struct{ key, value string }{key, value})
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
	sw := &mockSettingsWriter{}

	gotKey := runHeartbeat(context.Background(), holder, expiringSoonJWT, srv.URL, log, sw)

	// The returned key must be the fresh JWT.
	if gotKey != freshJWT {
		t.Errorf("runHeartbeat() returned key %q, want fresh JWT", gotKey)
	}

	// SetSetting must have been called exactly once.
	if len(sw.calls) != 1 {
		t.Fatalf("SetSetting call count = %d, want 1", len(sw.calls))
	}

	// The call must use the canonical key "license_jwt".
	if sw.calls[0].key != "license_jwt" {
		t.Errorf("SetSetting key = %q, want %q", sw.calls[0].key, "license_jwt")
	}

	// The value must be the fresh JWT as returned by the server.
	if sw.calls[0].value != freshJWT {
		t.Errorf("SetSetting value = %q, want fresh JWT", sw.calls[0].value)
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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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

	got := runHeartbeat(context.Background(), holder, rawKey, srv.URL, log, nil)

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
