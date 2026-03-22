package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
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
		log.LogAttrs(ctx, slog.LevelError, "skipped corrupt key record during cache load",
			slog.String("error", skipErr.Error()),
		)
	}

	entries := make(map[string]KeyInfo, len(records))

	for _, r := range records {
		ki := KeyInfo{
			ID:                    r.ID,
			KeyType:               r.KeyType,
			Name:                  r.Name,
			OrgID:                 r.OrgID,
			DailyTokenLimit:       r.DailyTokenLimit,
			MonthlyTokenLimit:     r.MonthlyTokenLimit,
			RequestsPerMinute:     r.RequestsPerMinute,
			RequestsPerDay:        r.RequestsPerDay,
			OrgDailyTokenLimit:    r.OrgDailyTokenLimit,
			OrgMonthlyTokenLimit:  r.OrgMonthlyTokenLimit,
			OrgRequestsPerMinute:  r.OrgRequestsPerMinute,
			OrgRequestsPerDay:     r.OrgRequestsPerDay,
			TeamDailyTokenLimit:   r.TeamDailyTokenLimit,
			TeamMonthlyTokenLimit: r.TeamMonthlyTokenLimit,
			TeamRequestsPerMinute: r.TeamRequestsPerMinute,
			TeamRequestsPerDay:    r.TeamRequestsPerDay,
			ExpiresAt:             r.ExpiresAt,
		}

		if r.TeamID != nil {
			ki.TeamID = *r.TeamID
		}
		if r.UserID != nil {
			ki.UserID = *r.UserID
		}
		if r.ServiceAccountID != nil {
			ki.ServiceAccountID = *r.ServiceAccountID
		}

		// Resolve role inline from JOIN columns — no secondary DB query needed.
		switch r.KeyType {
		case keygen.KeyTypeUser, keygen.KeyTypeSession:
			if r.IsSystemAdmin == 1 {
				ki.Role = RoleSystemAdmin
			} else if r.MembershipRole != "" {
				ki.Role = r.MembershipRole
			} else {
				log.LogAttrs(ctx, slog.LevelWarn, "load keys: user has no org membership, defaulting to member",
					slog.String("user_id", ki.UserID),
					slog.String("org_id", ki.OrgID),
				)
				ki.Role = RoleMember
			}
		case keygen.KeyTypeTeam:
			ki.Role = RoleTeamAdmin
		case keygen.KeyTypeSA:
			if ki.TeamID != "" {
				ki.Role = RoleTeamAdmin
			} else {
				ki.Role = RoleOrgAdmin
			}
		default:
			log.LogAttrs(ctx, slog.LevelWarn, "load keys: unknown key type, defaulting to member",
				slog.String("key_type", r.KeyType),
			)
			ki.Role = RoleMember
		}

		entries[r.KeyHash] = ki
	}

	keyCache.LoadAll(entries)

	log.LogAttrs(ctx, slog.LevelInfo, "key cache loaded",
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
