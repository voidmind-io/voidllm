// Package update provides a background checker that periodically queries
// GitHub Releases for new VoidLLM versions and caches the result in the
// settings table.
package update

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	githubReleasesURL = "https://api.github.com/repos/voidmind-io/voidllm/releases/latest"
	defaultInterval   = 24 * time.Hour
	initialDelay      = 5 * time.Minute
)

// SettingsReadWriter provides read/write access to the settings table.
type SettingsReadWriter interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// UpdateInfo holds the current and available version information.
type UpdateInfo struct {
	CurrentVersion   string `json:"current_version"`
	AvailableVersion string `json:"available_version,omitempty"`
	ReleaseNotes     string `json:"release_notes,omitempty"`
	ReleaseURL       string `json:"release_url,omitempty"`
	NeedsUpdate      bool   `json:"needs_update"`
	CheckedAt        string `json:"checked_at,omitempty"`
}

// Checker periodically queries GitHub Releases for new VoidLLM versions
// and caches the result in the settings table.
type Checker struct {
	currentVersion string
	db             SettingsReadWriter
	log            *slog.Logger
	client         *http.Client
}

// NewChecker creates an update checker that compares the running version
// against the latest GitHub release.
func NewChecker(currentVersion string, db SettingsReadWriter, log *slog.Logger) *Checker {
	return &Checker{
		currentVersion: strings.TrimPrefix(currentVersion, "v"),
		db:             db,
		log:            log.With(slog.String("component", "update_checker")),
		client:         &http.Client{Timeout: 30 * time.Second},
	}
}

// Start launches the background check goroutine. It returns a stop function
// that blocks until the goroutine exits.
func (c *Checker) Start() func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Initial delay before the first check so startup is not slowed.
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-done:
			return
		}

		c.check()

		ticker := time.NewTicker(defaultInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.check()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// GetInfo reads cached update info from the settings table. It never blocks
// on a network call — the background goroutine populates the cache.
func (c *Checker) GetInfo(ctx context.Context) UpdateInfo {
	info := UpdateInfo{CurrentVersion: c.currentVersion}

	version, _ := c.db.GetSetting(ctx, "update_available_version")
	notes, _ := c.db.GetSetting(ctx, "update_available_notes")
	releaseURL, _ := c.db.GetSetting(ctx, "update_available_url")
	checkedAt, _ := c.db.GetSetting(ctx, "update_checked_at")

	if version != "" && isNewer(version, c.currentVersion) {
		info.AvailableVersion = version
		info.ReleaseNotes = notes
		info.ReleaseURL = releaseURL
		info.NeedsUpdate = true
	}
	info.CheckedAt = checkedAt
	return info
}

// check fetches the latest release from GitHub and writes the result to the
// settings table. All errors are logged at Debug level and do not propagate —
// the checker is best-effort.
func (c *Checker) check() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesURL, nil)
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelDebug, "failed to build update check request",
			slog.String("error", err.Error()))
		return
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "VoidLLM/"+c.currentVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelDebug, "update check request failed",
			slog.String("error", err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.LogAttrs(ctx, slog.LevelDebug, "update check returned non-200",
			slog.Int("status", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelDebug, "failed to read update check response",
			slog.String("error", err.Error()))
		return
	}

	var release struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		c.log.LogAttrs(ctx, slog.LevelDebug, "failed to parse update check response",
			slog.String("error", err.Error()))
		return
	}

	version := strings.TrimPrefix(release.TagName, "v")
	now := time.Now().UTC().Format(time.RFC3339)

	if err := c.db.SetSetting(ctx, "update_checked_at", now); err != nil {
		c.log.LogAttrs(ctx, slog.LevelDebug, "failed to store update_checked_at", slog.String("error", err.Error()))
	}

	if isNewer(version, c.currentVersion) {
		if err := c.db.SetSetting(ctx, "update_available_version", version); err != nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "failed to store update version", slog.String("error", err.Error()))
		}
		notes := release.Body
		if len(notes) > 10240 {
			notes = notes[:10240] + "\n\n(truncated)"
		}
		if err := c.db.SetSetting(ctx, "update_available_notes", notes); err != nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "failed to store update notes", slog.String("error", err.Error()))
		}
		if strings.HasPrefix(release.HTMLURL, "https://github.com/") {
			if err := c.db.SetSetting(ctx, "update_available_url", release.HTMLURL); err != nil {
				c.log.LogAttrs(ctx, slog.LevelDebug, "failed to store update URL", slog.String("error", err.Error()))
			}
		}
		c.log.LogAttrs(ctx, slog.LevelInfo, "new version available",
			slog.String("current", c.currentVersion),
			slog.String("available", version),
		)
	} else {
		// Clear any stale update info so GetInfo does not surface an outdated result.
		if err := c.db.SetSetting(ctx, "update_available_version", ""); err != nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "failed to clear update version", slog.String("error", err.Error()))
		}
		if err := c.db.SetSetting(ctx, "update_available_notes", ""); err != nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "failed to clear update notes", slog.String("error", err.Error()))
		}
		if err := c.db.SetSetting(ctx, "update_available_url", ""); err != nil {
			c.log.LogAttrs(ctx, slog.LevelDebug, "failed to clear update URL", slog.String("error", err.Error()))
		}
	}
}

// isNewer reports whether version a is strictly newer than version b.
// Both strings are expected to be bare semver without a leading "v" prefix
// (e.g. "0.1.2"). Comparison is performed component-by-component from
// major to patch; non-numeric components parse as zero.
func isNewer(a, b string) bool {
	if idx := strings.IndexByte(a, '-'); idx >= 0 {
		a = a[:idx]
	}
	if idx := strings.IndexByte(b, '-'); idx >= 0 {
		b = b[:idx]
	}
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		var numA, numB int
		if i < len(partsA) {
			numA, _ = strconv.Atoi(partsA[i])
		}
		if i < len(partsB) {
			numB, _ = strconv.Atoi(partsB[i])
		}
		if numA > numB {
			return true
		}
		if numA < numB {
			return false
		}
	}
	return false
}
