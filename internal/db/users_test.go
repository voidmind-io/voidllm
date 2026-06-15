package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// countUsers returns the number of active (non-deleted) users in the DB.
func countUsers(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM users WHERE deleted_at IS NULL",
	).Scan(&n); err != nil {
		t.Fatalf("countUsers: %v", err)
	}
	return n
}

// countMemberships returns the number of org_memberships rows for a given user.
func countMemberships(t *testing.T, d *DB, userID string) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM org_memberships WHERE user_id = ?", userID,
	).Scan(&n); err != nil {
		t.Fatalf("countMemberships: %v", err)
	}
	return n
}

// mustCreateUser creates a user and fatals the test on error.
func mustCreateUser(t *testing.T, d *DB, params CreateUserParams) *User {
	t.Helper()
	user, err := d.CreateUser(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateUser(%q): %v", params.Email, err)
	}
	return user
}

// testPasswordHash returns a bcrypt hash of a fixed test password.
func testPasswordHash(t *testing.T) *string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("testpassword123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash test password: %v", err)
	}
	s := string(hash)
	return &s
}

// ---- CreateUser --------------------------------------------------------------

func TestCreateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		params    CreateUserParams
		wantErr   error
		checkFunc func(t *testing.T, got *User, params CreateUserParams)
	}{
		{
			name: "correct fields and ID generated",
			params: CreateUserParams{
				Email:       "alice@example.com",
				DisplayName: "Alice",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *User, params CreateUserParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.Email != params.Email {
					t.Errorf("Email = %q, want %q", got.Email, params.Email)
				}
				if got.DisplayName != params.DisplayName {
					t.Errorf("DisplayName = %q, want %q", got.DisplayName, params.DisplayName)
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty, want a timestamp")
				}
				if got.UpdatedAt == "" {
					t.Error("UpdatedAt is empty, want a timestamp")
				}
				if got.DeletedAt != nil {
					t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
				}
			},
		},
		{
			name: "auth_provider defaults to local when empty",
			params: CreateUserParams{
				Email:       "bob@example.com",
				DisplayName: "Bob",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *User, _ CreateUserParams) {
				t.Helper()
				if got.AuthProvider != "local" {
					t.Errorf("AuthProvider = %q, want %q", got.AuthProvider, "local")
				}
			},
		},
		{
			name: "explicit auth_provider oidc is stored",
			params: CreateUserParams{
				Email:        "carol@example.com",
				DisplayName:  "Carol",
				AuthProvider: "oidc",
				ExternalID:   ptr("oidc-sub-12345"),
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *User, _ CreateUserParams) {
				t.Helper()
				if got.AuthProvider != "oidc" {
					t.Errorf("AuthProvider = %q, want %q", got.AuthProvider, "oidc")
				}
			},
		},
		{
			name: "is_system_admin=true stored correctly",
			params: CreateUserParams{
				Email:         "admin@example.com",
				DisplayName:   "Admin User",
				IsSystemAdmin: true,
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *User, _ CreateUserParams) {
				t.Helper()
				if !got.IsSystemAdmin {
					t.Error("IsSystemAdmin = false, want true")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			if tc.params.PasswordHash == nil {
				tc.params.PasswordHash = testPasswordHash(t)
			}

			got, err := d.CreateUser(context.Background(), tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateUser() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, tc.params)
			}
		})
	}
}

func TestCreateUser_DuplicateEmail(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateUser(t, d, CreateUserParams{
		Email:        "dup@example.com",
		DisplayName:  "Original",
		PasswordHash: testPasswordHash(t),
	})

	_, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "dup@example.com",
		DisplayName:  "Duplicate",
		PasswordHash: testPasswordHash(t),
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateUser() duplicate email error = %v, want ErrConflict", err)
	}
}

// ---- GetUser -----------------------------------------------------------------

func TestGetUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the ID to look up
		wantErr error
	}{
		{
			name: "existing user returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:         "getuser@example.com",
					DisplayName:   "Get User",
					PasswordHash:  testPasswordHash(t),
					IsSystemAdmin: true,
				})
				return u.ID
			},
			wantErr: nil,
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted user returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "deleted@example.com",
					DisplayName:  "Deleted User",
					PasswordHash: testPasswordHash(t),
				})
				if err := d.DeleteUser(context.Background(), u.ID); err != nil {
					t.Fatalf("DeleteUser(): %v", err)
				}
				return u.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetUser(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetUser() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetUser() returned nil, want non-nil User")
				}
				if got.ID != id {
					t.Errorf("GetUser().ID = %q, want %q", got.ID, id)
				}
				if got.Email != "getuser@example.com" {
					t.Errorf("GetUser().Email = %q, want %q", got.Email, "getuser@example.com")
				}
				if !got.IsSystemAdmin {
					t.Error("GetUser().IsSystemAdmin = false, want true")
				}
			}
		})
	}
}

