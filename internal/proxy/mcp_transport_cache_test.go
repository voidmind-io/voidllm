package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// newTransportCacheTestDB opens a real, migrated in-memory SQLite database
// for MCPTransportCache tests. Each test gets its own private DSN so tests
// can run in parallel without sharing state.
func newTransportCacheTestDB(t *testing.T, dsn string) *db.DB {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return database
}

// TestMCPTransportCache_LoadAll_ProtocolVersionChange_RebuildsTransport is
// the regression test for the fixed LoadAll bug: previously ProtocolVersion
// was absent from the reuse-comparison, so changing ONLY a server's pinned
// mcp_servers.protocol_version left the OLD transport — built with the OLD
// pin baked into its HTTPTransport.pinnedVersion field at construction time —
// cached and in use until the process restarted. An admin who pinned a
// server to a different era via the Admin API would see no effect at all
// until a restart.
func TestMCPTransportCache_LoadAll_ProtocolVersionChange_RebuildsTransport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_ProtocolVersionChange?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:            "upstream-1",
		Alias:           "upstream-1",
		URL:             "https://upstream.example.com/mcp",
		AuthType:        "none",
		CreatedBy:       "test",
		ProtocolVersion: "auto",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})
	before, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after first LoadAll")
	}
	beforeTransport := before.Transport
	if beforeTransport == nil {
		t.Fatal("Transport = nil after first LoadAll")
	}

	// Change ONLY protocol_version — URL, AuthType, AuthHeader, and the
	// encrypted token fields are all untouched.
	newVersion := "2025-11-25"
	updated, err := database.UpdateMCPServer(ctx, created.ID, db.UpdateMCPServerParams{
		ProtocolVersion: &newVersion,
	})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if updated.ProtocolVersion != newVersion {
		t.Fatalf("test setup: updated.ProtocolVersion = %q, want %q", updated.ProtocolVersion, newVersion)
	}

	c.LoadAll([]db.MCPServer{*updated})
	after, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after second LoadAll")
	}

	if after.Transport == beforeTransport {
		t.Error("Transport unchanged after protocol_version changed — the pinned version override would remain silently in effect until process restart")
	}
	if after.ProtocolVersion != newVersion {
		t.Errorf("cached ProtocolVersion = %q, want %q", after.ProtocolVersion, newVersion)
	}
}

// TestMCPTransportCache_LoadAll_NoChange_ReusesTransport is the reuse-side
// counterpart of the regression test above: reloading with a server row that
// changed nothing at all — including protocol_version, once it participates
// in the comparison — must keep the SAME transport instance rather than
// rebuilding on every refresh tick.
func TestMCPTransportCache_LoadAll_NoChange_ReusesTransport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_NoChange?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:            "upstream-1",
		Alias:           "upstream-1",
		URL:             "https://upstream.example.com/mcp",
		AuthType:        "none",
		CreatedBy:       "test",
		ProtocolVersion: "2025-03-26",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})
	before, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after first LoadAll")
	}

	// Reload with the exact same row (fetched fresh from the DB, not the
	// same Go value) — nothing changed.
	reloaded, err := database.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	c.LoadAll([]db.MCPServer{*reloaded})

	after, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after second LoadAll")
	}
	if after.Transport != before.Transport {
		t.Error("Transport was rebuilt even though nothing changed, want the same instance reused")
	}
}

// ---- Fund 8: OAuthTokenURL/OAuthClientID/OAuthScopes must participate in --
// ---- LoadAll's reuse-vs-rebuild comparison ---------------------------------

// TestMCPTransportCache_LoadAll_OAuthFieldChange_RebuildsTransport is the
// regression test for docs/mcp-v2.md Fund 8: MCPTransportCache.LoadAll's
// reuse-vs-rebuild comparison previously omitted OAuthTokenURL,
// OAuthClientID, and OAuthScopes entirely — changing only one of them left
// the OLD transport, and its already-cached OAuth token built against the
// OLD issuer/client, silently in place until the next process restart or an
// unrelated field also changed at the same time. AuthType "none" is used
// deliberately: LoadAll's OAuth-field comparison runs unconditionally,
// regardless of AuthType (see resolvedMCPServer's own doc) — only the OAuth
// CONFIG construction is gated on AuthType == "oauth", and exercising that
// separate code path would force this test to also decrypt a real OAuth
// client secret, which is orthogonal to the comparison property under test.
func TestMCPTransportCache_LoadAll_OAuthFieldChange_RebuildsTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(p *db.UpdateMCPServerParams)
	}{
		{
			name: "OAuthTokenURL changes",
			mutate: func(p *db.UpdateMCPServerParams) {
				v := "https://issuer-2.example.com/token"
				p.OAuthTokenURL = &v
			},
		},
		{
			name: "OAuthClientID changes",
			mutate: func(p *db.UpdateMCPServerParams) {
				v := "client-id-2"
				p.OAuthClientID = &v
			},
		},
		{
			name: "OAuthScopes changes",
			mutate: func(p *db.UpdateMCPServerParams) {
				v := "scope-a scope-b"
				p.OAuthScopes = &v
			},
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			dsn := fmt.Sprintf("file:TestMCPTransportCache_OAuthFieldChange_%d?mode=memory&cache=private", i)
			database := newTransportCacheTestDB(t, dsn)

			created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
				Name:            "oauth-upstream",
				Alias:           "oauth-upstream",
				URL:             "https://upstream.example.com/mcp",
				AuthType:        "none",
				CreatedBy:       "test",
				ProtocolVersion: "auto",
				OAuthTokenURL:   "https://issuer-1.example.com/token",
				OAuthClientID:   "client-id-1",
				OAuthScopes:     "scope-a",
			})
			if err != nil {
				t.Fatalf("CreateMCPServer: %v", err)
			}

			c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
			t.Cleanup(c.Close)

			c.LoadAll([]db.MCPServer{*created})
			before, ok := c.Get(created.ID)
			if !ok {
				t.Fatal("Get() ok = false after first LoadAll")
			}
			beforeTransport := before.Transport

			var params db.UpdateMCPServerParams
			tc.mutate(&params)
			updated, err := database.UpdateMCPServer(ctx, created.ID, params)
			if err != nil {
				t.Fatalf("UpdateMCPServer: %v", err)
			}

			c.LoadAll([]db.MCPServer{*updated})
			after, ok := c.Get(created.ID)
			if !ok {
				t.Fatal("Get() ok = false after second LoadAll")
			}
			if after.Transport == beforeTransport {
				t.Errorf("Transport unchanged after %s — the OLD OAuth config (and its already-cached OAuth "+
					"token) would remain silently in effect until process restart", tc.name)
			}
		})
	}
}

