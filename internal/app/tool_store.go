package app

import (
	"context"

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

// LoadAll returns all cached tool schemas grouped by server alias. Only tools
// for active, non-deleted servers are returned.
func (s *dbToolStore) LoadAll(ctx context.Context) (map[string][]mcp.Tool, error) {
	dbTools, err := s.db.ListAllServerTools(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]mcp.Tool, len(dbTools))
	for alias, tools := range dbTools {
		mcpTools := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			var schema mcp.InputSchema
			if err := jsonx.Unmarshal([]byte(t.InputSchema), &schema); err != nil {
				continue // skip tools with corrupt schemas
			}
			mcpTools = append(mcpTools, mcp.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
		result[alias] = mcpTools
	}
	return result, nil
}

// Save persists the tool schemas for a server alias, replacing any previous
// entry. It resolves the server ID by alias before writing.
func (s *dbToolStore) Save(ctx context.Context, alias string, tools []mcp.Tool) error {
	server, err := s.db.GetMCPServerByAliasAny(ctx, alias)
	if err != nil {
		return err
	}
	dbTools := make([]db.MCPServerTool, 0, len(tools))
	for _, t := range tools {
		schemaJSON, _ := jsonx.Marshal(t.InputSchema) //nolint:errcheck
		dbTools = append(dbTools, db.MCPServerTool{
			ServerID:    server.ID,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: string(schemaJSON),
		})
	}
	return s.db.UpsertServerTools(ctx, server.ID, dbTools)
}

// Delete removes all cached tool schemas for a server by its database ID.
// Using the ID directly avoids the problem where alias-based lookups fail
// after a server has been soft-deleted or deactivated.
func (s *dbToolStore) Delete(ctx context.Context, serverID string) error {
	return s.db.DeleteServerTools(ctx, serverID)
}
