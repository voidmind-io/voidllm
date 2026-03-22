package license_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/license"
)

func TestCommunityLicense(t *testing.T) {
	t.Parallel()

	lic := license.Verify("", false)

	if got := lic.Edition(); got != license.EditionCommunity {
		t.Errorf("Edition() = %q, want %q", got, license.EditionCommunity)
	}
	if !lic.Valid() {
		t.Error("Valid() = false, want true")
	}
	if !lic.ExpiresAt().IsZero() {
		t.Errorf("ExpiresAt() = %v, want zero time", lic.ExpiresAt())
	}
	if got := lic.MaxOrgs(); got != license.CommunityMaxOrgs {
		t.Errorf("MaxOrgs() = %d, want %d", got, license.CommunityMaxOrgs)
	}
	if got := lic.MaxTeams(); got != license.CommunityMaxTeams {
		t.Errorf("MaxTeams() = %d, want %d", got, license.CommunityMaxTeams)
	}
	if got := lic.CustomerID(); got != "" {
		t.Errorf("CustomerID() = %q, want empty string", got)
	}
	if got := lic.Features(); len(got) != 0 {
		t.Errorf("Features() = %v, want empty slice", got)
	}

	allFeatures := []string{
		license.FeatureAuditLogs,
		license.FeatureOTelTracing,
		license.FeatureSSOOIDC,
		license.FeatureCustomRoles,
		license.FeatureMultiOrg,
	}
	for _, f := range allFeatures {
		if lic.HasFeature(f) {
			t.Errorf("HasFeature(%q) = true, want false", f)
		}
	}
}

func TestDevLicense(t *testing.T) {
	t.Parallel()

	lic := license.Verify("", true)

	if got := lic.Edition(); got != license.EditionDev {
		t.Errorf("Edition() = %q, want %q", got, license.EditionDev)
	}
	if !lic.Valid() {
		t.Error("Valid() = false, want true")
	}
	if !lic.ExpiresAt().IsZero() {
		t.Errorf("ExpiresAt() = %v, want zero time", lic.ExpiresAt())
	}
	if got := lic.MaxOrgs(); got != -1 {
		t.Errorf("MaxOrgs() = %d, want -1", got)
	}
	if got := lic.MaxTeams(); got != -1 {
		t.Errorf("MaxTeams() = %d, want -1", got)
	}
	if got := lic.CustomerID(); got != "" {
		t.Errorf("CustomerID() = %q, want empty string", got)
	}

	allFeatures := []string{
		license.FeatureAuditLogs,
		license.FeatureOTelTracing,
		license.FeatureSSOOIDC,
		license.FeatureCustomRoles,
		license.FeatureMultiOrg,
	}
	for _, f := range allFeatures {
		if !lic.HasFeature(f) {
			t.Errorf("HasFeature(%q) = false, want true", f)
		}
	}

	features := lic.Features()
	if len(features) == 0 {
		t.Error("Features() returned empty slice, want at least one feature")
	}
	featureSet := make(map[string]struct{}, len(features))
	for _, f := range features {
		featureSet[f] = struct{}{}
	}
	for _, f := range allFeatures {
		if _, ok := featureSet[f]; !ok {
			t.Errorf("Features() missing %q", f)
		}
	}
}