// TestMCPTransportCache_LoadAll_OAuthFieldsUnchanged_ReusesTransport is the
// reuse-side counterpart of the regression test above: reloading with a
// server row whose OAuthTokenURL/OAuthClientID/OAuthScopes (and every other
// compared field) are all unchanged must keep the SAME transport instance,
// not rebuild on every refresh tick merely because these three fields now
// participate in the comparison.
func TestMCPTransportCache_LoadAll_OAuthFieldsUnchanged_ReusesTransport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_OAuthFieldsUnchanged?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:            "oauth-upstream-stable",
		Alias:           "oauth-upstream-stable",
		URL:             "https://upstream.example.com/mcp",
		AuthType:        "none",
		CreatedBy:       "test",
		ProtocolVersion: "auto",
		OAuthTokenURL:   "https://issuer.example.com/token",
		OAuthClientID:   "client-id",
		OAuthScopes:     "scope-a scope-b",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})
	before, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after first LoadAll")
	}

	reloaded, err := database.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	c.LoadAll([]db.MCPServer{*reloaded})

	after, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after second LoadAll")
	}
	if after.Transport != before.Transport {
		t.Error("Transport was rebuilt even though the OAuth fields did not change, want the same instance reused")
	}
}

// ---- Fund 3 (root cause): a decrypt failure must not leave a usable-------
// ---- looking transport cached ----------------------------------------------

// TestMCPTransportCache_LoadAll_DecryptFailure_ServerSkipped_GetReturnsNothing
// is the regression test for docs/mcp-v2.md Fund 3's root cause:
// MCPTransportCache.LoadAll must SKIP a server whose stored auth token
// cannot be decrypted (e.g. after an encryption key rotation, or — as here —
// a corrupted/invalid stored value) entirely, rather than building a
// transport with the plaintext left at its zero value, which would then send
// an empty credential-carrying header to the upstream — the same failure
// mode errEmptyAuthCredential (internal/mcp/http_transport.go) independently
// closes one layer down, for any transport that somehow still gets built
// with an empty token. Get(id) must miss exactly as it would for a server
// never loaded into the cache at all, so every caller (HandleMCPProxy,
// CallMCPTool, MakeToolFetcher) falls back to buildAdHocTransport — which
// decrypts the SAME stored value and fails the SAME way, returning a
// definite internal-error response — instead of silently reusing a transport
// built from a credential that could not be verified.
func TestMCPTransportCache_LoadAll_DecryptFailure_ServerSkipped_GetReturnsNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_DecryptFailure?mode=memory&cache=private")

	// Not valid base64 at all — DecryptString fails at the base64-decode
	// step, before AES-GCM ever runs. This is the same failure shape a
	// genuine encryption-key rotation produces (base64 decodes fine but GCM
	// authentication then fails); either way DecryptString returns an error.
	garbage := "not-valid-base64-ciphertext!!!"
	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:         "decrypt-fail-upstream",
		Alias:        "decrypt-fail-upstream",
		URL:          "https://upstream.example.com/mcp",
		AuthType:     "bearer",
		AuthTokenEnc: &garbage,
		CreatedBy:    "test",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})

	if _, ok := c.Get(created.ID); ok {
		t.Error("Get() ok = true after a decrypt failure, want false — a server whose credential cannot be " +
			"decrypted must have no cached transport at all")
	}
	if got := c.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0 — the decrypt-failed server must not occupy a cache slot", got)
	}
}

// TestMCPTransportCache_LoadAll_ValidToken_TransportBuilt is the
// counter-proof for the decrypt-failure test above: a server whose stored
// token DOES decrypt successfully gets a real, usable cached transport —
// Get must not start failing for every server merely because the
// decrypt-failure path now skips the broken ones.
func TestMCPTransportCache_LoadAll_ValidToken_TransportBuilt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_ValidToken?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:      "valid-token-upstream",
		Alias:     "valid-token-upstream",
		URL:       "https://upstream.example.com/mcp",
		AuthType:  "bearer",
		CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	encKey := make([]byte, 32)
	enc, err := crypto.EncryptString("real-bearer-token", encKey, []byte("mcp_server:"+created.ID))
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	updated, err := database.UpdateMCPServer(ctx, created.ID, db.UpdateMCPServerParams{AuthTokenEnc: &enc})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(encKey, true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*updated})

	if _, ok := c.Get(created.ID); !ok {
		t.Fatal("Get() ok = false for a server whose token decrypted successfully, want a cached transport")
	}
	if got := c.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}
