package proxy

import (
	"sync"
	"testing"
)

// ---- Check -------------------------------------------------------------------

// TestMCPAccessCache_OrgNotConfigured_Denied verifies that an org with no entry
// in the cache is denied (closed-by-default).
func TestMCPAccessCache_OrgNotConfigured_Denied(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()

	if c.Check("org-unknown", "", "key-1", "sv-target") {
		t.Error("Check() = true, want false (org not configured)")
	}
}

// TestMCPAccessCache_OrgEmpty_Denied verifies that an org present in the cache
// with an empty allowlist (nil set) is denied (closed-by-default).
func TestMCPAccessCache_OrgEmpty_Denied(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	// Load an org with an explicitly empty slice — becomes nil in the set map.
	c.Load(
		map[string][]string{"org-1": {}},
		nil,
		nil,
	)

	if c.Check("org-1", "", "key-1", "sv-target") {
		t.Error("Check() = true, want false (org configured with empty allowlist)")
	}
}

// TestMCPAccessCache_OrgAllowed verifies that an org allowlist containing the
// server ID grants access.
func TestMCPAccessCache_OrgAllowed(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target", "sv-other"}},
		nil,
		nil,
	)

	if !c.Check("org-1", "", "key-1", "sv-target") {
		t.Error("Check() = false, want true (org allows server)")
	}
}

// TestMCPAccessCache_OrgDenied verifies that an org allowlist that does NOT
// contain the server ID denies access.
func TestMCPAccessCache_OrgDenied(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-other"}},
		nil,
		nil,
	)

	if c.Check("org-1", "", "key-1", "sv-target") {
		t.Error("Check() = true, want false (server not in org allowlist)")
	}
}

// TestMCPAccessCache_TeamInherits verifies that a team not present in the cache
// does not further restrict access that the org has granted.
func TestMCPAccessCache_TeamInherits(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		nil, // team-1 absent — open-by-default
		nil,
	)

	if !c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("Check() = false, want true (team inherits from org)")
	}
}

// TestMCPAccessCache_TeamRestricts verifies that a non-empty team allowlist
// that excludes the server ID overrides org-level access.
func TestMCPAccessCache_TeamRestricts(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		map[string][]string{"team-1": {"sv-other"}}, // restricts to different server
		nil,
	)

	if c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("Check() = true, want false (team allowlist excludes server)")
	}
}

// TestMCPAccessCache_TeamEmpty_Inherits verifies that a team present in the
// cache with an empty allowlist is treated as open-by-default.
func TestMCPAccessCache_TeamEmpty_Inherits(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		map[string][]string{"team-1": {}}, // empty — open-by-default
		nil,
	)

	if !c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("Check() = false, want true (empty team allowlist does not restrict)")
	}
}

// TestMCPAccessCache_KeyInherits verifies that a key not present in the cache
// does not further restrict access that org+team have granted.
func TestMCPAccessCache_KeyInherits(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		map[string][]string{"team-1": {"sv-target"}},
		nil, // key-1 absent — open-by-default
	)

	if !c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("Check() = false, want true (key inherits from org+team)")
	}
}

// TestMCPAccessCache_KeyRestricts verifies that a non-empty key allowlist that
// excludes the server ID overrides org+team access.
func TestMCPAccessCache_KeyRestricts(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		map[string][]string{"team-1": {"sv-target"}},
		map[string][]string{"key-1": {"sv-other"}}, // restricts to different server
	)

	if c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("Check() = true, want false (key allowlist excludes server)")
	}
}

// TestMCPAccessCache_KeyEmpty_Inherits verifies that a key present in the cache
// with an empty allowlist is treated as open-by-default.
func TestMCPAccessCache_KeyEmpty_Inherits(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		nil,
		map[string][]string{"key-1": {}}, // empty — open-by-default
	)

	if !c.Check("org-1", "", "key-1", "sv-target") {
		t.Error("Check() = false, want true (empty key allowlist does not restrict)")
	}
}

// TestMCPAccessCache_NoTeamID_SkipsTeamCheck verifies that passing an empty
// teamID bypasses the team-level check entirely.
func TestMCPAccessCache_NoTeamID_SkipsTeamCheck(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		map[string][]string{"team-1": {"sv-other"}}, // would deny if checked
		nil,
	)

	// Pass empty teamID — team restriction for team-1 must be ignored.
	if !c.Check("org-1", "", "key-1", "sv-target") {
		t.Error("Check() = false, want true (empty teamID skips team check)")
	}
}

