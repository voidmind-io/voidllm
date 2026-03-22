package db

import "testing"

func TestSQLiteDialect_Placeholder(t *testing.T) {
	t.Parallel()

	d := SQLiteDialect{}

	tests := []struct {
		name string
		n    int
		want string
	}{
		{name: "position 1", n: 1, want: "?"},
		{name: "position 5", n: 5, want: "?"},
		{name: "position 10", n: 10, want: "?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := d.Placeholder(tc.n)
			if got != tc.want {
				t.Errorf("SQLiteDialect.Placeholder(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func TestPostgresDialect_Placeholder(t *testing.T) {
	t.Parallel()

	d := PostgresDialect{}

	tests := []struct {
		name string
		n    int
		want string
	}{
		{name: "position 1", n: 1, want: "$1"},
		{name: "position 5", n: 5, want: "$5"},
		{name: "position 10", n: 10, want: "$10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := d.Placeholder(tc.n)
			if got != tc.want {
				t.Errorf("PostgresDialect.Placeholder(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}
