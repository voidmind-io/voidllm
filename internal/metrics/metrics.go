// Package metrics defines application-level Prometheus metrics for VoidLLM.
// All vars are registered with the default Prometheus registry via promauto,
// so they appear automatically on the /metrics endpoint served by the health
// package without any additional wiring.
package metrics

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// UpstreamRequestsTotal counts requests successfully dispatched to upstream
// providers, partitioned by model name, upstream provider, and the HTTP status
// code returned by the upstream. Requests rejected before the upstream call
// (auth failures, rate limits, model not found, etc.) are not counted here.
var UpstreamRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "voidllm_upstream_requests_total",
	Help: "Total requests dispatched to upstream providers.",
}, []string{"model", "provider", "status"})

// ProxyDurationSeconds observes the end-to-end wall-clock duration of each
// proxied request in seconds, labelled by model name and whether the response
// was streamed ("true") or buffered ("false").
var ProxyDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "voidllm_proxy_duration_seconds",
	Help:    "Duration of proxied requests in seconds.",
	Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
}, []string{"model", "stream"})

// ProxyTTFTSeconds observes the time-to-first-token latency for streaming
// requests in seconds, labelled by model name. Non-streaming requests are not
// recorded here; use ProxyDurationSeconds for those.
var ProxyTTFTSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "voidllm_proxy_ttft_seconds",
	Help:    "Time to first token for streaming requests in seconds.",
	Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
}, []string{"model"})

// TokensTotal counts tokens processed by the proxy, partitioned by model name
// and direction: "prompt" for input tokens and "completion" for output tokens.
var TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "voidllm_tokens_total",
	Help: "Total tokens processed by the proxy.",
}, []string{"model", "direction"})

// ActiveStreams is a gauge tracking the number of currently active streaming
// SSE connections. It is incremented when a streaming goroutine starts and
// decremented when it exits.
var ActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "voidllm_active_streams",
	Help: "Number of currently active streaming connections.",
})

// UpstreamErrorsTotal counts transport-level failures when contacting upstream
// providers (i.e. the HTTP client returned a non-nil error), partitioned by
// model name and provider.
var UpstreamErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "voidllm_upstream_errors_total",
	Help: "Total upstream provider errors.",
}, []string{"model", "provider"})

// RateLimitRejectionsTotal counts requests rejected before being proxied due
// to rate or token budget limits, partitioned by scope: "request" for
// requests-per-minute/day limits and "token" for daily/monthly token budgets.
var RateLimitRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "voidllm_ratelimit_rejections_total",
	Help: "Total requests rejected by rate limiting.",
}, []string{"scope"})

// CircuitBreakerRejectionsTotal counts requests rejected because the circuit
// breaker for a model is in the open state, partitioned by model name.
var CircuitBreakerRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "voidllm_circuitbreaker_rejections_total",
	Help: "Total requests rejected because the circuit breaker was open.",
}, []string{"model"})

// CacheSize tracks the number of entries in each in-memory cache. The "cache"
// label identifies which cache is being measured: "keys", "models", "access",
// or "aliases". Updated periodically by a background ticker in app.Start.
var CacheSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "voidllm_cache_size",
	Help: "Number of entries in each in-memory cache.",
}, []string{"cache"})

// ModelHealthStatus tracks the current health status of each upstream model as
// a gauge value: 1 = healthy, 0.5 = degraded, 0 = unhealthy or unknown.
// The "status" label carries the string status ("healthy", "degraded",
// "unhealthy", "unknown") alongside the numeric value for easy querying.
var ModelHealthStatus = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "voidllm",
		Name:      "model_health_status",
		Help:      "Current health status of upstream models (1=healthy, 0.5=degraded, 0=unhealthy).",
	},
	[]string{"model", "status"},
)

// ModelHealthLatencySeconds tracks the most recently observed health check
// round-trip latency in seconds, labelled by model name. Updated after each
// successful probe cycle.
var ModelHealthLatencySeconds = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "voidllm",
		Name:      "model_health_latency_seconds",
		Help:      "Last health check latency in seconds per model.",
	},
	[]string{"model"},
)