// ---- GetUserByEmail ----------------------------------------------------------

func TestGetUserByEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns email to look up
		wantErr error
	}{
		{
			name: "existing user returns correct user",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				mustCreateUser(t, d, CreateUserParams{
					Email:        "byemail@example.com",
					DisplayName:  "By Email",
					PasswordHash: testPasswordHash(t),
				})
				return "byemail@example.com"
			},
			wantErr: nil,
		},
		{
			name: "non-existent email returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "nobody@example.com"
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			email := tc.setup(t, d)
			got, err := d.GetUserByEmail(context.Background(), email)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetUserByEmail() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetUserByEmail() returned nil, want non-nil User")
				}
				if got.Email != email {
					t.Errorf("GetUserByEmail().Email = %q, want %q", got.Email, email)
				}
			}
		})
	}
}

// ---- ListUsers ---------------------------------------------------------------

func TestListUsers_Pagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	for i, email := range []string{"list-a@example.com", "list-b@example.com", "list-c@example.com"} {
		mustCreateUser(t, d, CreateUserParams{
			Email:        email,
			DisplayName:  "List User " + string(rune('A'+i)),
			PasswordHash: testPasswordHash(t),
		})
	}

	// First page: limit=2.
	page1, err := d.ListUsers(ctx, "", 2, false)
	if err != nil {
		t.Fatalf("ListUsers page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Second page: use last ID from page1 as cursor.
	page2, err := d.ListUsers(ctx, page1[len(page1)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListUsers page2 error = %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2))
	}

	// All IDs across pages must be distinct.
	seen := map[string]bool{}
	for _, u := range append(page1, page2...) {
		if seen[u.ID] {
			t.Errorf("duplicate ID %q in paginated results", u.ID)
		}
		seen[u.ID] = true
	}
}

func TestListUsers_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	live := mustCreateUser(t, d, CreateUserParams{
		Email:        "live@example.com",
		DisplayName:  "Live User",
		PasswordHash: testPasswordHash(t),
	})
	gone := mustCreateUser(t, d, CreateUserParams{
		Email:        "gone@example.com",
		DisplayName:  "Gone User",
		PasswordHash: testPasswordHash(t),
	})

	if err := d.DeleteUser(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteUser(): %v", err)
	}

	users, err := d.ListUsers(ctx, "", 100, false)
	if err != nil {
		t.Fatalf("ListUsers(includeDeleted=false) error = %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListUsers len = %d, want 1", len(users))
	}
	if users[0].ID != live.ID {
		t.Errorf("ListUsers returned ID %q, want %q", users[0].ID, live.ID)
	}
}

func TestListUsers_IncludeDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateUser(t, d, CreateUserParams{
		Email:        "incl-live@example.com",
		DisplayName:  "Incl Live",
		PasswordHash: testPasswordHash(t),
	})
	gone := mustCreateUser(t, d, CreateUserParams{
		Email:        "incl-gone@example.com",
		DisplayName:  "Incl Gone",
		PasswordHash: testPasswordHash(t),
	})

	if err := d.DeleteUser(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteUser(): %v", err)
	}

	users, err := d.ListUsers(ctx, "", 100, true)
	if err != nil {
		t.Fatalf("ListUsers(includeDeleted=true) error = %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers(includeDeleted=true) len = %d, want 2", len(users))
	}

	var foundDeleted bool
	for _, u := range users {
		if u.ID == gone.ID {
			foundDeleted = true
			if u.DeletedAt == nil {
				t.Error("deleted user has nil DeletedAt, want a timestamp")
			}
		}
	}
	if !foundDeleted {
		t.Error("deleted user not found in ListUsers(includeDeleted=true) results")
	}
}

// ---- UpdateUser --------------------------------------------------------------

func TestUpdateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) *User
		params    UpdateUserParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *User)
	}{
		{
			name: "update display_name only leaves email unchanged",
			setup: func(t *testing.T, d *DB) *User {
				t.Helper()
				return mustCreateUser(t, d, CreateUserParams{
					Email:        "upd-name@example.com",
					DisplayName:  "Original Name",
					PasswordHash: testPasswordHash(t),
				})
			},
			params:  UpdateUserParams{DisplayName: ptr("Updated Name")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *User) {
				t.Helper()
				if got.DisplayName != "Updated Name" {
					t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Updated Name")
				}
				if got.Email != original.Email {
					t.Errorf("Email = %q, want %q (unchanged)", got.Email, original.Email)
				}
			},
		},
		{
			name: "no fields set returns current user unchanged",
			setup: func(t *testing.T, d *DB) *User {
				t.Helper()
				return mustCreateUser(t, d, CreateUserParams{
					Email:        "no-change@example.com",
					DisplayName:  "Stable User",
					PasswordHash: testPasswordHash(t),
				})
			},
			params:  UpdateUserParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *User) {
				t.Helper()
				if got.Email != original.Email {
					t.Errorf("Email = %q, want %q", got.Email, original.Email)
				}
				if got.DisplayName != original.DisplayName {
					t.Errorf("DisplayName = %q, want %q", got.DisplayName, original.DisplayName)
				}
			},
		},
		{
			name: "is_system_admin change works",
			setup: func(t *testing.T, d *DB) *User {
				t.Helper()
				return mustCreateUser(t, d, CreateUserParams{
					Email:         "promote@example.com",
					DisplayName:   "Promote Me",
					PasswordHash:  testPasswordHash(t),
					IsSystemAdmin: false,
				})
			},
			params:  UpdateUserParams{IsSystemAdmin: ptr(true)},
			wantErr: nil,
			checkFunc: func(t *testing.T, _, got *User) {
				t.Helper()
				if !got.IsSystemAdmin {
					t.Error("IsSystemAdmin = false, want true after update")
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *User {
				return &User{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateUserParams{DisplayName: ptr("Ghost")},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted user returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *User {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "upd-deleted@example.com",
					DisplayName:  "Deleted",
					PasswordHash: testPasswordHash(t),
				})
				if err := d.DeleteUser(context.Background(), u.ID); err != nil {
					t.Fatalf("DeleteUser(): %v", err)
				}
				return u
			},
			params:  UpdateUserParams{DisplayName: ptr("Still Gone")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			original := tc.setup(t, d)
			got, err := d.UpdateUser(context.Background(), original.ID, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateUser() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

func TestUpdateUser_EmailConflict(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateUser(t, d, CreateUserParams{
		Email:        "taken@example.com",
		DisplayName:  "Taken",
		PasswordHash: testPasswordHash(t),
	})
	target := mustCreateUser(t, d, CreateUserParams{
		Email:        "target@example.com",
		DisplayName:  "Target",
		PasswordHash: testPasswordHash(t),
	})

	_, err := d.UpdateUser(ctx, target.ID, UpdateUserParams{Email: ptr("taken@example.com")})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("UpdateUser() email conflict error = %v, want ErrConflict", err)
	}
}

// ---- GetUserPasswordHash -----------------------------------------------------

func TestGetUserPasswordHash(t *testing.T) {
	t.Parallel()

	const testPassword = "testpassword123"

	tests := []struct {
		name        string
		setup       func(t *testing.T, d *DB) string // returns email to look up
		wantErr     error
		errContains string // non-empty: check error message contains this substring
		checkHash   func(t *testing.T, userID, hash string, d *DB)
	}{
		{
			name: "valid user returns userID and hash",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				mustCreateUser(t, d, CreateUserParams{
					Email:        "hashtest@example.com",
					DisplayName:  "Hash Test",
					PasswordHash: testPasswordHash(t),
				})
				return "hashtest@example.com"
			},
			wantErr: nil,
			checkHash: func(t *testing.T, userID, hash string, d *DB) {
				t.Helper()
				if userID == "" {
					t.Error("userID is empty, want non-empty")
				}
				if hash == "" {
					t.Error("hash is empty, want non-empty bcrypt hash")
				}
				// Verify the returned hash actually matches the test password.
				if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)); err != nil {
					t.Errorf("returned hash does not match test password: %v", err)
				}
				// Verify the returned userID matches the real user record.
				user, err := d.GetUserByEmail(context.Background(), "hashtest@example.com")
				if err != nil {
					t.Fatalf("GetUserByEmail: %v", err)
				}
				if userID != user.ID {
					t.Errorf("userID = %q, want %q", userID, user.ID)
				}
			},
		},
		{
			name: "non-existent email returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "nobody@example.com"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted user returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "deleted-hash@example.com",
					DisplayName:  "Deleted Hash",
					PasswordHash: testPasswordHash(t),
				})
				if err := d.DeleteUser(context.Background(), u.ID); err != nil {
					t.Fatalf("DeleteUser(): %v", err)
				}
				return "deleted-hash@example.com"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "SSO user with NULL password_hash returns no-password error",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				mustCreateUser(t, d, CreateUserParams{
					Email:        "sso@example.com",
					DisplayName:  "SSO User",
					AuthProvider: "oidc",
					ExternalID:   ptr("oidc-sub-abc123"),
					// PasswordHash intentionally nil — SSO account
				})
				return "sso@example.com"
			},
			wantErr:     nil, // errors.Is won't match; check errContains instead
			errContains: "no password",
			checkHash:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			email := tc.setup(t, d)
			userID, hash, err := d.GetUserPasswordHash(context.Background(), email)

			if tc.errContains != "" {
				if err == nil {
					t.Fatalf("GetUserPasswordHash() error = nil, want error containing %q", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("GetUserPasswordHash() error = %q, want it to contain %q", err.Error(), tc.errContains)
				}
				return
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetUserPasswordHash() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkHash != nil {
				tc.checkHash(t, userID, hash, d)
			}
		})
	}
}

