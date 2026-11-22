package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// summaryPageSize is deliberately smaller than the fixture sizes so the list
// side of every comparison has to walk several pages.
const summaryPageSize = 4

func tsIn(d time.Duration) *string {
	s := FormatTimestamp(time.Now().Add(d))
	return &s
}

func TestSummarizeAPIKeys(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sum-keys"})
	other := mustCreateOrg(t, d, CreateOrgParams{Name: "Other", Slug: "org-sum-keys-other"})
	u1 := mustCreateUser(t, d, CreateUserParams{Email: "u1@sum.example", DisplayName: "U1"})
	u2 := mustCreateUser(t, d, CreateUserParams{Email: "u2@sum.example", DisplayName: "U2"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sum-keys"})

	n := 0
	mk := func(orgID, userID string, teamID *string, exp *string) *APIKey {
		n++
		plain := fmt.Sprintf("vl_uk_summary%04d", n)
		return mustCreateAPIKey(t, d, CreateAPIKeyParams{
			KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
			KeyType: keygen.KeyTypeUser, Name: plain, OrgID: orgID,
			UserID: ptr(userID), TeamID: teamID, ExpiresAt: exp, CreatedBy: userID,
		})
	}

	for range 6 { // u1: no expiry
		mk(org.ID, u1.ID, nil, nil)
	}
	for range 3 { // u1 team keys: far future
		mk(org.ID, u1.ID, &team.ID, tsIn(30*24*time.Hour))
	}
	for range 2 { // u2: expiring soon
		mk(org.ID, u2.ID, nil, tsIn(48*time.Hour))
	}
	for range 2 { // u2: already expired
		mk(org.ID, u2.ID, &team.ID, tsIn(-time.Hour))
	}
	deleted := mk(org.ID, u2.ID, nil, nil)
	if err := d.DeleteAPIKey(ctx, deleted.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	mk(other.ID, u1.ID, nil, nil) // other org, never visible

	// 'session_key' rows must never be counted.
	n++
	sessPlain := fmt.Sprintf("vl_sk_summary%04d", n)
	mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(sessPlain, testHMACSecret), KeyHint: "sess", KeyType: "session_key",
		Name: "session", OrgID: org.ID, UserID: ptr(u1.ID), CreatedBy: u1.ID,
	})

	tests := []struct {
		name           string
		userID, teamID string
		includeDeleted bool
		want           APIKeysSummary
	}{
		{"whole org", "", "", false, APIKeysSummary{Total: 13, Active: 11, ExpiringSoon: 2}},
		{"user filter", u1.ID, "", false, APIKeysSummary{Total: 9, Active: 9, ExpiringSoon: 0}},
		{"team filter", "", team.ID, false, APIKeysSummary{Total: 5, Active: 3, ExpiringSoon: 0}},
		{"include deleted", "", "", true, APIKeysSummary{Total: 14, Active: 11, ExpiringSoon: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.SummarizeAPIKeys(ctx, org.ID, tt.userID, tt.teamID, tt.includeDeleted)
			if err != nil {
				t.Fatalf("SummarizeAPIKeys: %v", err)
			}
			if *got != tt.want {
				t.Errorf("summary = %+v, want %+v", *got, tt.want)
			}

			// The summary must equal counting every page of the list.
			var all []APIKey
			cursor := ""
			for {
				page, err := d.ListAPIKeys(ctx, org.ID, tt.userID, tt.teamID, cursor, summaryPageSize, tt.includeDeleted)
				if err != nil {
					t.Fatalf("ListAPIKeys: %v", err)
				}
				all = append(all, page...)
				if len(page) < summaryPageSize {
					break
				}
				cursor = page[len(page)-1].ID
			}
			if len(all) != got.Total {
				t.Errorf("paged list total = %d, summary total = %d", len(all), got.Total)
			}
		})
	}
}

func TestSummarizeTeams(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sum-teams"})
	u1 := mustCreateUser(t, d, CreateUserParams{Email: "t1@sum.example", DisplayName: "U1"})
	u2 := mustCreateUser(t, d, CreateUserParams{Email: "t2@sum.example", DisplayName: "U2"})

	var teams []*Team
	for i := range 6 {
		teams = append(teams, mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: fmt.Sprintf("T%d", i), Slug: fmt.Sprintf("t-sum-%d", i)}))
	}
	// u1 is in teams 0..2, u2 in teams 2..3, so 2 distinct members overall.
	for _, tm := range []struct{ team, user string }{
		{teams[0].ID, u1.ID}, {teams[1].ID, u1.ID}, {teams[2].ID, u1.ID},
		{teams[2].ID, u2.ID}, {teams[3].ID, u2.ID},
	} {
		mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: tm.team, UserID: tm.user, Role: "member"})
	}
	n := 0
	mkKey := func(teamID string, exp *string) *APIKey {
		n++
		plain := fmt.Sprintf("vl_tk_summary%04d", n)
		return mustCreateAPIKey(t, d, CreateAPIKeyParams{
			KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
			KeyType: keygen.KeyTypeTeam, Name: plain, OrgID: org.ID, TeamID: ptr(teamID), CreatedBy: u1.ID,
		})
	}
	mkKey(teams[0].ID, nil)
	mkKey(teams[0].ID, nil)
	mkKey(teams[2].ID, nil)
	gone := mkKey(teams[2].ID, nil)
	if err := d.DeleteAPIKey(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if err := d.DeleteTeam(ctx, teams[5].ID); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}

	t.Run("org scope excludes deleted and matches paged list", func(t *testing.T) {
		got, err := d.SummarizeTeams(ctx, org.ID, false)
		if err != nil {
			t.Fatalf("SummarizeTeams: %v", err)
		}
		want := TeamsSummary{Total: 5, Members: 2, Keys: 3}
		if *got != want {
			t.Errorf("summary = %+v, want %+v", *got, want)
		}
		var all []TeamWithCounts
		cursor := ""
		for {
			page, err := d.ListTeamsWithCounts(ctx, org.ID, cursor, summaryPageSize, false)
			if err != nil {
				t.Fatalf("ListTeamsWithCounts: %v", err)
			}
			all = append(all, page...)
			if len(page) < summaryPageSize {
				break
			}
			cursor = page[len(page)-1].ID
		}
		keys := 0
		for _, tm := range all {
			keys += tm.KeyCount
		}
		if len(all) != got.Total || keys != got.Keys {
			t.Errorf("paged list total/keys = %d/%d, summary = %d/%d", len(all), keys, got.Total, got.Keys)
		}
	})

	t.Run("include deleted", func(t *testing.T) {
		got, err := d.SummarizeTeams(ctx, org.ID, true)
		if err != nil {
			t.Fatalf("SummarizeTeams: %v", err)
		}
		if got.Total != 6 {
			t.Errorf("Total = %d, want 6", got.Total)
		}
	})

	t.Run("user teams scope", func(t *testing.T) {
		got, err := d.SummarizeUserTeams(ctx, org.ID, u2.ID)
		if err != nil {
			t.Fatalf("SummarizeUserTeams: %v", err)
		}
		list, err := d.ListUserTeams(ctx, org.ID, u2.ID)
		if err != nil {
			t.Fatalf("ListUserTeams: %v", err)
		}
		keys := 0
		for _, tm := range list {
			keys += tm.KeyCount
		}
		want := TeamsSummary{Total: len(list), Members: 2, Keys: keys}
		if *got != want || got.Total != 2 {
			t.Errorf("summary = %+v, want %+v (total 2)", *got, want)
		}
	})
}

