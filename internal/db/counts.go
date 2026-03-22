package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CountOrgs returns the number of non-deleted organizations.
func (d *DB) CountOrgs(ctx context.Context) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM organizations WHERE deleted_at IS NULL",
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountOrgs: %w", translateError(err))
	}
	return count, nil
}

// CountActiveKeys returns the number of non-deleted, non-expired API keys for an org.
func (d *DB) CountActiveKeys(ctx context.Context, orgID string) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_keys WHERE org_id = "+d.dialect.Placeholder(1)+
			" AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > "+d.dialect.Placeholder(2)+")",
		orgID, time.Now().UTC().Format(time.RFC3339),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountActiveKeys org %s: %w", orgID, translateError(err))
	}
	return count, nil
}

// CountTeams returns the number of non-deleted teams for an org.
func (d *DB) CountTeams(ctx context.Context, orgID string) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM teams WHERE org_id = "+d.dialect.Placeholder(1)+" AND deleted_at IS NULL",
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountTeams org %s: %w", orgID, translateError(err))
	}
	return count, nil
}

// CountOrgMembers returns the number of members in an org.
func (d *DB) CountOrgMembers(ctx context.Context, orgID string) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM org_memberships WHERE org_id = "+d.dialect.Placeholder(1),
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountOrgMembers org %s: %w", orgID, translateError(err))
	}
	return count, nil
}

// CountTeamKeys returns the number of active, non-expired API keys for a team.
func (d *DB) CountTeamKeys(ctx context.Context, teamID string) (int, error) {
	p := d.dialect.Placeholder
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_keys WHERE team_id = "+p(1)+
			" AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > "+p(2)+")",
		teamID, time.Now().UTC().Format(time.RFC3339),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountTeamKeys team %s: %w", teamID, translateError(err))
	}
	return count, nil
}

// CountTeamMembers returns the number of members in a team.
func (d *DB) CountTeamMembers(ctx context.Context, teamID string) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM team_memberships WHERE team_id = "+d.dialect.Placeholder(1),
		teamID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountTeamMembers team %s: %w", teamID, translateError(err))
	}
	return count, nil
}

// CountUserKeys returns the number of active, non-expired API keys owned by a
// user within an org.
func (d *DB) CountUserKeys(ctx context.Context, orgID, userID string) (int, error) {
	p := d.dialect.Placeholder
	var count int
	err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_keys WHERE org_id = "+p(1)+
			" AND user_id = "+p(2)+
			" AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > "+p(3)+")",
		orgID, userID, time.Now().UTC().Format(time.RFC3339),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountUserKeys org %s user %s: %w", orgID, userID, translateError(err))
	}
	return count, nil
}

// GetUserTeamID returns the ID of the first team a user belongs to within an org.
// Returns an empty string with no error when the user has no team membership.
func (d *DB) GetUserTeamID(ctx context.Context, orgID, userID string) (string, error) {
	p := d.dialect.Placeholder
	query := "SELECT t.id FROM teams t" +
		" JOIN team_memberships tm ON tm.team_id = t.id" +
		" WHERE t.org_id = " + p(1) +
		" AND tm.user_id = " + p(2) +
		" AND t.deleted_at IS NULL ORDER BY t.created_at LIMIT 1"

	var teamID string
	err := d.sql.QueryRowContext(ctx, query, orgID, userID).Scan(&teamID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("GetUserTeamID org %s user %s: %w", orgID, userID, translateError(err))
	}
	return teamID, nil
}
