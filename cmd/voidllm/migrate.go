package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// migrationOrder lists tables in foreign-key-safe order so that referenced
// tables are cleared and populated before the tables that reference them.
// These names are hardcoded constants that match the schema exactly — they are
// never derived from user input, so direct string interpolation into SQL is safe.
var migrationOrder = []string{
	"users",
	"organizations",
	"org_memberships",
	"teams",
	"team_memberships",
	"service_accounts",
	"models",
	"api_keys",
	"model_aliases",
	"org_model_access",
	"team_model_access",
	"key_model_access",
	"invite_tokens",
	"usage_events",
	"usage_hourly",
}

// runMigrate is the entry point for the "migrate" subcommand. It opens source
// and target databases, applies schema migrations to the target, clears all
// target tables in reverse FK order, then copies all rows from each table in
// migrationOrder. A --dry-run flag counts source rows without writing anything
// to the target.
func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	fromDSN := fs.String("from", "", "Source database DSN (e.g., /data/voidllm.db or postgres://...)")
	toDSN := fs.String("to", "", "Target database DSN")
	dryRun := fs.Bool("dry-run", false, "Count rows only, don't write")
	fs.Parse(args) //nolint:errcheck // ExitOnError handles the error

	if *fromDSN == "" || *toDSN == "" {
		fmt.Println("Usage: voidllm migrate --from <source-dsn> --to <target-dsn> [--dry-run]")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  voidllm migrate --from /data/voidllm.db --to postgres://user:pass@host:5432/voidllm")
		fmt.Println("  voidllm migrate --from postgres://... --to /data/backup.db")
		fmt.Println("  voidllm migrate --from /data/voidllm.db --to /data/copy.db --dry-run")
		os.Exit(1)
	}

	ctx := context.Background()

	srcDriver := detectDriver(*fromDSN)
	dstDriver := detectDriver(*toDSN)
	srcDSN := cleanDSN(*fromDSN)
	dstDSN := cleanDSN(*toDSN)

	fmt.Println("VoidLLM Database Migration")
	fmt.Printf("  Source: %s (%s)\n", redactDSN(srcDSN), srcDriver)
	fmt.Printf("  Target: %s (%s)\n", redactDSN(dstDSN), dstDriver)
	if *dryRun {
		fmt.Println("  Mode:   DRY RUN (no writes)")
	}
	fmt.Println()

	srcDB, err := db.Open(ctx, config.DatabaseConfig{
		Driver: srcDriver,
		DSN:    srcDSN,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open source: %v\n", err)
		os.Exit(1)
	}
	defer srcDB.Close()

	dstDB, err := db.Open(ctx, config.DatabaseConfig{
		Driver: dstDriver,
		DSN:    dstDSN,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open target: %v\n", err)
		os.Exit(1)
	}
	defer dstDB.Close()

	if !*dryRun {
		fmt.Print("Running schema migrations on target... ")
		if err := db.RunMigrations(ctx, dstDB.SQL(), dstDB.Dialect(), slog.Default()); err != nil {
			fmt.Fprintf(os.Stderr, "failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("done")

		// Clear target in reverse FK order (children before parents) so that
		// foreign-key constraints are never violated during the wipe.
		fmt.Print("Clearing target tables... ")
		for i := len(migrationOrder) - 1; i >= 0; i-- {
			table := migrationOrder[i]
			if _, err := dstDB.SQL().ExecContext(ctx, "DELETE FROM "+table); err != nil {
				fmt.Fprintf(os.Stderr, "failed to clear %s: %v\n", table, err)
				os.Exit(1)
			}
		}
		fmt.Println("done")
	}

	fmt.Println("Migrating data:")
	totalRows := 0
	start := time.Now()

	for _, table := range migrationOrder {
		count, err := migrateTable(ctx, srcDB, dstDB, table, *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-20s FAILED: %v\n", table, err)
			fmt.Fprintf(os.Stderr, "\nMigration aborted. Fix the issue and re-run.\n")
			fmt.Fprintf(os.Stderr, "Completed tables are committed. Failed table was rolled back.\n")
			os.Exit(1)
		}
		fmt.Printf("  %-20s %s rows %s\n", table, formatCount(count), checkmark(*dryRun))
		totalRows += count
	}

	elapsed := time.Since(start)
	fmt.Printf("\nMigration %s. %s total rows %s in %v.\n",
		modeLabel(*dryRun), formatCount(totalRows),
		actionLabel(*dryRun), elapsed.Round(time.Millisecond))

	if !*dryRun {
		fmt.Printf("You can now switch database.driver to %q in your config.\n", dstDriver)
	}
}

// maxSQLiteParams is the default SQLITE_MAX_VARIABLE_NUMBER limit.
const maxSQLiteParams = 999

// batchRows returns the number of rows per INSERT batch, capped so the total
// parameter count stays within SQLite's limit of 999 variables.
func batchRows(columnCount int) int {
	b := maxSQLiteParams / columnCount
	if b < 1 {
		b = 1
	}
	return b
}

// migrateTable copies all rows for table from src to dst inside a single
// transaction using batched multi-row INSERT statements. When dryRun is true
// it only counts source rows without writing to dst. It returns the number of
// rows counted or copied. On error it returns the number of rows successfully
// inserted before the failure; the transaction is rolled back automatically
// via the deferred call.
func migrateTable(ctx context.Context, src, dst *db.DB, table string, dryRun bool) (int, error) {
	// Table names come from the hardcoded migrationOrder slice, not user input.
	var count int
	if err := src.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		return 0, fmt.Errorf("count source: %w", err)
	}

	if count == 0 || dryRun {
		return count, nil
	}

	// Probe column names by requesting an empty result set.
	probe, err := src.SQL().QueryContext(ctx, "SELECT * FROM "+table+" LIMIT 0")
	if err != nil {
		return 0, fmt.Errorf("probe columns: %w", err)
	}
	columns, err := probe.Columns()
	_ = probe.Close() // LIMIT 0 result set; close error is irrelevant
	if err != nil {
		return 0, fmt.Errorf("get columns: %w", err)
	}

	batchSize := batchRows(len(columns))

	tx, err := dst.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // superseded by Commit on the success path

	rows, err := src.SQL().QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return 0, fmt.Errorf("read source: %w", err)
	}
	defer rows.Close()

	// Allocate scan buffers once and reuse them for every row.
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}

	batch := make([]any, 0, batchSize*len(columns))
	batchCount := 0
	inserted := 0

	flush := func() error {
		if batchCount == 0 {
			return nil
		}
		stmt := buildBatchInsert(table, columns, batchCount, dst.Dialect())
		if _, execErr := tx.ExecContext(ctx, stmt, batch...); execErr != nil {
			return execErr
		}
		inserted += batchCount
		batch = batch[:0]
		batchCount = 0
		return nil
	}

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return inserted, fmt.Errorf("scan row %d: %w", inserted, err)
		}
		// Copy the scanned values; the ptrs slice points into values which is
		// overwritten on each Scan call.
		rowCopy := make([]any, len(columns))
		copy(rowCopy, values)
		batch = append(batch, rowCopy...)
		batchCount++

		if batchCount >= batchSize {
			if err := flush(); err != nil {
				return inserted, fmt.Errorf("batch insert at row %d: %w", inserted, err)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return inserted, fmt.Errorf("rows iteration: %w", err)
	}

	if err := flush(); err != nil {
		return inserted, fmt.Errorf("final batch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("commit: %w", err)
	}

	return inserted, nil
}

// buildBatchInsert constructs a multi-row INSERT statement of the form:
//
//	INSERT INTO t (c1, c2) VALUES ($1, $2), ($3, $4), ...
//
// Placeholders are generated using dialect for rowCount rows each with
// len(columns) columns. Column and table names come from the hardcoded
// migrationOrder slice and probed schema — never from user input.
func buildBatchInsert(table string, columns []string, rowCount int, dialect db.Dialect) string {
	var sb strings.Builder
	sb.Grow(len(table)*2 + len(columns)*10 + rowCount*(len(columns)*5+3) + 50)
	sb.WriteString("INSERT INTO ")
	sb.WriteString(table)
	sb.WriteString(" (")
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = `"` + c + `"`
	}
	sb.WriteString(strings.Join(quoted, ", "))
	sb.WriteString(") VALUES ")

	argN := 1
	for r := 0; r < rowCount; r++ {
		if r > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		for c := 0; c < len(columns); c++ {
			if c > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(dialect.Placeholder(argN))
			argN++
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// redactDSN replaces the password in a DSN URL with "***" before printing.
// Non-URL DSNs (e.g., SQLite file paths) are returned unchanged.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// detectDriver infers the database driver from the DSN string.
// A postgres:// or postgresql:// prefix indicates PostgreSQL; everything else
// is treated as SQLite (file path or sqlite:// URI).
func detectDriver(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// cleanDSN normalises the DSN for use with the Go database drivers.
// The SQLite driver expects a plain file path; strip any sqlite:// URI prefix.
// PostgreSQL DSNs (postgres://...) are passed through unchanged — pgx understands them.
func cleanDSN(dsn string) string {
	// Strip the longer prefix first to avoid leaving a leading slash when the
	// input is "sqlite:///path" (three slashes = absolute path).
	if after, ok := strings.CutPrefix(dsn, "sqlite:///"); ok {
		return "/" + after
	}
	dsn, _ = strings.CutPrefix(dsn, "sqlite://")
	return dsn
}

// formatCount formats n with comma separators for readability (e.g. 1,234,567).
func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// checkmark returns a short status suffix for the per-table progress line.
func checkmark(dryRun bool) string {
	if dryRun {
		return "(counted)"
	}
	return "✓"
}

// modeLabel returns the completion label used in the summary line.
func modeLabel(dryRun bool) string {
	if dryRun {
		return "dry run complete"
	}
	return "complete"
}

// actionLabel returns the verb used in the summary line.
func actionLabel(dryRun bool) string {
	if dryRun {
		return "counted"
	}
	return "copied"
}