func TestSummarizeOrgMemberships(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sum-mem"})
	other := mustCreateOrg(t, d, CreateOrgParams{Name: "Other", Slug: "org-sum-mem-other"})
	for i := range 9 {
		u := mustCreateUser(t, d, CreateUserParams{Email: fmt.Sprintf("m%d@sum.example", i), DisplayName: "M"})
		role := "member"
		if i < 2 {
			role = "org_admin"
		}
		mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: u.ID, Role: role})
	}
	ou := mustCreateUser(t, d, CreateUserParams{Email: "o@sum.example", DisplayName: "O"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: other.ID, UserID: ou.ID, Role: "org_admin"})

	got, err := d.SummarizeOrgMemberships(ctx, org.ID)
	if err != nil {
		t.Fatalf("SummarizeOrgMemberships: %v", err)
	}
	if want := (OrgMembershipsSummary{Total: 9, Admins: 2}); *got != want {
		t.Errorf("summary = %+v, want %+v", *got, want)
	}
}

func TestSummarizeServiceAccounts(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sum-sa"})
	u1 := mustCreateUser(t, d, CreateUserParams{Email: "s1@sum.example", DisplayName: "U1"})
	u2 := mustCreateUser(t, d, CreateUserParams{Email: "s2@sum.example", DisplayName: "U2"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sum-sa"})

	for i := range 6 {
		mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: fmt.Sprintf("o%d", i), OrgID: org.ID, CreatedBy: u1.ID})
	}
	for i := range 3 {
		mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: fmt.Sprintf("t%d", i), OrgID: org.ID, TeamID: &team.ID, CreatedBy: u2.ID})
	}
	gone := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "gone", OrgID: org.ID, CreatedBy: u2.ID})
	if err := d.DeleteServiceAccount(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteServiceAccount: %v", err)
	}

	tests := []struct {
		name           string
		createdBy      string
		includeDeleted bool
		want           ServiceAccountsSummary
	}{
		{"all", "", false, ServiceAccountsSummary{Total: 9, OrgScoped: 6, TeamScoped: 3}},
		{"created_by filter", u2.ID, false, ServiceAccountsSummary{Total: 3, OrgScoped: 0, TeamScoped: 3}},
		{"include deleted", "", true, ServiceAccountsSummary{Total: 10, OrgScoped: 7, TeamScoped: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.SummarizeServiceAccounts(ctx, org.ID, tt.createdBy, tt.includeDeleted)
			if err != nil {
				t.Fatalf("SummarizeServiceAccounts: %v", err)
			}
			if *got != tt.want {
				t.Errorf("summary = %+v, want %+v", *got, tt.want)
			}
			var total int
			cursor := ""
			for {
				page, err := d.ListServiceAccountsWithCounts(ctx, org.ID, tt.createdBy, cursor, summaryPageSize, tt.includeDeleted)
				if err != nil {
					t.Fatalf("ListServiceAccountsWithCounts: %v", err)
				}
				total += len(page)
				if len(page) < summaryPageSize {
					break
				}
				cursor = page[len(page)-1].ID
			}
			if total != got.Total {
				t.Errorf("paged list total = %d, summary total = %d", total, got.Total)
			}
		})
	}
}

