// Package db provides database access primitives, dialect abstraction,
// transaction helpers, and the embedded migration runner for VoidLLM.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when an insert or update violates a uniqueness constraint.
var ErrConflict = errors.New("conflict")

// ErrNoPassword is returned when a user has no password hash (SSO-only account).
var ErrNoPassword = errors.New("no password")

// ErrForeignKey is returned when an insert or update violates a foreign key constraint.
// It indicates that a referenced record (e.g. organization) does not exist.
var ErrForeignKey = errors.New("foreign key violation")

// ErrReservedValue is returned when a caller-supplied value for a unique
// column (model/deployment name, user email, organization/team slug) contains
// the reserved soft-delete tombstone marker. Such values are rejected before
// any SQL is executed so a live row can never collide with, or be mistaken
// for, a tombstoned one. See tombstone.go and #172.
var ErrReservedValue = errors.New("reserved value")

// ErrInvalidProtocolVersion is returned by CreateMCPServer and
// UpdateMCPServer when a caller-supplied protocol_version value is neither
// "auto"/empty nor one of mcp.SupportedVersions(). Migration
// 0017_mcp_protocol_version.up.sql stores this column as unconstrained TEXT
// specifically because validating the allowed set lives in Go instead of a
// CHECK constraint — this is that validation's home in the write path,
// alongside the equivalent checks internal/api/admin's Admin API handlers and
// internal/config's YAML loader already perform on their own inputs before
// ever reaching here. It exists as defense in depth, not as those callers'
// only line of defense: any caller that writes to mcp_servers through a path
// this package does not already validate at (a future direct DB caller, a
// bug in one of the existing validators) still cannot persist a value the
// rest of VoidLLM does not know how to interpret.
var ErrInvalidProtocolVersion = errors.New("invalid protocol_version")

// translateError maps low-level driver errors to domain sentinels.
// sql.ErrNoRows becomes ErrNotFound, UNIQUE constraint violations become ErrConflict,
// FOREIGN KEY constraint violations become ErrForeignKey,
// and all other errors are returned unchanged. Both sentinel and original error
// are preserved in the chain so callers can use errors.Is on either.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if strings.Contains(msg, "FOREIGN KEY constraint failed") ||
		strings.Contains(msg, "violates foreign key constraint") {
		return fmt.Errorf("%w: %w", ErrForeignKey, err)
	}
	return err
}