// ---- CreateUserWithMembership ------------------------------------------------

func TestCreateUserWithMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, d *DB) (orgID string)
		userParams CreateUserParams
		role       string
		wantErr    error
		checkFunc  func(t *testing.T, d *DB, got *User, orgID string)
	}{
		{
			name: "happy path: user and membership created atomically",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Test Org", Slug: "cwm-happy"})
				return org.ID
			},
			userParams: CreateUserParams{
				Email:       "cwm-happy@example.com",
				DisplayName: "Happy Path User",
			},
			role:    "member",
			wantErr: nil,
			checkFunc: func(t *testing.T, d *DB, got *User, orgID string) {
				t.Helper()
				// Returned user has the expected fields.
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.Email != "cwm-happy@example.com" {
					t.Errorf("Email = %q, want %q", got.Email, "cwm-happy@example.com")
				}
				// ResolveUserRole returns the correct role and orgID.
				role, resolvedOrgID, err := d.ResolveUserRole(context.Background(), got.ID)
				if err != nil {
					t.Fatalf("ResolveUserRole() error = %v", err)
				}
				if role != "member" {
					t.Errorf("ResolveUserRole() role = %q, want %q", role, "member")
				}
				if resolvedOrgID != orgID {
					t.Errorf("ResolveUserRole() orgID = %q, want %q", resolvedOrgID, orgID)
				}
				// Membership row exists in the DB.
				if n := countMemberships(t, d, got.ID); n != 1 {
					t.Errorf("org_memberships count = %d, want 1", n)
				}
			},
		},
		{
			name: "non-existent orgID returns ErrForeignKey",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000" // does not exist
			},
			userParams: CreateUserParams{
				Email:       "cwm-fk@example.com",
				DisplayName: "FK User",
			},
			role:      "member",
			wantErr:   ErrForeignKey,
			checkFunc: nil,
		},
		{
			name: "FK failure rolls back: no orphan user record left behind",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			userParams: CreateUserParams{
				Email:       "cwm-rollback@example.com",
				DisplayName: "Rollback User",
			},
			role:    "member",
			wantErr: ErrForeignKey,
			checkFunc: func(t *testing.T, d *DB, _ *User, _ string) {
				t.Helper()
				// After FK failure no user row should exist.
				_, err := d.GetUserByEmail(context.Background(), "cwm-rollback@example.com")
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("GetUserByEmail() after FK rollback = %v, want ErrNotFound (no orphan user)", err)
				}
			},
		},
		{
			name: "role org_admin is stored correctly in membership",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Admin Role Org", Slug: "cwm-admin-role"})
				return org.ID
			},
			userParams: CreateUserParams{
				Email:       "cwm-admin@example.com",
				DisplayName: "Org Admin User",
			},
			role:    "org_admin",
			wantErr: nil,
			checkFunc: func(t *testing.T, d *DB, got *User, orgID string) {
				t.Helper()
				role, resolvedOrgID, err := d.ResolveUserRole(context.Background(), got.ID)
				if err != nil {
					t.Fatalf("ResolveUserRole() error = %v", err)
				}
				if role != "org_admin" {
					t.Errorf("ResolveUserRole() role = %q, want %q", role, "org_admin")
				}
				if resolvedOrgID != orgID {
					t.Errorf("ResolveUserRole() orgID = %q, want %q", resolvedOrgID, orgID)
				}
			},
		},
		{
			name: "duplicate email returns ErrConflict",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Dup Email Org", Slug: "cwm-dup-email"})
				// Pre-create a user with the same email.
				mustCreateUser(t, d, CreateUserParams{
					Email:        "cwm-dup@example.com",
					DisplayName:  "Original",
					PasswordHash: testPasswordHash(t),
				})
				return org.ID
			},
			userParams: CreateUserParams{
				Email:       "cwm-dup@example.com",
				DisplayName: "Duplicate",
			},
			role:    "member",
			wantErr: ErrConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			if tc.userParams.PasswordHash == nil {
				tc.userParams.PasswordHash = testPasswordHash(t)
			}

			orgID := tc.setup(t, d)
			usersBefore := countUsers(t, d)

			got, err := d.CreateUserWithMembership(context.Background(), tc.userParams, orgID, tc.role)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateUserWithMembership() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantErr != nil {
				// On any error, no new user row must have been committed.
				if after := countUsers(t, d); after != usersBefore {
					t.Errorf("user count = %d, want %d (no orphan rows after failure)", after, usersBefore)
				}
			}

			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, d, got, orgID)
			}
		})
	}
}

