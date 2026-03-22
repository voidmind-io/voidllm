// Package logger provides structured logging utilities for VoidLLM.
package logger

import (
	"context"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/voidmind-io/voidllm/internal/config"
)

// RequestIDHandler wraps a slog.Handler and automatically adds a request_id
// attribute to every log record when a request ID is present in the context.
// Background goroutines using context.Background() will not have a request ID,
// which is the correct behavior.
type RequestIDHandler struct {
	inner   slog.Handler
	extract func(context.Context) string
}

// NewRequestIDHandler creates a handler wrapper that injects request IDs from
// the context into log records. Both inner and extract must be non-nil.
func NewRequestIDHandler(inner slog.Handler, extract func(context.Context) string) *RequestIDHandler {
	return &RequestIDHandler{inner: inner, extract: extract}
}

// Enabled reports whether the inner handler will handle records at the given level.
func (h *RequestIDHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle appends a request_id attribute when one is present in ctx, then
// injects trace_id and span_id when the context carries an active OTel span,
// before delegating to the inner handler. This correlates log lines with
// distributed traces in backends such as Grafana Tempo or Jaeger.
func (h *RequestIDHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := h.extract(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, record)
}

// WithAttrs returns a new RequestIDHandler whose inner handler has the given
// attributes pre-applied.
func (h *RequestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RequestIDHandler{inner: h.inner.WithAttrs(attrs), extract: h.extract}
}

// WithGroup returns a new RequestIDHandler whose inner handler uses the given
// group name for subsequent attributes.
func (h *RequestIDHandler) WithGroup(name string) slog.Handler {
	return &RequestIDHandler{inner: h.inner.WithGroup(name), extract: h.extract}
}

// contextKey is an unexported type used as the key for storing a logger in a
// context.Context. Using a struct prevents collisions with keys from other packages.
type contextKey struct{}

// New constructs a *slog.Logger configured according to cfg, writing log output to w.
// Pass os.Stdout for production use.
// AddSource is enabled only at debug level to avoid the overhead in production.
// All timestamps are normalized to UTC RFC3339Nano format.
func New(cfg config.LoggingConfig, w io.Writer) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: level == slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}

	return slog.New(handler)
}

// WithContext returns a copy of ctx with the given logger stored inside it.
// Retrieve the logger later with FromContext.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// FromContext retrieves the logger stored in ctx by WithContext.
// If no logger has been stored, it returns slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