// RoutingRetriesTotal counts the number of retry attempts during
// multi-deployment load balancing, partitioned by model name and routing
// strategy. It is incremented each time the proxy abandons a failing
// deployment candidate and moves on to the next one in the ordered list.
var RoutingRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "voidllm",
	Name:      "routing_retries_total",
	Help:      "Number of upstream retry attempts during load-balanced routing.",
}, []string{"model", "strategy"})

// RegisterDBCollectors registers database connection pool metrics against the
// default Prometheus registry. The gauges are implemented as GaugeFuncs that
// read live values from sql.DB.Stats() on each scrape, so they always reflect
// the current pool state without any additional polling.
// Must be called after the database is opened.
func RegisterDBCollectors(sqlDB *sql.DB) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "voidllm_db_open_connections",
			Help: "Number of open database connections.",
		},
		func() float64 { return float64(sqlDB.Stats().OpenConnections) },
	))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "voidllm_db_idle_connections",
			Help: "Number of idle database connections.",
		},
		func() float64 { return float64(sqlDB.Stats().Idle) },
	))
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "voidllm_db_wait_count_total",
			Help: "Total number of connections waited for.",
		},
		func() float64 { return float64(sqlDB.Stats().WaitCount) },
	))
}

// MCPToolCallsTotal counts MCP tool calls proxied to external servers, partitioned
// by server alias, tool name, and call status ("success", "error", or "timeout").
var MCPToolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "voidllm",
	Name:      "mcp_tool_calls_total",
	Help:      "Total MCP tool calls proxied to external servers.",
}, []string{"server", "tool", "status"})

// MCPToolCallDurationSeconds observes the round-trip duration of each proxied
// MCP tool call in seconds, labelled by server alias and tool name.
var MCPToolCallDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "voidllm",
	Name:      "mcp_tool_call_duration_seconds",
	Help:      "Duration of proxied MCP tool calls.",
	Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
}, []string{"server", "tool"})

// MCPTransportErrorsTotal counts transport-level failures when contacting
// external MCP servers (i.e. the HTTP client returned a non-nil error),
// partitioned by server alias and error type.
var MCPTransportErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "voidllm",
	Name:      "mcp_transport_errors_total",
	Help:      "Total MCP transport errors when proxying to external servers.",
}, []string{"server", "error_type"})

// Code Mode metrics

// CodeModeExecutionsTotal counts Code Mode script executions by outcome.
var CodeModeExecutionsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "voidllm",
		Name:      "code_mode_executions_total",
		Help:      "Total Code Mode script executions by status.",
	},
	[]string{"status"}, // "success", "error", "timeout", "oom"
)

// CodeModeExecutionDurationSeconds observes the total wall-clock duration of
// Code Mode executions, including time spent on tool calls.
var CodeModeExecutionDurationSeconds = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "voidllm",
		Name:      "code_mode_execution_duration_seconds",
		Help:      "Code Mode total execution time including tool calls.",
		Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30},
	},
)

// CodeModeToolCallsPerExecution observes the number of MCP tool calls made
// within a single Code Mode execution.
var CodeModeToolCallsPerExecution = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "voidllm",
		Name:      "code_mode_tool_calls_per_execution",
		Help:      "Number of MCP tool calls per Code Mode execution.",
		Buckets:   []float64{1, 2, 3, 5, 10, 20, 50},
	},
)

// CodeModePoolAvailable tracks the number of idle QJS runtimes in the pool.
var CodeModePoolAvailable = promauto.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "voidllm",
		Name:      "code_mode_pool_available",
		Help:      "Number of available QJS runtimes in the Code Mode pool.",
	},
)

// RegisterUsageCollector registers the usage buffer depth metric against the
// default Prometheus registry. The gauge is implemented as a GaugeFunc that
// reads the current channel length on each scrape.
// Must be called after the usage logger is constructed.
func RegisterUsageCollector(logger interface{ BufferLen() int }) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "voidllm_usage_buffer_depth",
			Help: "Number of events buffered in the usage logger.",
		},
		func() float64 { return float64(logger.BufferLen()) },
	))
}
