package db

import (
	"context"
	"testing"
)

// TestGetSetting_NotFound verifies that retrieving a key that has never been
// written returns an empty string with no error.
func TestGetSetting_NotFound(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)

	got, err := d.GetSetting(context.Background(), "nonexistent_key")
	if err != nil {
		t.Fatalf("GetSetting() error = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("GetSetting() = %q, want %q (empty string for missing key)", got, "")
	}
}

// TestSetAndGetSetting verifies that a value written via SetSetting is
// returned unchanged by a subsequent GetSetting call.
func TestSetAndGetSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{
			name:  "plain string value",
			key:   "test_key_plain",
			value: "hello world",
		},
		{
			name:  "empty value",
			key:   "test_key_empty",
			value: "",
		},
		{
			name:  "jwt-like value",
			key:   "license_jwt",
			value: "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ0ZXN0In0.sig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			if err := d.SetSetting(context.Background(), tc.key, tc.value); err != nil {
				t.Fatalf("SetSetting() error = %v, want nil", err)
			}

			got, err := d.GetSetting(context.Background(), tc.key)
			if err != nil {
				t.Fatalf("GetSetting() error = %v, want nil", err)
			}
			if got != tc.value {
				t.Errorf("GetSetting() = %q, want %q", got, tc.value)
			}
		})
	}
}

// TestSetSetting_Upsert verifies that writing the same key twice updates the
// stored value to the most recently written value.
func TestSetSetting_Upsert(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	const key = "upsert_key"
	const firstValue = "first"
	const secondValue = "second"

	if err := d.SetSetting(ctx, key, firstValue); err != nil {
		t.Fatalf("SetSetting() first write error = %v, want nil", err)
	}

	if err := d.SetSetting(ctx, key, secondValue); err != nil {
		t.Fatalf("SetSetting() second write error = %v, want nil", err)
	}

	got, err := d.GetSetting(ctx, key)
	if err != nil {
		t.Fatalf("GetSetting() error = %v, want nil", err)
	}
	if got != secondValue {
		t.Errorf("GetSetting() after upsert = %q, want %q", got, secondValue)
	}
}

// TestSetSetting_MultipleKeys verifies that multiple distinct keys are stored
// and retrieved independently without interfering with each other.
func TestSetSetting_MultipleKeys(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	entries := map[string]string{
		"alpha": "value-alpha",
		"beta":  "value-beta",
		"gamma": "value-gamma",
	}

	for key, value := range entries {
		if err := d.SetSetting(ctx, key, value); err != nil {
			t.Fatalf("SetSetting(%q) error = %v, want nil", key, err)
		}
	}

	for key, wantValue := range entries {
		got, err := d.GetSetting(ctx, key)
		if err != nil {
			t.Fatalf("GetSetting(%q) error = %v, want nil", key, err)
		}
		if got != wantValue {
			t.Errorf("GetSetting(%q) = %q, want %q", key, got, wantValue)
		}
	}
}
