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
// replaced atomically. Rows with unparseable data are skipped with an error
// log rather than aborting the entire load.
//
// The database snapshot can be older than the cache by the time it is
// installed: a key evicted (Delete) or stored (Set) while the query ran is
// newer than the snapshot. The load is therefore bracketed by BeginLoad and
// CommitLoad, which keep such an evicted key out and such a stored key in.
func LoadKeysIntoCache(ctx context.Context, database *db.DB, keyCache *cache.Cache[string, KeyInfo], log *slog.Logger) error {
	return loadKeysIntoCache(ctx, database.LoadAllActiveKeys, keyCache, log)
}

// loadKeysIntoCache is LoadKeysIntoCache with the database read supplied as
// loadRecords.
func loadKeysIntoCache(
	ctx context.Context,
	loadRecords func(ctx context.Context) ([]db.KeyRecord, []error, error),
	keyCache *cache.Cache[string, KeyInfo],
	log *slog.Logger,
) error {
	// The token must be taken before the read so every eviction that the
	// snapshot might miss is recorded. AbortLoad is a no-op after CommitLoad.
	token := keyCache.BeginLoad()
	defer keyCache.AbortLoad(token)

	records, skipErrors, err := loadRecords(ctx)
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

	loaded := len(entries)
	if !keyCache.CommitLoad(token, entries) {
		// A load that began later has already been installed; its snapshot
		// is at least as recent as this one.
		log.LogAttrs(ctx, slog.LevelDebug, "key cache load superseded by a newer load",
			slog.Int("keys", loaded),
		)
		return nil
	}

	log.LogAttrs(ctx, slog.LevelDebug, "key cache loaded",
		slog.Int("keys", loaded),
	)

	return nil
}

// StartCacheRefresh starts a background goroutine that reloads the key cache
// through reloader every interval, so periodic reloads coalesce with reloads
// triggered by admin changes and pub/sub messages. Returns a stop function that
// blocks until the refresh goroutine has exited, ensuring a clean shutdown.
// Stop reloader first so a reload in flight is cancelled rather than awaited.
func StartCacheRefresh(reloader *Reloader, interval time.Duration) func() {
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
				// A failed load is logged by the reloader.
				_ = reloader.Reload(ctx)
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
