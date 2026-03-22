package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditLogFilter holds filter and pagination parameters for querying audit logs.
// All string fields are optional equality filters; an empty string means no
// filter is applied for that field. Zero-value time.Time fields mean no bound.
type AuditLogFilter struct {
	// OrgID restricts results to a specific organization. Empty matches all orgs.
	OrgID string
	// ActorID restricts results to a specific actor. Empty matches all actors.
	ActorID string
	// ResourceType restricts results to a specific resource kind. Empty matches all.
	ResourceType string
	// Action restricts results to a specific action verb. Empty matches all.
	Action string
	// From is the inclusive lower bound on the event timestamp. Zero means no lower bound.
	From time.Time
	// To is the inclusive upper bound on the event timestamp. Zero means no upper bound.
	To time.Time
	// Cursor is the exclusive upper bound for cursor-based pagination. When non-empty,
	// only entries with id < cursor are returned, enabling newest-first traversal.
	Cursor string
	// Limit is the maximum number of entries to return. Must be in [1, 200]; the
	// caller is responsible for clamping before passing to QueryAuditLogs.
	Limit int
}

// AuditLogEntry represents a single audit log row from the database.
type AuditLogEntry struct {
	// ID is the UUIDv7 identifier of the audit log row.
	ID string
	// Timestamp is when the action occurred, in UTC.
	Timestamp time.Time
	// OrgID is the organization context in which the action was performed.
	OrgID string
	// ActorID is the user or service account ID that performed the action.
	ActorID string
	// ActorType describes the kind of actor: "user", "service_account", or "system".
	ActorType string
	// ActorKeyID is the api_keys.id of the key used for the action.
	ActorKeyID string
	// Action is the verb describing what occurred.
	Action string
	// ResourceType identifies the kind of resource affected.
	ResourceType string
	// ResourceID is the identifier of the affected resource.
	ResourceID string
	// Description is a human-readable summary of the action.
	Description string
	// IPAddress is the client IP address extracted from the request.
	IPAddress string
	// RequestID is the per-request trace ID correlating the audit record with
	// the proxy access log and usage record.
	RequestID string
	// StatusCode is the HTTP status code returned by the handler.
	StatusCode int
}

// AuditLogResult holds a page of audit log entries with cursor pagination metadata.
type AuditLogResult struct {
	// Entries is the page of matching audit log entries ordered by id descending (newest first).
	Entries []AuditLogEntry
	// HasMore is true when there are more entries beyond this page.
	HasMore bool
}

// QueryAuditLogs retrieves audit log entries matching the given filter with
// cursor-based pagination, ordered by id descending (newest first). UUIDv7 IDs
// are time-sortable, so descending id order is equivalent to newest-first by
// creation time. When filter.Cursor is non-empty, only entries with id strictly
// less than the cursor are returned. The filter's Limit must already be clamped
// to a valid range by the caller. One extra row is fetched to determine HasMore;
// the returned Entries slice is always truncated to Limit.
func (d *DB) QueryAuditLogs(ctx context.Context, filter AuditLogFilter) (*AuditLogResult, error) {
	p := d.dialect.Placeholder

	// Build the WHERE clause dynamically from non-zero filter fields.
	// n tracks the current positional argument index (1-based).
	var conditions []string
	var args []any
	n := 1

	if filter.OrgID != "" {
		conditions = append(conditions, "org_id = "+p(n))
		args = append(args, filter.OrgID)
		n++
	}
	if filter.ActorID != "" {
		conditions = append(conditions, "actor_id = "+p(n))
		args = append(args, filter.ActorID)
		n++
	}
	if filter.ResourceType != "" {
		conditions = append(conditions, "resource_type = "+p(n))
		args = append(args, filter.ResourceType)
		n++
	}
	if filter.Action != "" {
		conditions = append(conditions, "action = "+p(n))
		args = append(args, filter.Action)
		n++
	}
	if !filter.From.IsZero() {
		conditions = append(conditions, "timestamp >= "+p(n))
		args = append(args, filter.From.UTC().Format(time.RFC3339))
		n++
	}
	if !filter.To.IsZero() {
		conditions = append(conditions, "timestamp <= "+p(n))
		args = append(args, filter.To.UTC().Format(time.RFC3339))
		n++
	}
	if filter.Cursor != "" {
		conditions = append(conditions, "id < "+p(n))
		args = append(args, filter.Cursor)
		n++
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Fetch limit+1 rows so we can detect whether a next page exists without
	// issuing a separate COUNT query.
	fetchLimit := filter.Limit + 1
	pageArgs := make([]any, len(args), len(args)+1)
	copy(pageArgs, args)
	pageArgs = append(pageArgs, fetchLimit)

	dataQuery := "SELECT id, timestamp, org_id, actor_id, actor_type, actor_key_id, " +
		"action, resource_type, resource_id, description, ip_address, request_id, status_code " +
		"FROM audit_logs" + where +
		" ORDER BY id DESC" +
		" LIMIT " + p(n)

	rows, err := d.sql.QueryContext(ctx, dataQuery, pageArgs...)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: select: %w", err)
	}
	defer rows.Close()

	entries := make([]AuditLogEntry, 0, fetchLimit)
	for rows.Next() {
		var entry AuditLogEntry
		var tsRaw string
		if err := rows.Scan(
			&entry.ID,
			&tsRaw,
			&entry.OrgID,
			&entry.ActorID,
			&entry.ActorType,
			&entry.ActorKeyID,
			&entry.Action,
			&entry.ResourceType,
			&entry.ResourceID,
			&entry.Description,
			&entry.IPAddress,
			&entry.RequestID,
			&entry.StatusCode,
		); err != nil {
			return nil, fmt.Errorf("query audit logs: scan row: %w", err)
		}
		ts, err := time.Parse(time.RFC3339, tsRaw)
		if err != nil {
			return nil, fmt.Errorf("query audit logs: parse timestamp %q: %w", tsRaw, err)
		}
		entry.Timestamp = ts
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query audit logs: iterate rows: %w", err)
	}

	hasMore := len(entries) > filter.Limit
	if hasMore {
		entries = entries[:filter.Limit]
	}

	return &AuditLogResult{
		Entries: entries,
		HasMore: hasMore,
	}, nil
}
