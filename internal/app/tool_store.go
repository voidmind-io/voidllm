package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// dbToolStore implements mcp.ToolStore using the database layer. It bridges
// the mcp package (which must not import db) to the DB methods for persisting
// and loading tool schemas.
type dbToolStore struct {
	db *db.DB
}

// LoadAll returns all cached tool schemas grouped by server ID. Only tools
// for active, non-deleted servers are returned.
func (s *dbToolStore) LoadAll(ctx context.Context) (map[string][]mcp.Tool, error) {
	dbTools, err := s.db.ListAllServerTools(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]mcp.Tool, len(dbTools))
	for serverID, tools := range dbTools {
		mcpTools := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			var schema mcp.JSONSchema
			if err := jsonx.Unmarshal([]byte(t.InputSchema), &schema); err != nil {
				// A corrupt row must not abort loading every other server's
				// tools, so this tool alone is skipped — but silently, that
				// is exactly how a tool can vanish across a restart without
				// anyone noticing (see FIX 9 / tool_store_test.go). Log it
				// instead of ignoring it outright.
				slog.Default().LogAttrs(ctx, slog.LevelError, "tool_store: skipping tool with corrupt cached schema",
					slog.String("server_id", serverID),
					slog.String("tool", t.Name),
					slog.String("error", err.Error()),
				)
				continue
			}
			mcpTools = append(mcpTools, mcp.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
		result[serverID] = mcpTools
	}
	return result, nil
}

// Save persists the tool schemas for a server by its database ID, replacing
// any previous entry. The serverID is used directly without alias resolution.
// Returns an error, writing nothing, if any tool's InputSchema fails to
// marshal — silently writing an empty schema for that one tool instead would
// make it vanish after the next restart (LoadAll would then fail to parse
// the empty string this wrote and skip it too), with no diagnostic at either
// end of the round trip.
func (s *dbToolStore) Save(ctx context.Context, serverID string, tools []mcp.Tool) error {
	dbTools := make([]db.MCPServerTool, 0, len(tools))
	for _, t := range tools {
		schemaJSON, err := jsonx.Marshal(t.InputSchema)
		if err != nil {
			return fmt.Errorf("tool_store: save server %s: marshal schema for tool %q: %w", serverID, t.Name, err)
		}
		dbTools = append(dbTools, db.MCPServerTool{
			ServerID:    serverID,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: string(schemaJSON),
		})
	}
	return s.db.UpsertServerTools(ctx, serverID, dbTools)
}

// Delete removes all cached tool schemas for a server by its database ID.
// Using the ID directly avoids the problem where alias-based lookups fail
// after a server has been soft-deleted or deactivated.
func (s *dbToolStore) Delete(ctx context.Context, serverID string) error {
	return s.db.DeleteServerTools(ctx, serverID)
}
