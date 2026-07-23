package db

import "testing"

// ---- StripTombstone -----------------------------------------------------

func TestStripTombstone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		id    string
		want  string
	}{
		{
			name:  "value with matching tombstone suffix is stripped",
			value: "gpt-4:deleted:0198c9a2-1111-7000-8000-000000000001",
			id:    "0198c9a2-1111-7000-8000-000000000001",
			want:  "gpt-4",
		},
		{
			name:  "value ending in a different id's tombstone is left unchanged",
			value: "gpt-4:deleted:0198c9a2-1111-7000-8000-000000000001",
			id:    "0198c9a2-2222-7000-8000-000000000002",
			want:  "gpt-4:deleted:0198c9a2-1111-7000-8000-000000000001",
		},
		{
			name:  "value containing the marker mid-string but not as a suffix for this id is unchanged",
			value: "gpt-4:deleted:not-the-id:extra",
			id:    "not-the-id",
			want:  "gpt-4:deleted:not-the-id:extra",
		},
		{
			name:  "value with no tombstone marker at all is unchanged",
			value: "gpt-4",
			id:    "0198c9a2-1111-7000-8000-000000000001",
			want:  "gpt-4",
		},
		{
			name:  "value equal to just the suffix strips to empty string",
			value: ":deleted:0198c9a2-1111-7000-8000-000000000001",
			id:    "0198c9a2-1111-7000-8000-000000000001",
			want:  "",
		},
		{
			name:  "empty value with non-empty id is unchanged",
			value: "",
			id:    "0198c9a2-1111-7000-8000-000000000001",
			want:  "",
		},
		{
			name:  "empty value with empty id: suffix is just the marker, still no match against empty value",
			value: "",
			id:    "",
			want:  "",
		},
		{
			name:  "value equal to the marker+empty-id suffix strips to empty string",
			value: ":deleted:",
			id:    "",
			want:  "",
		},
		{
			name:  "original value happens to contain a colon but not the marker is unchanged",
			value: "team:engineering",
			id:    "0198c9a2-1111-7000-8000-000000000001",
			want:  "team:engineering",
		},
		{
			name:  "email-shaped value with matching tombstone suffix is stripped",
			value: "alice@example.com:deleted:0198c9a2-3333-7000-8000-000000000003",
			id:    "0198c9a2-3333-7000-8000-000000000003",
			want:  "alice@example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := StripTombstone(tc.value, tc.id)
			if got != tc.want {
				t.Errorf("StripTombstone(%q, %q) = %q, want %q", tc.value, tc.id, got, tc.want)
			}
		})
	}
}

// ---- ContainsTombstoneMarker ---------------------------------------------

func TestContainsTombstoneMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{
			name:  "value with the marker in a delete-shaped suffix",
			value: "gpt-4:deleted:0198c9a2-1111-7000-8000-000000000001",
			want:  true,
		},
		{
			name:  "value with the marker anywhere in the middle",
			value: "prefix:deleted:suffix",
			want:  true,
		},
		{
			name:  "value with just the marker itself",
			value: ":deleted:",
			want:  true,
		},
		{
			name:  "value with no marker at all",
			value: "gpt-4",
			want:  false,
		},
		{
			name:  "value containing a colon but not the full marker",
			value: "team:engineering",
			want:  false,
		},
		{
			name:  "empty value",
			value: "",
			want:  false,
		},
		{
			name:  "value containing 'deleted' without surrounding colons",
			value: "deleted-model",
			want:  false,
		},
		{
			name:  "email containing the marker",
			value: "alice:deleted:123@example.com",
			want:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ContainsTombstoneMarker(tc.value)
			if got != tc.want {
				t.Errorf("ContainsTombstoneMarker(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
