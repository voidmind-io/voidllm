package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"log/slog"
	"os"
)

// mapSettings is a simple in-memory SettingsReadWriter backed by a map.
type mapSettings struct {
	mu   sync.RWMutex
	data map[string]string
}

func newMapSettings() *mapSettings {
	return &mapSettings{data: make(map[string]string)}
}

func (m *mapSettings) GetSetting(_ context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.data[key], nil
}

func (m *mapSettings) SetSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

// --- isNewer tests ---

func TestIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.0.14", "0.0.13", true},
		{"0.0.13", "0.0.14", false},
		{"0.1.0", "0.0.99", true},
		{"0.0.99", "0.1.0", false},
		{"1.0.0", "0.9.9", true},
		{"0.9.9", "1.0.0", false},
		{"0.0.14", "0.0.14", false},
		{"1.0.0", "1.0.0", false},
		{"0.1.0", "0.1.0", false},
		// Shorter vs longer version strings
		{"1.0", "0.9.9", true},
		{"0.9", "1.0.0", false},
		// Pre-release / non-numeric treated as zero
		{"0.1.0", "0.0.0", true},
	}
	for _, tc := range cases {
		got := isNewer(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- GetInfo tests ---

func TestGetInfo_NoUpdate(t *testing.T) {
	store := newMapSettings()
	c := NewChecker("0.1.0", store, discardLogger())

	info := c.GetInfo(context.Background())
	if info.CurrentVersion != "0.1.0" {
		t.Errorf("expected current_version 0.1.0, got %s", info.CurrentVersion)
	}
	if info.NeedsUpdate {
		t.Error("expected NeedsUpdate false when no version stored")
	}
	if info.AvailableVersion != "" {
		t.Errorf("expected empty available_version, got %s", info.AvailableVersion)
	}
}

func TestGetInfo_UpdateAvailable(t *testing.T) {
	store := newMapSettings()
	ctx := context.Background()
	_ = store.SetSetting(ctx, "update_available_version", "0.2.0")
	_ = store.SetSetting(ctx, "update_available_notes", "## What's new\n- Feature A")
	_ = store.SetSetting(ctx, "update_available_url", "https://github.com/voidmind-io/voidllm/releases/tag/v0.2.0")
	_ = store.SetSetting(ctx, "update_checked_at", "2026-04-03T00:00:00Z")

	c := NewChecker("0.1.0", store, discardLogger())
	info := c.GetInfo(ctx)

	if !info.NeedsUpdate {
		t.Error("expected NeedsUpdate true")
	}
	if info.AvailableVersion != "0.2.0" {
		t.Errorf("expected available_version 0.2.0, got %s", info.AvailableVersion)
	}
	if info.ReleaseNotes != "## What's new\n- Feature A" {
		t.Errorf("unexpected release_notes: %s", info.ReleaseNotes)
	}
	if info.ReleaseURL != "https://github.com/voidmind-io/voidllm/releases/tag/v0.2.0" {
		t.Errorf("unexpected release_url: %s", info.ReleaseURL)
	}
	if info.CheckedAt != "2026-04-03T00:00:00Z" {
		t.Errorf("unexpected checked_at: %s", info.CheckedAt)
	}
}

func TestGetInfo_StaleUpdateNotSurfaced(t *testing.T) {
	// Store a version that is older than current — should not surface as update.
	store := newMapSettings()
	ctx := context.Background()
	_ = store.SetSetting(ctx, "update_available_version", "0.0.5")

	c := NewChecker("0.1.0", store, discardLogger())
	info := c.GetInfo(ctx)

	if info.NeedsUpdate {
		t.Error("expected NeedsUpdate false for stale stored version")
	}
	if info.AvailableVersion != "" {
		t.Errorf("expected empty available_version for stale version, got %s", info.AvailableVersion)
	}
}

func TestGetInfo_VPrefixStripped(t *testing.T) {
	// NewChecker should strip the "v" prefix from currentVersion.
	store := newMapSettings()
	c := NewChecker("v0.1.0", store, discardLogger())
	if c.currentVersion != "0.1.0" {
		t.Errorf("expected currentVersion 0.1.0, got %s", c.currentVersion)
	}
}

// --- check (HTTP) tests ---

type githubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

func newMockGitHub(t *testing.T, statusCode int, release githubRelease) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/voidmind-io/voidllm/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if statusCode == http.StatusOK {
			_ = json.NewEncoder(w).Encode(release)
		}
	}))
}

