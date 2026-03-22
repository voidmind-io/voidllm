package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
)

// QueryParams holds filter and pagination parameters for audit log queries.
type QueryParams struct {
	// OrgID filters events to a specific organization. Empty matches all orgs.
	OrgID string
	// ActorID filters events by the actor who performed the action.
	ActorID string
	// ResourceType filters events by the kind of resource affected.
	ResourceType string
	// Action filters events by the action verb (e.g. "create", "delete").
	Action string
	// From is the inclusive start of the time range. Zero value means no lower bound.
	From time.Time
	// To is the inclusive end of the time range. Zero value means no upper bound.
	To time.Time
	// Cursor is the exclusive upper bound for cursor-based pagination. Pass the ID
	// of the last event from the previous page to retrieve the next page.
	Cursor string
	// Limit is the maximum number of events to return. Clamped to [1, 200]. Defaults to 50.
	Limit int
}

// QueryResult holds a page of audit events with cursor pagination metadata.
type QueryResult struct {
	// Events is the page of matching audit events ordered by id descending (newest first).
	Events []Event
	// HasMore is true when there are more events beyond this page.
	HasMore bool
}

// Query retrieves audit log events matching the given filters, ordered by id
// descending (newest first). UUIDv7 IDs are time-sortable, so this ordering
// is equivalent to newest-first by creation time. Limit is clamped to [1, 200]
// and defaults to 50. Cursor enables forward pagination: pass the ID of the
// last event from a previous page to retrieve the next page.
func Query(ctx context.Context, database *db.DB, params QueryParams) (*QueryResult, error) {
	if params.Limit <= 0 {
		params.Limit = 50
	}
	if params.Limit > 200 {
		params.Limit = 200
	}

	filter := db.AuditLogFilter{
		OrgID:        params.OrgID,
		ActorID:      params.ActorID,
		ResourceType: params.ResourceType,
		Action:       params.Action,
		From:         params.From,
		To:           params.To,
		Cursor:       params.Cursor,
		Limit:        params.Limit,
	}

	result, err := database.QueryAuditLogs(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}

	events := make([]Event, len(result.Entries))
	for i, entry := range result.Entries {
		events[i] = Event{
			ID:           entry.ID,
			Timestamp:    entry.Timestamp,
			OrgID:        entry.OrgID,
			ActorID:      entry.ActorID,
			ActorType:    entry.ActorType,
			ActorKeyID:   entry.ActorKeyID,
			Action:       entry.Action,
			ResourceType: entry.ResourceType,
			ResourceID:   entry.ResourceID,
			Description:  entry.Description,
			IPAddress:    entry.IPAddress,
			StatusCode:   entry.StatusCode,
			RequestID:    entry.RequestID,
		}
	}

	return &QueryResult{
		Events:  events,
		HasMore: result.HasMore,
	}, nil
}
