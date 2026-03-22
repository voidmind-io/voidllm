package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// Bootstrap performs first-time setup when the database is empty.
// If cfg.AdminKey is non-empty and no API keys exist in the database, it
// creates a default organization, system admin user with a random password,
// and an API key. The generated API key and password are written to stderr so
// the operator can copy them for initial setup.
// If the database already has keys, or cfg.AdminKey is empty, this is a no-op.
func Bootstrap(ctx context.Context, sqlDB *sql.DB, dialect db.Dialect,
	keyCache *cache.Cache[string, KeyInfo], cfg config.SettingsConfig,
	hmacSecret []byte, log *slog.Logger) error {
	if cfg.AdminKey == "" {
		return nil
	}

	if len(cfg.AdminKey) < 32 {
		return errors.New("admin key must be at least 32 characters")
	}

	orgName := cfg.Bootstrap.OrgName
	orgSlug := cfg.Bootstrap.OrgSlug
	adminEmail := cfg.Bootstrap.AdminEmail

	orgID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("bootstrap: generate org id: %w", err)
	}

	userID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("bootstrap: generate user id: %w", err)
	}

	membershipID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("bootstrap: generate membership id: %w", err)
	}

	keyID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("bootstrap: generate key id: %w", err)
	}

	password, err := generatePassword(16)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bootstrap: hash password: %w", err)
	}

	plaintextKey, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		return fmt.Errorf("bootstrap: generate api key: %w", err)
	}

	keyHash := keygen.Hash(plaintextKey, hmacSecret)
	keyHint := keygen.Hint(plaintextKey)

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bootstrap: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful Commit

	var count int
	row := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_keys WHERE deleted_at IS NULL")
	if err = row.Scan(&count); err != nil {
		return fmt.Errorf("bootstrap: count api_keys: %w", err)
	}

	if count > 0 {
		log.Warn("VOIDLLM_ADMIN_KEY is set but database already has keys, ignoring")
		return nil
	}

	insertOrg := "INSERT INTO organizations (id, name, slug, created_at, updated_at) VALUES (" +
		dialect.Placeholder(1) + ", " +
		dialect.Placeholder(2) + ", " +
		dialect.Placeholder(3) + ", CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
	if _, err = tx.ExecContext(ctx, insertOrg, orgID.String(), orgName, orgSlug); err != nil {
		return fmt.Errorf("bootstrap: insert organization: %w", err)
	}

	insertUser := "INSERT INTO users (id, email, display_name, password_hash, auth_provider, is_system_admin, created_at, updated_at) VALUES (" +
		dialect.Placeholder(1) + ", " +
		dialect.Placeholder(2) + ", " +
		dialect.Placeholder(3) + ", " +
		dialect.Placeholder(4) + ", " +
		dialect.Placeholder(5) + ", " +
		dialect.Placeholder(6) + ", CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
	if _, err = tx.ExecContext(ctx, insertUser,
		userID.String(),
		adminEmail,
		"Admin",
		string(passwordHash),
		"local",
		1,
	); err != nil {
		return fmt.Errorf("bootstrap: insert user: %w", err)
	}

	insertMembership := "INSERT INTO org_memberships (id, org_id, user_id, role, created_at) VALUES (" +
		dialect.Placeholder(1) + ", " +
		dialect.Placeholder(2) + ", " +
		dialect.Placeholder(3) + ", " +
		dialect.Placeholder(4) + ", CURRENT_TIMESTAMP)"
	if _, err = tx.ExecContext(ctx, insertMembership,
		membershipID.String(),
		orgID.String(),
		userID.String(),
		RoleOrgAdmin,
	); err != nil {
		return fmt.Errorf("bootstrap: insert org membership: %w", err)
	}

	insertKey := "INSERT INTO api_keys (id, key_hash, key_hint, key_type, name, org_id, user_id, created_by, created_at, updated_at) VALUES (" +
		dialect.Placeholder(1) + ", " +
		dialect.Placeholder(2) + ", " +
		dialect.Placeholder(3) + ", " +
		dialect.Placeholder(4) + ", " +
		dialect.Placeholder(5) + ", " +
		dialect.Placeholder(6) + ", " +
		dialect.Placeholder(7) + ", " +
		dialect.Placeholder(8) + ", CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
	if _, err = tx.ExecContext(ctx, insertKey,
		keyID.String(),
		keyHash,
		keyHint,
		keygen.KeyTypeUser,
		"Bootstrap Admin Key",
		orgID.String(),
		userID.String(),
		userID.String(),
	); err != nil {
		return fmt.Errorf("bootstrap: insert api key: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("bootstrap: commit transaction: %w", err)
	}

	keyCache.Set(keyHash, KeyInfo{
		ID:      keyID.String(),
		KeyType: keygen.KeyTypeUser,
		Role:    RoleSystemAdmin,
		OrgID:   orgID.String(),
		UserID:  userID.String(),
		Name:    "Bootstrap Admin Key",
	})

	os.Unsetenv("VOIDLLM_ADMIN_KEY")

	// Intentional use of fmt.Fprintln instead of slog: the bootstrap plaintext
	// key and password must be shown to the operator on stderr but must NOT
	// go through structured logging where they could be captured by log
	// aggregation systems (ELK, Datadog, CloudWatch).
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintln(os.Stderr, " BOOTSTRAP COMPLETE — COPY THESE NOW")
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintf(os.Stderr, "  API Key:    %s\n", plaintextKey)
	fmt.Fprintf(os.Stderr, "  Email:      %s\n", adminEmail)
	fmt.Fprintf(os.Stderr, "  Password:   %s\n", password)
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintln(os.Stderr, "")

	log.Warn("bootstrap complete, default organization and system admin created",
		slog.String("key_hint", keyHint))

	return nil
}

// generatePassword creates a random alphanumeric password of the given length.
func generatePassword(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		b[i] = charset[idx.Int64()]
	}
	return string(b), nil
}
