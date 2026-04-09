// Package license provides enterprise license verification for VoidLLM.
// The community edition always starts regardless of license state — a
// missing or invalid license downgrades to community silently.
package license

// Feature name constants used throughout VoidLLM to gate enterprise
// functionality. Each constant matches the claim stored in the JWT.
const (
	// FeatureAuditLogs enables the async audit-log subsystem and the
	// GET /api/v1/audit-logs endpoint.
	FeatureAuditLogs = "audit_logs"

	// FeatureOTelTracing enables OpenTelemetry trace export.
	FeatureOTelTracing = "otel_tracing"

	// FeatureSSOOIDC enables OIDC/SSO login flows.
	FeatureSSOOIDC = "sso_oidc"

	// FeatureCustomRoles enables custom RBAC role definitions beyond the
	// built-in system_admin / org_admin / team_admin / member set.
	FeatureCustomRoles = "custom_roles"

	// FeatureMultiOrg enables more than CommunityMaxOrgs organizations to
	// be created within a single VoidLLM instance.
	FeatureMultiOrg = "multi_org"

	// FeatureCostReports enables cost analysis, budget alerts, and usage export.
	FeatureCostReports = "cost_reports"

	// FeatureFallbackChains enables cross-model fallback. When the primary
	// model is unavailable, the proxy automatically retries on the configured
	// fallback model.
	FeatureFallbackChains = "fallback_chains"
)

// CommunityMaxOrgs is the maximum number of organizations permitted on the
// community (unlicensed) edition.
const CommunityMaxOrgs = 1

// CommunityMaxTeams is the maximum number of teams permitted on the community
// (unlicensed) edition.
const CommunityMaxTeams = 3
