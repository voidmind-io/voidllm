package db

import (
	"context"
	"fmt"
	"strings"
)

// labelResolveChunkSize is the maximum number of IDs processed per IN-list query.
// Keeping this below the SQLite bind-parameter limit (~999) avoids driver errors at
// high cardinality while still requiring very few round-trips in practice.
const labelResolveChunkSize = 500

// ResolveGroupLabels returns a map from entity ID to a human-readable display
// label for the given usage groupBy dimension. The resolvable dimensions are
// "key", "user", "team", "org", and "service_account"; for any other dimension
// (model/day/hour/server/tool/status/"") it returns an empty, non-nil map.
// Soft-deleted rows are intentionally included so historical usage still resolves
// to a name. IDs not found are simply absent from the returned map.
//
// This is an UNSCOPED global lookup by id. Callers MUST pass only IDs already
// constrained to the caller's authorized scope — this function does not enforce
// tenant or org boundaries itself.
func (d *DB) ResolveGroupLabels(ctx context.Context, groupBy string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}

	// Map groupBy to a hardcoded (table, labelExpr) pair.
	// User input never reaches the query string — only the switch selects the
	// expressions, and IDs are always passed as bind parameters.
	var table, labelExpr string
	switch groupBy {
	case "key":
		table = "api_keys"
		labelExpr = "CASE WHEN name != '' THEN name ELSE key_hint END"
	case "user":
		table = "users"
		labelExpr = "display_name"
	case "team":
		table = "teams"
		labelExpr = "name"
	case "org":
		table = "organizations"
		labelExpr = "name"
	case "service_account":
		table = "service_accounts"
		labelExpr = "name"
	default:
		// Non-resolvable dimension (model, day, hour, server, tool, status, "").
		return map[string]string{}, nil
	}

	// De-duplicate and strip empty-string IDs (the coalesced "" placeholder for
	// NULL group columns) — there is no row in any entity table with id = "".
	filtered := deduplicateIDs(ids)

	if len(filtered) == 0 {
		return map[string]string{}, nil
	}

	result := make(map[string]string, len(filtered))

	// Process IDs in chunks to stay below the DB bind-parameter limit.
	for start := 0; start < len(filtered); start += labelResolveChunkSize {
		end := start + labelResolveChunkSize
		if end > len(filtered) {
			end = len(filtered)
		}
		chunk := filtered[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = d.dialect.Placeholder(i + 1)
			args[i] = id
		}

		query := "SELECT id, " + labelExpr +
			" FROM " + table +
			" WHERE id IN (" + strings.Join(placeholders, ", ") + ")"

		if err := d.queryLabelsInto(ctx, result, "ResolveGroupLabels "+groupBy, query, args...); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// ResolveOrgActorLabels resolves audit actor IDs to display labels, restricted to
// actors that belong to the given organization. User IDs resolve to
// users.display_name only when the user is a member of orgID (via org_memberships);
// service-account IDs resolve to service_accounts.name only when scoped to orgID.
// IDs outside the org are simply absent from the returned map. Chunked like
// ResolveGroupLabels. Returns a non-nil map.
//
// This enforces tenant boundaries: only actors that belong to orgID are resolved.
// It is safe to call with IDs collected from a cross-org audit query provided by
// a system_admin; for org_admin callers pass only their own orgID.
func (d *DB) ResolveOrgActorLabels(ctx context.Context, orgID string, userIDs, saIDs []string) (map[string]string, error) {
	result := make(map[string]string)

	// Resolve user IDs scoped to org via membership join.
	filteredUsers := deduplicateIDs(userIDs)
	for start := 0; start < len(filteredUsers); start += labelResolveChunkSize {
		end := start + labelResolveChunkSize
		if end > len(filteredUsers) {
			end = len(filteredUsers)
		}
		chunk := filteredUsers[start:end]

		// orgID occupies position 1; user IDs follow from position 2.
		placeholders := make([]string, len(chunk))
		args := make([]any, 1+len(chunk))
		args[0] = orgID
		for i, id := range chunk {
			placeholders[i] = d.dialect.Placeholder(i + 2)
			args[i+1] = id
		}

		query := "SELECT u.id, u.display_name" +
			" FROM users u" +
			" JOIN org_memberships m ON m.user_id = u.id" +
			" WHERE m.org_id = " + d.dialect.Placeholder(1) +
			" AND u.id IN (" + strings.Join(placeholders, ", ") + ")"

		if err := d.queryLabelsInto(ctx, result, "ResolveOrgActorLabels", query, args...); err != nil {
			return nil, err
		}
	}

	// Resolve service-account IDs scoped to org.
	filteredSAs := deduplicateIDs(saIDs)
	for start := 0; start < len(filteredSAs); start += labelResolveChunkSize {
		end := start + labelResolveChunkSize
		if end > len(filteredSAs) {
			end = len(filteredSAs)
		}
		chunk := filteredSAs[start:end]

		// orgID occupies position 1; SA IDs follow from position 2.
		placeholders := make([]string, len(chunk))
		args := make([]any, 1+len(chunk))
		args[0] = orgID
		for i, id := range chunk {
			placeholders[i] = d.dialect.Placeholder(i + 2)
			args[i+1] = id
		}

		query := "SELECT id, name" +
			" FROM service_accounts" +
			" WHERE org_id = " + d.dialect.Placeholder(1) +
			" AND id IN (" + strings.Join(placeholders, ", ") + ")"

		if err := d.queryLabelsInto(ctx, result, "ResolveOrgActorLabels", query, args...); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// queryLabelsInto runs query with args, scanning each (id, label) row into dst.
// It closes its rows before returning so chunked callers do not accumulate open
// result sets across iterations. errCtx is prepended to any error message.
func (d *DB) queryLabelsInto(ctx context.Context, dst map[string]string, errCtx, query string, args ...any) error {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", errCtx, err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("%s: %w", errCtx, err)
		}
		dst[id] = label
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", errCtx, err)
	}
	return nil
}

// deduplicateIDs returns a new slice with empty strings removed and duplicates
// eliminated, preserving first-occurrence order.
func deduplicateIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
