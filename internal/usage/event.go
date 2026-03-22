// Package usage provides async usage event logging for the proxy hot path.
// Events are enqueued via Log() (non-blocking) and written to the database
// in batches by a background goroutine.
package usage

// Event represents a single proxy request for usage tracking. All fields are
// populated by the proxy handler immediately after the response is sent; none
// of them are computed inside the logger.
type Event struct {
	// KeyID is the unique identifier of the API key that made the request.
	KeyID string
	// KeyType is the category of the key: user_key, team_key, or sa_key.
	KeyType string
	// OrgID is the organization the key belongs to.
	OrgID string
	// TeamID is the team the key is scoped to. Empty if not team-scoped.
	TeamID string
	// UserID is the user the key belongs to. Empty if not user-scoped.
	UserID string
	// ServiceAccountID is the service account the key belongs to. Empty if not a SA key.
	ServiceAccountID string
	// ModelName is the canonical upstream model name.
	ModelName string
	// PromptTokens is the number of input tokens consumed.
	PromptTokens int
	// CompletionTokens is the number of output tokens produced.
	CompletionTokens int
	// TotalTokens is the sum of prompt and completion tokens.
	TotalTokens int
	// CostEstimate is the estimated cost in USD, or nil if pricing is not configured.
	CostEstimate *float64
	// DurationMS is the total request duration in milliseconds.
	DurationMS int
	// TTFT_MS is the time to first token in milliseconds. Nil for non-streaming requests.
	TTFT_MS *int
	// TokensPerSecond is the generation throughput. Nil when unavailable.
	TokensPerSecond *float64
	// StatusCode is the HTTP status code returned to the client.
	StatusCode int
	// RequestID is the per-request trace ID set by the request ID middleware.
	// It correlates the usage record with the proxy access log and audit log.
	RequestID string
}