// ---- GetUserPasswordHashByID -------------------------------------------------

func TestGetUserPasswordHashByID(t *testing.T) {
	t.Parallel()

	const testPassword = "testpassword123"

	tests := []struct {
		name              string
		setup             func(t *testing.T, d *DB) string // returns userID to look up
		wantErr           error
		checkAuthProvider string // non-empty: verify returned authProvider equals this
		checkHashMatches  bool   // true: verify returned hash matches testPassword
	}{
		{
			name: "local user returns auth_provider and bcrypt hash",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "byid-local@example.com",
					DisplayName:  "By ID Local",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				return u.ID
			},
			wantErr:           nil,
			checkAuthProvider: "local",
			checkHashMatches:  true,
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted user returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "byid-deleted@example.com",
					DisplayName:  "By ID Deleted",
					PasswordHash: testPasswordHash(t),
				})
				if err := d.DeleteUser(context.Background(), u.ID); err != nil {
					t.Fatalf("DeleteUser(): %v", err)
				}
				return u.ID
			},
			wantErr: ErrNotFound,
		},
		{
			name: "SSO user with NULL password_hash returns ErrNoPassword",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				extID := "oidc-sub-byid-sso"
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "byid-sso@example.com",
					DisplayName:  "By ID SSO",
					AuthProvider: "oidc",
					ExternalID:   &extID,
					// PasswordHash intentionally nil — SSO account.
				})
				return u.ID
			},
			wantErr: ErrNoPassword,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			userID := tc.setup(t, d)
			authProvider, hash, err := d.GetUserPasswordHashByID(context.Background(), userID)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetUserPasswordHashByID() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantErr != nil {
				return
			}

			if tc.checkAuthProvider != "" && authProvider != tc.checkAuthProvider {
				t.Errorf("authProvider = %q, want %q", authProvider, tc.checkAuthProvider)
			}

			if tc.checkHashMatches {
				if hash == "" {
					t.Error("hash is empty, want non-empty bcrypt hash")
				}
				if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)); err != nil {
					t.Errorf("returned hash does not match test password: %v", err)
				}
			}
		})
	}
}

// ---- DeleteUser --------------------------------------------------------------

func TestDeleteUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "delete existing user makes GetUser return ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				return mustCreateUser(t, d, CreateUserParams{
					Email:        "to-delete@example.com",
					DisplayName:  "To Delete",
					PasswordHash: testPasswordHash(t),
				}).ID
			},
			wantErr: nil,
		},
		{
			name: "delete non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "delete already-deleted user returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				u := mustCreateUser(t, d, CreateUserParams{
					Email:        "double-delete@example.com",
					DisplayName:  "Double Delete",
					PasswordHash: testPasswordHash(t),
				})
				if err := d.DeleteUser(context.Background(), u.ID); err != nil {
					t.Fatalf("first DeleteUser(): %v", err)
				}
				return u.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			err := d.DeleteUser(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteUser() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success verify GetUser now returns ErrNotFound.
			if tc.wantErr == nil {
				_, getErr := d.GetUser(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetUser() after DeleteUser() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
