package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// MCPToolCall represents a single MCP tool invocation event for async persistence.
// It is an append-only, fully denormalized record — no foreign keys, survives
// soft-deletes of related entities (same design philosophy as usage_events).
type MCPToolCall struct {
	// ID is the UUIDv7 identifier assigned at insert time.
	ID string
	// RequestID is the per-gateway-request trace identifier.
	RequestID string
	// KeyID is the API key used to authenticate the gateway request.
	KeyID string
	// KeyType is the type of the API key ("user_key", "team_key", or "sa_key").
	KeyType string
	// OrgID is the organization that owns the API key.
	OrgID string
	// TeamID is the team associated with the key; empty for org-level keys.
	TeamID string
	// UserID is the user associated with the key; empty for service accounts.
	UserID string
	// ServiceAccountID is the service account ID; empty for user keys.
	ServiceAccountID string
	// ServerAlias is the stable short name of the MCP server that handled the call.
	ServerAlias string
	// ToolName is the name of the tool that was invoked.
	ToolName string
	// DurationMS is the round-trip duration in milliseconds; nil if not measured.
	DurationMS *int
	// Status is the outcome of the call: "success", "error", or "timeout".
	Status string
	// CodeMode indicates whether this tool call was made inside a Code Mode
	// sandboxed execution session.
	CodeMode bool
	// CreatedAt is the UTC timestamp of the event stored as a string.
	CreatedAt string
}

// InsertMCPToolCalls persists a batch of MCP tool call events in a single
// transaction. Each call in the slice that has an empty ID receives a newly
// generated UUIDv7. The slice must be non-empty; callers should check before
// invoking to avoid an unnecessary transaction.
func (d *DB) InsertMCPToolCalls(ctx context.Context, calls []MCPToolCall) error {
	if len(calls) == 0 {
		return nil
	}

	p := d.dialect.Placeholder
	query := "INSERT INTO mcp_tool_calls " +
		"(id, request_id, key_id, key_type, org_id, team_id, user_id, service_account_id, " +
		"server_alias, tool_name, duration_ms, status, code_mode) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " + p(8) + ", " +
		p(9) + ", " + p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ")"

	if err := d.WithTx(ctx, func(q Querier) error {
		for _, call := range calls {
			id := call.ID
			if id == "" {
				generated, err := uuid.NewV7()
				if err != nil {
					return fmt.Errorf("insert mcp tool calls: generate id: %w", err)
				}
				id = generated.String()
			}

			var teamID, userID, saID any
			if call.TeamID != "" {
				teamID = call.TeamID
			}
			if call.UserID != "" {
				userID = call.UserID
			}
			if call.ServiceAccountID != "" {
				saID = call.ServiceAccountID
			}

			codeMode := 0
			if call.CodeMode {
				codeMode = 1
			}

			_, err := q.ExecContext(ctx, query,
				id,
				call.RequestID,
				call.KeyID,
				call.KeyType,
				call.OrgID,
				teamID,
				userID,
				saID,
				call.ServerAlias,
				call.ToolName,
				call.DurationMS,
				call.Status,
				codeMode,
			)
			if err != nil {
				return fmt.Errorf("insert mcp tool calls: exec: %w", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("insert mcp tool calls: %w", err)
	}

	return nil
}