func checkerWithURL(currentVersion string, store SettingsReadWriter, log *slog.Logger, serverURL string) *Checker {
	c := NewChecker(currentVersion, store, log)
	// Point the checker at the test server by overriding the URL via a round-tripper shim.
	c.client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{base: http.DefaultTransport, replace: serverURL},
	}
	return c
}

// rewriteTransport rewrites all requests to point at replace instead of the
// original githubReleasesURL host+path. This lets tests use a real Checker
// against a local httptest server without exporting the URL constant.
type rewriteTransport struct {
	base    http.RoundTripper
	replace string // base URL of test server, e.g. "http://127.0.0.1:PORT"
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = rt.replace[len("http://"):]
	return rt.base.RoundTrip(cloned)
}

func TestCheck_NewVersionWritesToSettings(t *testing.T) {
	release := githubRelease{
		TagName: "v0.2.0",
		Body:    "release notes",
		HTMLURL: "https://github.com/voidmind-io/voidllm/releases/tag/v0.2.0",
	}
	srv := newMockGitHub(t, http.StatusOK, release)
	defer srv.Close()

	store := newMapSettings()
	c := checkerWithURL("0.1.0", store, discardLogger(), srv.URL)
	c.check()

	ctx := context.Background()
	ver, _ := store.GetSetting(ctx, "update_available_version")
	notes, _ := store.GetSetting(ctx, "update_available_notes")
	releaseURL, _ := store.GetSetting(ctx, "update_available_url")
	checkedAt, _ := store.GetSetting(ctx, "update_checked_at")

	if ver != "0.2.0" {
		t.Errorf("expected update_available_version 0.2.0, got %q", ver)
	}
	if notes != "release notes\n" && notes != "release notes" {
		// json.Encoder adds a newline; accept both forms.
		if notes != "release notes\n" {
			t.Errorf("unexpected update_available_notes: %q", notes)
		}
	}
	if releaseURL != release.HTMLURL {
		t.Errorf("expected update_available_url %q, got %q", release.HTMLURL, releaseURL)
	}
	if checkedAt == "" {
		t.Error("expected update_checked_at to be set")
	}
}

func TestCheck_UpToDateClearsStaleSettings(t *testing.T) {
	// Server reports same version as current — stale settings should be cleared.
	release := githubRelease{
		TagName: "v0.1.0",
		Body:    "",
		HTMLURL: "https://github.com/voidmind-io/voidllm/releases/tag/v0.1.0",
	}
	srv := newMockGitHub(t, http.StatusOK, release)
	defer srv.Close()

	store := newMapSettings()
	ctx := context.Background()
	// Pre-populate stale data.
	_ = store.SetSetting(ctx, "update_available_version", "0.0.9")
	_ = store.SetSetting(ctx, "update_available_notes", "old notes")
	_ = store.SetSetting(ctx, "update_available_url", "https://old-url")

	c := checkerWithURL("0.1.0", store, discardLogger(), srv.URL)
	c.check()

	ver, _ := store.GetSetting(ctx, "update_available_version")
	if ver != "" {
		t.Errorf("expected empty update_available_version after up-to-date check, got %q", ver)
	}
	notes, _ := store.GetSetting(ctx, "update_available_notes")
	if notes != "" {
		t.Errorf("expected empty update_available_notes after up-to-date check, got %q", notes)
	}
}

func TestCheck_Non200DoesNotWriteSettings(t *testing.T) {
	srv := newMockGitHub(t, http.StatusNotFound, githubRelease{})
	defer srv.Close()

	store := newMapSettings()
	c := checkerWithURL("0.1.0", store, discardLogger(), srv.URL)
	c.check()

	ctx := context.Background()
	ver, _ := store.GetSetting(ctx, "update_available_version")
	checkedAt, _ := store.GetSetting(ctx, "update_checked_at")
	if ver != "" {
		t.Errorf("expected no update_available_version on 404, got %q", ver)
	}
	if checkedAt != "" {
		t.Errorf("expected no update_checked_at on 404, got %q", checkedAt)
	}
}

func TestCheck_ServerUnavailableDoesNotPanic(t *testing.T) {
	// Use a closed server to simulate a network error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	store := newMapSettings()
	c := checkerWithURL("0.1.0", store, discardLogger(), srv.URL)
	// Must not panic or block.
	c.check()
}
