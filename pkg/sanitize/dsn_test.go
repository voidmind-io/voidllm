package sanitize_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/pkg/sanitize"
)

func TestDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "URL style with password",
			in:   "postgres://alice:s3cr3t@localhost:5432/mydb",
			want: "postgres://alice:***@localhost:5432/mydb",
		},
		{
			name: "URL style without password",
			in:   "postgres://alice@localhost:5432/mydb",
			want: "postgres://alice@localhost:5432/mydb",
		},
		{
			name: "URL style no user info",
			in:   "postgres://localhost:5432/mydb",
			want: "postgres://localhost:5432/mydb",
		},
		{
			name: "key-value style lowercase",
			in:   "host=localhost user=alice password=s3cr3t dbname=mydb",
			want: "host=localhost user=alice password=*** dbname=mydb",
		},
		{
			name: "key-value style uppercase PASSWORD",
			in:   "host=localhost PASSWORD=s3cr3t dbname=mydb",
			want: "host=localhost PASSWORD=*** dbname=mydb",
		},
		{
			name: "key-value style with spaces around equals",
			in:   "host=localhost password = s3cr3t dbname=mydb",
			want: "host=localhost password = *** dbname=mydb",
		},
		{
			name: "SQLite file path unchanged",
			in:   "/var/lib/voidllm/voidllm.db",
			want: "/var/lib/voidllm/voidllm.db",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "no credentials present",
			in:   "host=localhost dbname=mydb sslmode=disable",
			want: "host=localhost dbname=mydb sslmode=disable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitize.DSN(tc.in)
			if got != tc.want {
				t.Errorf("DSN(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
