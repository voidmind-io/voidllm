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
