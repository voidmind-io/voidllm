package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
)

// MCPToolCallEvent carries metadata about a single proxied MCP tool call.
// It is logged asynchronously for usage tracking. No prompt or response
// content is included — only structural metadata.
type MCPToolCallEvent struct {
	// KeyID is the ID of the API key that made the call.
	KeyID string
	// KeyType is the category of the key (user_key, team_key, sa_key).
	KeyType string
	// OrgID is the organisation the key belongs to.
	OrgID string
	// TeamID is the team the key is scoped to. Empty if not team-scoped.
	TeamID string
	// UserID is the user associated with the key. Empty if not user-scoped.
	UserID string
	// ServiceAccountID is the SA the key belongs to. Empty if not an SA key.
	ServiceAccountID string
	// ServerAlias identifies the external MCP server that handled the call.
	ServerAlias string
	// ToolName is the MCP tool that was invoked. Empty for non-tool-call methods.
	ToolName string
	// DurationMS is the round-trip call duration in milliseconds.
	DurationMS int
	// Status is "success" or "transport_error".
	Status string
	// RequestID is the per-gateway-request trace identifier.
	RequestID string
	// CodeMode indicates the call originated from a Code Mode sandboxed execution.
	CodeMode bool
	// CodeModeExecutionID groups all tool calls from a single execute_code
	// invocation. Empty for non-Code-Mode calls.
	CodeModeExecutionID string
}

// MCPToolCallLogger logs MCP tool call events asynchronously.
// Implementations must be safe for concurrent use. Log must never block.
type MCPToolCallLogger interface {
	Log(event MCPToolCallEvent)
}

// MCPLogger collects MCP tool call events from the proxy handler and writes
// them to the database in batches. It is safe for concurrent use. The zero
// value is not usable; construct via NewMCPLogger.
type MCPLogger struct {
	events   chan MCPToolCallEvent
	database *db.DB
	log      *slog.Logger
	done     chan struct{}
}

// NewMCPLogger constructs an MCPLogger wired to the given database connection.
// bufferSize controls the event channel capacity; events are dropped when the
// channel is full, matching the fire-and-forget semantics of the usage logger.
// log is used to report flush errors without propagating them to the caller.
// The background goroutine is started immediately inside the constructor.
func NewMCPLogger(database *db.DB, bufferSize int, log *slog.Logger) *MCPLogger {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	l := &MCPLogger{
		events:   make(chan MCPToolCallEvent, bufferSize),
		database: database,
		log:      log,
		done:     make(chan struct{}),
	}
	go l.run()
	return l
}

// Log enqueues an event for async persistence. If the internal buffer is full
// the event is silently dropped to avoid blocking the proxy hot path.
func (l *MCPLogger) Log(event MCPToolCallEvent) {
	select {
	case l.events <- event:
	default:
	}
}

// Stop signals the background goroutine to stop, flushes any remaining events,
// and blocks until the goroutine has fully exited.
func (l *MCPLogger) Stop() {
	close(l.events)
	<-l.done
}

// run is the background flush loop. It accumulates events and flushes them
// either when the batch reaches 100 or when the 5-second ticker fires.
// When the events channel is closed it flushes any remaining events and exits.
func (l *MCPLogger) run() {
	defer close(l.done)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	batch := make([]db.MCPToolCall, 0, 100)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		l.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case event, ok := <-l.events:
			if !ok {
				// Channel closed — flush remaining events and exit.
				flush()
				return
			}
			durationMS := event.DurationMS
			call := db.MCPToolCall{
				KeyID:            event.KeyID,
				KeyType:          event.KeyType,
				OrgID:            event.OrgID,
				TeamID:           event.TeamID,
				UserID:           event.UserID,
				ServiceAccountID: event.ServiceAccountID,
				ServerAlias:      event.ServerAlias,
				ToolName:         event.ToolName,
				DurationMS:       &durationMS,
				Status:           event.Status,
				RequestID:        event.RequestID,
				CodeMode:         event.CodeMode,
			}
			if event.CodeModeExecutionID != "" {
				call.CodeModeExecutionID = &event.CodeModeExecutionID
			}
			batch = append(batch, call)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// flush writes a batch of MCP tool call events to the database.
// Errors are logged but not returned; the caller (run) continues regardless.
func (l *MCPLogger) flush(batch []db.MCPToolCall) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.database.InsertMCPToolCalls(ctx, batch); err != nil {
		l.log.LogAttrs(ctx, slog.LevelError, "mcp logger: flush failed",
			slog.String("error", err.Error()))
	}
}