// ---- Load --------------------------------------------------------------------

// TestMCPAccessCache_Load verifies that Load correctly populates org, team, and
// key maps and that subsequent Check calls reflect the loaded data.
func TestMCPAccessCache_Load(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()

	// Initial state: everything denied (org not configured).
	if c.Check("org-1", "team-1", "key-1", "sv-target") {
		t.Error("initial Check() = true before Load, want false")
	}

	// Load data.
	c.Load(
		map[string][]string{
			"org-1": {"sv-target", "sv-extra"},
			"org-2": {"sv-other"},
		},
		map[string][]string{
			"team-1": {"sv-target"},
		},
		map[string][]string{
			"key-1": {"sv-target"},
		},
	)

	tests := []struct {
		name     string
		orgID    string
		teamID   string
		keyID    string
		serverID string
		want     bool
	}{
		{
			name:  "all levels allow",
			orgID: "org-1", teamID: "team-1", keyID: "key-1",
			serverID: "sv-target", want: true,
		},
		{
			name:  "org allows but team restricts",
			orgID: "org-1", teamID: "team-1", keyID: "key-1",
			serverID: "sv-extra", want: false, // team-1 only has sv-target
		},
		{
			name:  "different org allows its own server",
			orgID: "org-2", teamID: "", keyID: "key-2",
			serverID: "sv-other", want: true,
		},
		{
			name:  "different org denied for sv-target",
			orgID: "org-2", teamID: "", keyID: "key-2",
			serverID: "sv-target", want: false,
		},
		{
			name:  "unknown org denied",
			orgID: "org-99", teamID: "", keyID: "key-1",
			serverID: "sv-target", want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := c.Check(tc.orgID, tc.teamID, tc.keyID, tc.serverID)
			if got != tc.want {
				t.Errorf("Check(%q, %q, %q, %q) = %v, want %v",
					tc.orgID, tc.teamID, tc.keyID, tc.serverID, got, tc.want)
			}
		})
	}
}

// TestMCPAccessCache_Load_ReplacesExisting verifies that a second call to Load
// atomically replaces the previous state.
func TestMCPAccessCache_Load_ReplacesExisting(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-old"}},
		nil,
		nil,
	)

	// After first load, sv-old is accessible.
	if !c.Check("org-1", "", "key-1", "sv-old") {
		t.Fatal("pre-condition: sv-old should be accessible before second load")
	}

	// Second load replaces data.
	c.Load(
		map[string][]string{"org-1": {"sv-new"}},
		nil,
		nil,
	)

	if c.Check("org-1", "", "key-1", "sv-old") {
		t.Error("Check(sv-old) = true after replace, want false")
	}
	if !c.Check("org-1", "", "key-1", "sv-new") {
		t.Error("Check(sv-new) = false after replace, want true")
	}
}

// ---- Len ---------------------------------------------------------------------

// TestMCPAccessCache_Len verifies that Len returns the sum of entries across all
// three scopes (one entry per entity ID, not per server ID).
func TestMCPAccessCache_Len(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	if c.Len() != 0 {
		t.Errorf("Len() = %d before Load, want 0", c.Len())
	}

	c.Load(
		map[string][]string{"org-1": {"sv-a"}, "org-2": {"sv-b"}},
		map[string][]string{"team-1": {"sv-a"}},
		map[string][]string{"key-1": {"sv-a"}, "key-2": {"sv-b"}, "key-3": {"sv-c"}},
	)
	// 2 orgs + 1 team + 3 keys = 6
	if c.Len() != 6 {
		t.Errorf("Len() = %d, want 6", c.Len())
	}
}

// ---- Concurrent access -------------------------------------------------------

// TestMCPAccessCache_ConcurrentAccess verifies that concurrent Load and Check
// calls do not race (run with -race).
func TestMCPAccessCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := NewMCPAccessCache()
	c.Load(
		map[string][]string{"org-1": {"sv-target"}},
		nil,
		nil,
	)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// Writers reload the cache.
				c.Load(
					map[string][]string{"org-1": {"sv-target"}},
					nil,
					nil,
				)
			} else {
				// Readers check access.
				_ = c.Check("org-1", "", "key-1", "sv-target")
			}
		}(i)
	}

	wg.Wait()
}
