package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// newTestDB opens an in-memory SQLite database for testing.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file::memory:?cache=shared&mode=memory",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	database, err := db.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// newTestApp builds a Fiber app with all health routes mounted.
// checker may be nil, in which case no drain check is performed.
func newTestApp(database *db.DB, checker ShutdownChecker) *fiber.App {
	app := fiber.New()
	app.Get("/healthz", Liveness())
	app.Get("/health", Liveness())
	app.Get("/readyz", Readiness(database, checker))
	app.Get("/metrics", Metrics())
	return app
}

func TestLiveness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{name: "/healthz returns 200", path: "/healthz", wantOK: true},
		{name: "/health alias returns 200", path: "/health", wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			database := newTestDB(t)
			app := newTestApp(database, nil)

			req := httptest.NewRequest("GET", tc.path, nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}

			if resp.StatusCode != fiber.StatusOK {
				t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}

			if got, ok := payload["status"]; !ok || got != "ok" {
				t.Errorf("status field = %v, want \"ok\"", got)
			}

			version, ok := payload["version"]
			if !ok {
				t.Error("response missing \"version\" field")
			} else if v, _ := version.(string); v == "" {
				t.Error("\"version\" field is empty")
			}

			uptime, ok := payload["uptime_seconds"]
			if !ok {
				t.Error("response missing \"uptime_seconds\" field")
			} else {
				// JSON numbers unmarshal as float64.
				if v, _ := uptime.(float64); v < 0 {
					t.Errorf("\"uptime_seconds\" = %v, want >= 0", v)
				}
			}
		})
	}
}

// drainingChecker is a test implementation of ShutdownChecker that always
// reports the server as draining.
type drainingChecker struct{}

func (d drainingChecker) Draining() bool { return true }

func TestReadiness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		setupDB        func(t *testing.T) *db.DB
		checker        ShutdownChecker
		wantStatus     int
		wantBodyStatus string
		wantDatabase   string
	}{
		{
			name: "healthy DB returns 200 ok",
			setupDB: func(t *testing.T) *db.DB {
				t.Helper()
				return newTestDB(t)
			},
			checker:        nil,
			wantStatus:     fiber.StatusOK,
			wantBodyStatus: "ok",
			wantDatabase:   "ok",
		},
		{
			name: "closed DB returns 503 not ready",
			setupDB: func(t *testing.T) *db.DB {
				t.Helper()
				database := newTestDB(t)
				// Close the DB before the request so Ping fails.
				if err := database.Close(); err != nil {
					t.Fatalf("close DB: %v", err)
				}
				// Cancel the cleanup registered by newTestDB — double-close is
				// harmless for SQLite but we keep it tidy.
				return database
			},
			checker:        nil,
			wantStatus:     fiber.StatusServiceUnavailable,
			wantBodyStatus: "not ready",
			wantDatabase:   "", // field is "error", we only check status here
		},
		{
			name: "draining server returns 503 draining",
			setupDB: func(t *testing.T) *db.DB {
				t.Helper()
				return newTestDB(t)
			},
			checker:        drainingChecker{},
			wantStatus:     fiber.StatusServiceUnavailable,
			wantBodyStatus: "draining",
			wantDatabase:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			database := tc.setupDB(t)
			app := newTestApp(database, tc.checker)

			req := httptest.NewRequest("GET", "/readyz", nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}

			if got, _ := payload["status"].(string); got != tc.wantBodyStatus {
				t.Errorf("status field = %q, want %q", got, tc.wantBodyStatus)
			}

			if tc.wantDatabase != "" {
				if got, _ := payload["database"].(string); got != tc.wantDatabase {
					t.Errorf("database field = %q, want %q", got, tc.wantDatabase)
				}
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	app := newTestApp(database, nil)

	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want it to contain \"text/plain\"", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !strings.Contains(string(body), "go_goroutines") {
		t.Error("metrics body does not contain \"go_goroutines\"")
	}
}
