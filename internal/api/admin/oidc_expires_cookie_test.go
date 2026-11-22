package admin_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
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
	"github.com/voidmind-io/voidllm/internal/sso"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// newMockIdP starts an OIDC identity provider that issues one RS256 ID token
// for the given nonce at its token endpoint.
func newMockIdP(t *testing.T, nonce string) *httptest.Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/auth",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": srv.URL, "aud": "client-id", "sub": "idp-subject-1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": nonce, "email": "sso-user@example.com", "email_verified": true, "name": "SSO User",
		})
		tok.Header["kid"] = "k1"
		signed, signErr := tok.SignedString(key)
		if signErr != nil {
			http.Error(w, signErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": signed,
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOIDCCallback_SetsExpiresAtCookie(t *testing.T) {
	t.Parallel()

	const (
		state = "state-123"
		nonce = "nonce-456"
	)
	idp := newMockIdP(t, nonce)

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver: "sqlite", DSN: "file:TestOIDC_ExpiresCookie?mode=memory&cache=private",
		MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	mustCreateOrg(t, database, "SSO Org", "sso-org")

	ssoCfg := config.SSOConfig{
		Enabled: true, Issuer: idp.URL, ClientID: "client-id", ClientSecret: "secret",
		RedirectURL:   "https://voidllm.example.com/api/v1/auth/oidc/callback",
		AutoProvision: true, DefaultOrgSlug: "sso-org",
	}
	provider, err := sso.NewProvider(ctx, ssoCfg)
	if err != nil {
		t.Fatalf("sso.NewProvider: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB: database, HMACSecret: testHMACSecret, KeyCache: keyCache,
		License:     license.NewHolder(license.Verify("", true)),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		SSOProvider: provider, SSOConfig: ssoCfg,
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	callback := func() *http.Response {
		req := httptest.NewRequest("GET", "/api/v1/auth/oidc/callback?code=abc&state="+state, nil)
		req.Header.Set("Cookie", "voidllm_oidc_state="+state+"|"+nonce)
		r, callErr := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if callErr != nil {
			t.Fatalf("callback: %v", callErr)
		}
		return r
	}

	first := callback()
	first.Body.Close()
	var firstToken string
	for _, ck := range first.Cookies() {
		if ck.Name == "voidllm_oidc_token" {
			firstToken = ck.Value
		}
	}
	firstHash := keygen.Hash(firstToken, testHMACSecret)
	if _, ok := keyCache.Get(firstHash); !ok {
		t.Fatal("first SSO session not cached")
	}

	resp := callback()
	defer resp.Body.Close()

	// The second callback must revoke and evict the first session.
	if _, ok := keyCache.Get(firstHash); ok {
		t.Error("previous SSO session still cached after a new callback")
	}

	if loc := resp.Header.Get("Location"); loc != "/auth/callback" {
		t.Fatalf("redirect = %q, want /auth/callback", loc)
	}
	cookies := map[string]*http.Cookie{}
	for _, ck := range resp.Cookies() {
		cookies[ck.Name] = ck
	}
	tokenCk, expCk := cookies["voidllm_oidc_token"], cookies["voidllm_oidc_expires_at"]
	if tokenCk == nil || expCk == nil {
		t.Fatalf("cookies = %v, want token and expires_at", cookies)
	}
	exp, err := time.Parse(time.RFC3339, expCk.Value)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", expCk.Value, err)
	}
	if d := time.Until(exp); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("expires_at in %v, want about 24h", d)
	}
	if expCk.Path != tokenCk.Path || expCk.MaxAge != tokenCk.MaxAge || expCk.SameSite != tokenCk.SameSite || expCk.Secure != tokenCk.Secure {
		t.Errorf("expires cookie attrs %+v differ from token cookie %+v", expCk, tokenCk)
	}
}
