// Package audit provides async audit logging for admin API mutations.
// Events are collected in a buffered channel and flushed to the database in
// batches, following the same pattern as the usage logger. The proxy hot path
// is never touched — audit logging is admin-only.
package audit

import "time"

// Event represents a single auditable action performed via the Admin API.
type Event struct {
	// ID is the UUIDv7 identifier assigned during flush. Callers do not set this.
	ID string
	// Timestamp is when the action occurred. Callers must set this to time.Now().UTC().
	Timestamp time.Time
	// OrgID is the organization context in which the action was performed.
	OrgID string
	// ActorID is the user or service account ID that performed the action.
	ActorID string
	// ActorType describes the kind of actor: "user", "service_account", or "system".
	ActorType string
	// ActorKeyID is the api_keys.id of the key used for the action. Empty for
	// non-key-authenticated actions such as internal system events.
	ActorKeyID string
	// Action is the verb describing what occurred: "create", "update", "delete",
	// "revoke", "activate", "deactivate", "login", or "logout".
	Action string
	// ResourceType identifies the kind of resource affected: "org", "team",
	// "user", "key", "model", "service_account", "invite", "membership",
	// "model_access", "model_alias", or "session".
	ResourceType string
	// ResourceID is the identifier of the affected resource. Empty for
	// collection-level actions such as create (the new ID is not yet known
	// at request time) or login.
	ResourceID string
	// Description is a human-readable summary of the action.
	Description string
	// IPAddress is the client IP address extracted from the request.
	IPAddress string
	// StatusCode is the HTTP status code returned by the handler.
	StatusCode int
	// RequestID is the per-request trace ID set by the request ID middleware.
	// It correlates the audit record with the proxy access log and usage record.
	RequestID string
}
