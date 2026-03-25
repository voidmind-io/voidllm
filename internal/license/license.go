package license

import "time"

// Edition represents the product tier of a VoidLLM installation.
type Edition string

const (
	// EditionCommunity is the default open-source tier with no enterprise features.
	EditionCommunity Edition = "community"

	// EditionEnterprise is the paid tier unlocked by a valid signed JWT license key.
	EditionEnterprise Edition = "enterprise"

	// EditionDev is a special in-process tier used during development and testing.
	// It grants all features with no expiry and must never appear in production.
	EditionDev Edition = "dev"
)

// License describes the capabilities and constraints of a VoidLLM installation.
// Every method is safe to call concurrently. Verify always returns a non-nil
// License; callers never need to nil-check the return value.
type License interface {
	// Edition returns the product tier.
	Edition() Edition

	// Valid reports whether the license is currently active. A community
	// license is always valid. An enterprise license is valid until its
	// expiry timestamp. A dev license is always valid.
	Valid() bool

	// ExpiresAt returns the license expiry time. Community and dev licenses
	// return the zero time (no expiry).
	ExpiresAt() time.Time

	// HasFeature reports whether the named feature is available under this
	// license. Feature name constants are defined in features.go.
	HasFeature(feature string) bool

	// Features returns all feature names enabled by this license.
	Features() []string

	// MaxOrgs returns the maximum number of organizations permitted.
	// Returns -1 for unlimited (dev and fully licensed enterprise).
	MaxOrgs() int

	// MaxTeams returns the maximum number of teams permitted across all
	// organizations. Returns -1 for unlimited.
	MaxTeams() int

	// CustomerID returns the customer identifier embedded in an enterprise
	// license JWT. Returns an empty string for community and dev licenses.
	CustomerID() string
}

// communityLicense is the default license applied when no key is provided or
// when key verification fails. It never grants enterprise features.
type communityLicense struct{}

func (communityLicense) Edition() Edition       { return EditionCommunity }
func (communityLicense) Valid() bool            { return true }
func (communityLicense) ExpiresAt() time.Time   { return time.Time{} }
func (communityLicense) HasFeature(string) bool { return false }
func (communityLicense) Features() []string     { return []string{} }
func (communityLicense) MaxOrgs() int           { return CommunityMaxOrgs }
func (communityLicense) MaxTeams() int          { return CommunityMaxTeams }
func (communityLicense) CustomerID() string     { return "" }

// enterpriseLicense is constructed from a successfully verified JWT.
type enterpriseLicense struct {
	features    map[string]struct{}
	featureList []string
	expiresAt   time.Time
	maxOrgs     int
	maxTeams    int
	customerID  string
}

func (l *enterpriseLicense) Edition() Edition     { return EditionEnterprise }
func (l *enterpriseLicense) ExpiresAt() time.Time { return l.expiresAt }

func (l *enterpriseLicense) Valid() bool {
	if l.expiresAt.IsZero() {
		return true
	}
	return time.Now().UTC().Before(l.expiresAt)
}

func (l *enterpriseLicense) HasFeature(feature string) bool {
	_, ok := l.features[feature]
	return ok
}

func (l *enterpriseLicense) Features() []string {
	out := make([]string, len(l.featureList))
	copy(out, l.featureList)
	return out
}

func (l *enterpriseLicense) MaxOrgs() int       { return l.maxOrgs }
func (l *enterpriseLicense) MaxTeams() int      { return l.maxTeams }
func (l *enterpriseLicense) CustomerID() string { return l.customerID }

// devLicense is granted when VOIDLLM_ENTERPRISE_DEV is set. It enables all
// features with no expiry and must not be used in production builds.
type devLicense struct{}

func (devLicense) Edition() Edition     { return EditionDev }
func (devLicense) Valid() bool          { return true }
func (devLicense) ExpiresAt() time.Time { return time.Time{} }

func (devLicense) HasFeature(string) bool { return true }

func (devLicense) Features() []string {
	return []string{
		FeatureAuditLogs,
		FeatureOTelTracing,
		FeatureSSOOIDC,
		FeatureCustomRoles,
		FeatureMultiOrg,
		FeatureCostReports,
	}
}

func (devLicense) MaxOrgs() int       { return -1 }
func (devLicense) MaxTeams() int      { return -1 }
func (devLicense) CustomerID() string { return "" }
