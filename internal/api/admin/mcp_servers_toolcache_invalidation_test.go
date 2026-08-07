package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers docs/mcp-v2.md Fund 9: UpdateMCPServer must invalidate a
// server's cached tool listing (h.ToolCache) whenever the update changes a
// field that affects how VoidLLM authenticates to or reaches that server —
// the same field set MCPTransportCache.LoadAll itself compares
// (mcpAuthOrTransportFieldsChanged, mcp_servers.go) — but must NOT do so for
// a purely cosmetic change (e.g. only the display name), which would
// otherwise defeat the whole point of caching by forcing a fresh upstream
// fetch on every unrelated edit.
//
// ToolCache.FreshFor is used as the presence/absence probe: it returns -1
// exactly when serverID has no cache entry at all (evicted, or never
// fetched), and a non-negative duration otherwise — see its own doc.

// setupToolCacheInvalidationApp is like setupMCPServersTestAppWithToolCache
// but also returns the *mcp.ToolCache directly, which this file's tests need
// to probe via FreshFor after each update — a capability
// setupMCPServersTestAppWithToolCache's own return signature does not
// expose, since none of its other callers need it.
func setupToolCacheInvalidationApp(t *testing.T, dsn string, staticTools []mcp.Tool) (*db.DB, *cache.Cache[string, auth.KeyInfo], *fiber.App, *mcp.ToolCache) {
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

	keyCache := cache.New[string, auth.KeyInfo]()

	fetcher := mcp.ToolFetcher(func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: staticTools}, nil
	})
	toolCache := mcp.NewToolCache(fetcher, time.Hour)

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPAllowPrivateURLs: true,
		ToolCache:           toolCache,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return database, keyCache, app, toolCache
}

// primeToolCacheEntry populates toolCache's in-memory entry for serverID via
// RefreshServer (a real fetch through the static fetcher
// setupToolCacheInvalidationApp wires in), so a later invalidation (or lack
// of one) has something to actually observe.
func primeToolCacheEntry(t *testing.T, toolCache *mcp.ToolCache, serverID string) {
	t.Helper()
	if err := toolCache.RefreshServer(context.Background(), serverID); err != nil {
		t.Fatalf("RefreshServer(%q): %v", serverID, err)
	}
	if toolCache.FreshFor(serverID) < 0 {
		t.Fatalf("FreshFor(%q) < 0 immediately after RefreshServer, want a cached entry", serverID)
	}
}

func TestUpdateMCPServer_ToolCacheInvalidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// slug is a lowercase-alphanumeric-and-hyphens-only identifier
		// derived from name, for use in the DSN and MCP server alias — both
		// of which reject the underscores, uppercase letters, and
		// apostrophes some of the names below contain.
		slug           string
		patch          map[string]any
		wantInvalidate bool
	}{
		{
			name:           "auth_token change invalidates the cached listing",
			slug:           "auth-token-change",
			patch:          map[string]any{"auth_token": "brand-new-credential"},
			wantInvalidate: true,
		},
		{
			name:           "url change invalidates the cached listing",
			slug:           "url-change",
			patch:          map[string]any{"url": "https://a-different-upstream.example.com"},
			wantInvalidate: true,
		},
		{
			name:           "auth_header change invalidates the cached listing",
			slug:           "auth-header-change",
			patch:          map[string]any{"auth_header": "X-Different-Header"},
			wantInvalidate: true,
		},
		{
			name:           "cosmetic name-only change does NOT invalidate the cached listing",
			slug:           "cosmetic-name-change",
			patch:          map[string]any{"name": "A Purely Cosmetic New Display Name"},
			wantInvalidate: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			slug := tc.slug
			dsn := "file:TestUpdateMCPServer_ToolCacheInvalidation_" + slug + "?mode=memory&cache=private"
			staticTools := []mcp.Tool{{Name: "static-tool", InputSchema: mcp.ObjectSchema(nil)}}
			_, keyCache, app, toolCache := setupToolCacheInvalidationApp(t, dsn, staticTools)

			key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-toolcache-inval-"+slug)

			created := createMCPServerViaAPI(t, app, key, map[string]any{
				"name":       "toolcache-inval-server",
				"alias":      "toolcache-inval-server-" + slug,
				"url":        "https://original-upstream.example.com",
				"auth_type":  "bearer",
				"auth_token": "original-credential",
			})
			serverID := created["id"].(string)

			primeToolCacheEntry(t, toolCache, serverID)

			resp := mcpServerRequest(t, app, http.MethodPatch,
				"/api/v1/mcp-servers/"+serverID, key, tc.patch)
			defer resp.Body.Close()
			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("PATCH status = %d, want 200", resp.StatusCode)
			}

			fresh := toolCache.FreshFor(serverID)
			if tc.wantInvalidate {
				if fresh >= 0 {
					t.Errorf("FreshFor(%q) = %v after an auth/transport-relevant update, want < 0 (evicted)", serverID, fresh)
				}
			} else if fresh < 0 {
				t.Errorf("FreshFor(%q) = %v after a purely cosmetic update, want >= 0 (the cached listing must "+
					"survive an unrelated field change)", serverID, fresh)
			}
		})
	}
}
