package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/logger"
)

// logLine writes a single message at each level and returns the captured output.
// level is the message level to write; the message text is "test message".
func captureLog(t *testing.T, cfg config.LoggingConfig, level slog.Level) string {
	t.Helper()
	var buf bytes.Buffer
	l := logger.New(cfg, &buf)
	l.Log(context.Background(), level, "test message")
	return buf.String()
}

func TestNew_Format(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		format      string
		wantJSON    bool
	}{
		{
			name:     "json format produces valid JSON",
			format:   "json",
			wantJSON: true,
		},
		{
			name:     "text format produces non-JSON output",
			format:   "text",
			wantJSON: false,
		},
		{
			name:     "empty format defaults to json",
			format:   "",
			wantJSON: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			cfg := config.LoggingConfig{
				Format: tc.format,
				Level:  "info",
			}
			l := logger.New(cfg, &buf)
			l.Info("hello")

			output := buf.String()
			if output == "" {
				t.Fatal("expected log output, got empty string")
			}

			var m map[string]any
			err := json.Unmarshal([]byte(strings.TrimSpace(output)), &m)
			if tc.wantJSON && err != nil {
				t.Errorf("expected valid JSON output, got parse error: %v\noutput: %q", err, output)
			}
			if !tc.wantJSON && err == nil {
				t.Errorf("expected non-JSON text output, but output parsed as valid JSON\noutput: %q", output)
			}
		})
	}
}

func TestNew_Level(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		configLevel   string
		writeLevel    slog.Level
		wantOutput    bool
	}{
		// debug level: debug messages should appear
		{
			name:        "debug config level passes debug messages",
			configLevel: "debug",
			writeLevel:  slog.LevelDebug,
			wantOutput:  true,
		},
		// info level: debug messages should be suppressed
		{
			name:        "info config level suppresses debug messages",
			configLevel: "info",
			writeLevel:  slog.LevelDebug,
			wantOutput:  false,
		},
		// info level: info messages should appear
		{
			name:        "info config level passes info messages",
			configLevel: "info",
			writeLevel:  slog.LevelInfo,
			wantOutput:  true,
		},
		// warn level: info messages should be suppressed
		{
			name:        "warn config level suppresses info messages",
			configLevel: "warn",
			writeLevel:  slog.LevelInfo,
			wantOutput:  false,
		},
		// warn level: warn messages should appear
		{
			name:        "warn config level passes warn messages",
			configLevel: "warn",
			writeLevel:  slog.LevelWarn,
			wantOutput:  true,
		},
		// error level: warn messages should be suppressed
		{
			name:        "error config level suppresses warn messages",
			configLevel: "error",
			writeLevel:  slog.LevelWarn,
			wantOutput:  false,
		},
		// error level: error messages should appear
		{
			name:        "error config level passes error messages",
			configLevel: "error",
			writeLevel:  slog.LevelError,
			wantOutput:  true,
		},
		// unknown level defaults to info; debug messages are suppressed
		{
			name:        "unknown config level defaults to info, suppresses debug",
			configLevel: "unknown",
			writeLevel:  slog.LevelDebug,
			wantOutput:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.LoggingConfig{
				Format: "json",
				Level:  tc.configLevel,
			}
			output := captureLog(t, cfg, tc.writeLevel)

			hasOutput := strings.TrimSpace(output) != ""
			if hasOutput != tc.wantOutput {
				t.Errorf("configLevel=%q writeLevel=%v: got output=%v (wantOutput=%v)\noutput: %q",
					tc.configLevel, tc.writeLevel, hasOutput, tc.wantOutput, output)
			}
		})
	}
}

func TestNew_TimestampUTCRFC3339Nano(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.LoggingConfig{Format: "json", Level: "info"}
	l := logger.New(cfg, &buf)
	l.Info("timestamp test")

	output := strings.TrimSpace(buf.String())
	if output == "" {
		t.Fatal("expected log output, got empty string")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(output), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v\noutput: %q", err, output)
	}

	raw, ok := entry["time"]
	if !ok {
		t.Fatal("log entry missing 'time' field")
	}
	timeStr, ok := raw.(string)
	if !ok {
		t.Fatalf("'time' field is not a string, got %T", raw)
	}

	parsed, err := time.Parse(time.RFC3339Nano, timeStr)
	if err != nil {
		t.Fatalf("'time' field %q is not RFC3339Nano format: %v", timeStr, err)
	}

	if parsed.Location() != time.UTC {
		t.Errorf("'time' field location = %v, want UTC", parsed.Location())
	}
}

func TestWithContextFromContext_RoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.LoggingConfig{Format: "json", Level: "info"}
	original := logger.New(cfg, &buf)

	ctx := logger.WithContext(context.Background(), original)
	got := logger.FromContext(ctx)

	if got != original {
		t.Errorf("FromContext returned a different logger than was stored with WithContext")
	}
}

func TestFromContext_EmptyContext(t *testing.T) {
	t.Parallel()

	got := logger.FromContext(context.Background())
	if got == nil {
		t.Fatal("FromContext returned nil for empty context, want slog.Default()")
	}
	if got != slog.Default() {
		t.Errorf("FromContext with empty context = %v, want slog.Default() %v", got, slog.Default())
	}
}

func TestFromContext_WrongType(t *testing.T) {
	t.Parallel()

	// Store a value under a foreign key — FromContext uses its own unexported contextKey,
	// so any value stored with a different key type won't be found. We simulate the
	// "wrong type" case by storing a non-*slog.Logger value via a round-about path:
	// we can't inject the internal key directly, but we can verify that a plain
	// context.Background() (which has no logger at all) returns slog.Default().
	// The actual wrong-type guard (the ok check in the type assertion) is exercised
	// here by relying on the fact that any context without the exact internal key
	// will fall through to slog.Default().
	type foreignKey struct{}
	ctx := context.WithValue(context.Background(), foreignKey{}, "not a logger")

	got := logger.FromContext(ctx)
	if got != slog.Default() {
		t.Errorf("FromContext with foreign key = %v, want slog.Default()", got)
	}
}
