package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
)

// LoadKeysIntoCache queries all active (non-deleted) API keys from the database
// in a single JOIN query, resolves their effective RBAC role and org/team
// limits inline, and populates the key cache. Existing cache entries are
// replaced atomically via LoadAll. Rows with unparseable data are skipped with
// an error log rather than aborting the entire load.
func LoadKeysIntoCache(ctx context.Context, database *db.DB, keyCache *cache.Cache[string, KeyInfo], log *slog.Logger) error {
	records, skipErrors, err := database.LoadAllActiveKeys(ctx)
	if err != nil {
		return fmt.Errorf("load keys into cache: %w", err)
	}
	for _, skipErr := range skipErrors {
		log.LogAttrs(ctx, slog.LevelWarn, "skipped corrupt key record during cache load",
			slog.String("error", skipErr.Error()),
		)
	}

	entries := make(map[string]KeyInfo, len(records))

	for _, r := range records {
		if !Cacheable(r) {
			log.LogAttrs(ctx, slog.LevelWarn, "load keys: skipping non-cacheable key",
				slog.String("key_id", r.ID),
				slog.String("key_type", r.KeyType),
			)
			continue
		}
		ki, ok := KeyInfoFromRecord(r)
		if !ok {
			log.LogAttrs(ctx, slog.LevelWarn, "load keys: could not resolve a definite role, defaulting to member",
				slog.String("key_type", r.KeyType),
				slog.String("user_id", derefStr(r.UserID)),
				slog.String("org_id", r.OrgID),
			)
		}
		entries[r.KeyHash] = ki
	}

	keyCache.LoadAll(entries)

	log.LogAttrs(ctx, slog.LevelDebug, "key cache loaded",
		slog.Int("keys", len(entries)),
	)

	return nil
}

// StartCacheRefresh starts a background goroutine that reloads the key cache
// from the database every interval. Returns a stop function that blocks until
// the refresh goroutine has exited, ensuring a clean shutdown.
func StartCacheRefresh(database *db.DB, keyCache *cache.Cache[string, KeyInfo], interval time.Duration, log *slog.Logger) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := LoadKeysIntoCache(ctx, database, keyCache, log); err != nil {
					log.LogAttrs(ctx, slog.LevelError, "key cache refresh failed",
						slog.String("error", err.Error()),
					)
				}
				cancel()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}