func TestSummarizeUsers(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	for i := range 7 {
		p := CreateUserParams{Email: fmt.Sprintf("usr%d@sum.example", i), DisplayName: "U"}
		switch {
		case i < 2:
			p.IsSystemAdmin = true
		case i < 5:
			p.AuthProvider = "oidc"
			p.ExternalID = ptr(fmt.Sprintf("ext-%d", i))
		}
		mustCreateUser(t, d, p)
	}
	gone := mustCreateUser(t, d, CreateUserParams{Email: "gone@sum.example", DisplayName: "G"})
	if err := d.DeleteUser(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	tests := []struct {
		name           string
		includeDeleted bool
		want           UsersSummary
	}{
		{"active only", false, UsersSummary{Total: 7, SystemAdmins: 2, SSO: 3}},
		{"include deleted", true, UsersSummary{Total: 8, SystemAdmins: 2, SSO: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.SummarizeUsers(ctx, tt.includeDeleted)
			if err != nil {
				t.Fatalf("SummarizeUsers: %v", err)
			}
			if *got != tt.want {
				t.Errorf("summary = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestSummarizeOrgs(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	u := mustCreateUser(t, d, CreateUserParams{Email: "org@sum.example", DisplayName: "U"})
	var orgs []*Org
	for i := range 6 {
		orgs = append(orgs, mustCreateOrg(t, d, CreateOrgParams{Name: fmt.Sprintf("O%d", i), Slug: fmt.Sprintf("o-sum-%d", i)}))
	}
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgs[0].ID, UserID: u.ID, Role: "member"})
	mustCreateTeam(t, d, CreateTeamParams{OrgID: orgs[0].ID, Name: "A", Slug: "a-sum-orgs"})
	mustCreateTeam(t, d, CreateTeamParams{OrgID: orgs[1].ID, Name: "B", Slug: "b-sum-orgs"})
	gone := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgs[1].ID, Name: "C", Slug: "c-sum-orgs"})
	if err := d.DeleteTeam(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if err := d.DeleteOrg(ctx, orgs[5].ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	for _, tt := range []struct {
		name           string
		includeDeleted bool
		wantTotal      int
	}{{"active only", false, 5}, {"include deleted", true, 6}} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.SummarizeOrgs(ctx, tt.includeDeleted)
			if err != nil {
				t.Fatalf("SummarizeOrgs: %v", err)
			}
			var total, members, teams int
			cursor := ""
			for {
				page, err := d.ListOrgsWithCounts(ctx, cursor, summaryPageSize, tt.includeDeleted)
				if err != nil {
					t.Fatalf("ListOrgsWithCounts: %v", err)
				}
				for _, o := range page {
					total++
					members += o.MemberCount
					teams += o.TeamCount
				}
				if len(page) < summaryPageSize {
					break
				}
				cursor = page[len(page)-1].ID
			}
			want := OrgsSummary{Total: total, Members: members, Teams: teams}
			if *got != want || got.Total != tt.wantTotal {
				t.Errorf("summary = %+v, want %+v (total %d)", *got, want, tt.wantTotal)
			}
		})
	}
}
