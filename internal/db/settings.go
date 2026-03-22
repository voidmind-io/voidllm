package db

import (
	"context"
	"database/sql"
	"fmt"
)

// GetSetting returns the value for a settings key. Returns an empty string
// if the key does not exist.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := d.sql.QueryRowContext(ctx,
		fmt.Sprintf("SELECT value FROM settings WHERE key = %s", d.dialect.Placeholder(1)),
		key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return value, nil
}

// SetSetting upserts a key-value pair in the settings table.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		fmt.Sprintf(
			`INSERT INTO settings (key, value, updated_at) VALUES (%s, %s, CURRENT_TIMESTAMP)
             ON CONFLICT(key) DO UPDATE SET value = %s, updated_at = CURRENT_TIMESTAMP`,
			d.dialect.Placeholder(1), d.dialect.Placeholder(2), d.dialect.Placeholder(3),
		),
		key, value, value,
	)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}
