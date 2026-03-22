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

// translateError maps low-level driver errors to domain sentinels.
// sql.ErrNoRows becomes ErrNotFound, UNIQUE constraint violations become ErrConflict,
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
	return err
}
