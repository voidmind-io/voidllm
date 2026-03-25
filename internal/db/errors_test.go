package db

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

func TestTranslateError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantNotFound bool
		wantConflict bool
		wantSame     bool // result == input (unchanged)
		wantNil      bool
	}{
		{
			name:         "sql.ErrNoRows maps to ErrNotFound",
			err:          sql.ErrNoRows,
			wantNotFound: true,
		},
		{
			name:         "sql.ErrNoRows does not map to ErrConflict",
			err:          sql.ErrNoRows,
			wantConflict: false,
			wantNotFound: true,
		},
		{
			name:     "io.EOF returned unchanged",
			err:      io.EOF,
			wantSame: true,
		},
		{
			name:    "nil returns nil",
			err:     nil,
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translateError(tc.err)

			if tc.wantNil {
				if got != nil {
					t.Errorf("translateError(nil) = %v, want nil", got)
				}
				return
			}

			if tc.wantSame {
				if !errors.Is(got, tc.err) {
					t.Errorf("translateError(%v) = %v, want same error", tc.err, got)
				}
				return
			}

			if tc.wantNotFound && !errors.Is(got, ErrNotFound) {
				t.Errorf("translateError(%v): errors.Is(result, ErrNotFound) = false, want true", tc.err)
			}
			if !tc.wantConflict && tc.wantNotFound && errors.Is(got, ErrConflict) {
				t.Errorf("translateError(%v): errors.Is(result, ErrConflict) = true, want false", tc.err)
			}
		})
	}
}

func TestTranslateError_UniqueConstraint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file::memory:?cache=shared&mode=memory",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sqlDB := db.SQL()
	if _, err := sqlDB.ExecContext(ctx, "CREATE TABLE err_unique_test (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, "INSERT INTO err_unique_test VALUES ('x')"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, dupErr := sqlDB.ExecContext(ctx, "INSERT INTO err_unique_test VALUES ('x')")
	if dupErr == nil {
		t.Fatal("expected UNIQUE constraint error from duplicate insert, got nil")
	}

	result := translateError(dupErr)

	if !errors.Is(result, ErrConflict) {
		t.Errorf("translateError(UNIQUE error): errors.Is(result, ErrConflict) = false, want true; result = %v", result)
	}
	if errors.Is(result, ErrNotFound) {
		t.Errorf("translateError(UNIQUE error): errors.Is(result, ErrNotFound) = true, want false")
	}
}
