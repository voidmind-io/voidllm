package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
)

// MCPServerHealth holds the most recent health state for a single registered
// MCP server. All fields are safe to read after being retrieved from
// MCPHealthChecker.GetHealth or MCPHealthChecker.GetAllHealth — stored values
// are never mutated in place.
type MCPServerHealth struct {
	// ServerID is the database ID of the MCP server.
	ServerID string `json:"server_id"`
	// ServerName is the display name of the MCP server.
	ServerName string `json:"server_name"`
	// Alias is the routing alias of the MCP server.
	Alias string `json:"alias"`
	// Status is the health classification from the most recent probe:
	// "healthy", "unhealthy", or "unknown".
	Status string `json:"status"`
	// LastCheck is the UTC timestamp of the most recent probe attempt.
	LastCheck time.Time `json:"last_check"`
	// LastError holds the sanitized error message from the most recently failed
	// probe, or is empty when the last probe succeeded.
	LastError string `json:"last_error,omitempty"`
	// LatencyMs is the round-trip duration of the most recent successful probe
	// in milliseconds. Zero when no probe has succeeded yet.
	LatencyMs int64 `json:"latency_ms"`
	// ToolCount is the number of tools reported by the server during the most
	// recent successful tools/list probe. Zero when no probe has succeeded yet.
	ToolCount int `json:"tool_count"`
}

// MCPServerTarget holds the minimal fields needed to probe a single MCP
// server. The health checker receives targets via the servers callback,
// which the caller builds from the in-memory MCPServerCache so that no
// database I/O occurs during probe cycles. It carries no endpoint or
// authentication details — those live on the *mcp.HTTPTransport the checker
// resolves separately via the transportFor callback (see
// NewMCPHealthChecker), so this type only needs to identify the server and
// tell the checker how to label and route around it.
type MCPServerTarget struct {
	// ID is the database ID of the MCP server (used as the sync.Map key and
	// as the lookup key passed to transportFor).
	ID string
	// Name is the display name used in logs and health results.
	Name string
	// Alias is the routing alias used in Prometheus metric labels.
	Alias string
	// Source is the origin of the server definition: "api", "yaml", or "builtin".
	// Built-in servers are skipped during health probes.
	Source string
}

// MCPHealthChecker periodically probes registered MCP servers via
// *mcp.HTTPTransport.ListTools and stores the results in memory. All methods
// are safe for concurrent use.
//
// The checker does not speak JSON-RPC or HTTP itself. It resolves each
// target's *mcp.HTTPTransport through the transportFor callback and delegates
// the actual probe to ListTools, which already knows how to negotiate the
// protocol era, run the legacy handshake when needed, and set the required
// headers and _meta for the modern era (docs/mcp-v2.md §2, §4.2) — exactly
// what a health probe needs, including the tool count and latency. This
// keeps auth, SSRF hardening, body-size limiting, and redirect prohibition
// defined in exactly one place (internal/mcp) instead of being duplicated
// here.
//
// Two side effects of reusing ListTools are deliberate, not incidental:
//   - The first health probe of a newly registered server is also that
//     server's first era probe. By the time the proxy path serves its first
//     real request, the era binding is already resolved and cached on the
//     shared *mcp.HTTPTransport (see HTTPTransport.resolveBinding), so that
//     request never pays the probe cost itself.
//   - ListTools always calls Call with the empty SessionScope (tool
//     discovery is not performed on behalf of any one organization — see
//     ListTools's doc), which is the same scope health probing uses. A
//     legacy upstream's warmup handshake therefore runs at most once for
//     that scope's lifetime, shared between health probing and tool
//     discovery, rather than once per probe cycle.
type MCPHealthChecker struct {
	// servers is a callback that returns the current list of probe targets.
	// It is called at the start of every probe cycle so newly added or removed
	// servers are picked up without restarting the checker.
	servers func() []MCPServerTarget
	// transportFor returns the resolved transport for a server ID, or false
	// when the transport cache has no entry yet. Supplied as a callback so
	// this package stays independent of internal/proxy — the same reason the
	// servers callback exists.
	transportFor func(serverID string) (*mcp.HTTPTransport, bool)
	results      sync.Map // serverID -> *MCPServerHealth (replaced atomically)
	interval     time.Duration
	log          *slog.Logger
}

// NewMCPHealthChecker constructs an MCPHealthChecker that calls servers to
// retrieve the current list of targets, resolves each target's transport via
// transportFor, and probes each one at the given interval. interval must be
// positive; the caller is responsible for reading the configured value from
// config.MCPHealthConfig.
func NewMCPHealthChecker(servers func() []MCPServerTarget, transportFor func(serverID string) (*mcp.HTTPTransport, bool), interval time.Duration, log *slog.Logger) *MCPHealthChecker {
	return &MCPHealthChecker{
		servers:      servers,
		transportFor: transportFor,
		interval:     interval,
		log:          log,
	}
}

// GetHealth returns the most recent health state for the server identified by
// serverID. It returns a zero MCPServerHealth with Status "unknown" when the
// server has not yet been probed.
// The returned value is safe to read without further synchronization.
func (c *MCPHealthChecker) GetHealth(serverID string) MCPServerHealth {
	v, ok := c.results.Load(serverID)
	if !ok {
		return MCPServerHealth{ServerID: serverID, Status: "unknown"}
	}
	return *v.(*MCPServerHealth)
}

// GetAllHealth returns a snapshot of health state for every MCP server that
// has been probed at least once. The returned slice is unordered.
func (c *MCPHealthChecker) GetAllHealth() []MCPServerHealth {
	var out []MCPServerHealth
	c.results.Range(func(_, v any) bool {
		h := v.(*MCPServerHealth)
		out = append(out, *h)
		return true
	})
	return out
}

// Start immediately runs a first probe cycle for all current targets and then
// starts a ticker that repeats the cycle at the configured interval. The
// returned function stops the background goroutine and waits for it to exit.
func (c *MCPHealthChecker) Start() func() {
	c.runAll()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.runAll()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// maxProbeConcurrency is the maximum number of concurrent health probes run
// during a single cycle. This prevents a large number of registered MCP
// servers from overwhelming the network or the runtime scheduler.
const maxProbeConcurrency = 5

// runAll probes every target returned by the servers callback with bounded
// concurrency (up to maxProbeConcurrency simultaneous probes). After all
// active targets have been probed, health records and Prometheus metrics for
// servers that are no longer present are removed.
func (c *MCPHealthChecker) runAll() {
	targets := c.servers()
	active := make(map[string]struct{}, len(targets))

	sem := make(chan struct{}, maxProbeConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		active[t.ID] = struct{}{}
		if t.Source == "builtin" {
			c.results.Store(t.ID, &MCPServerHealth{
				ServerID: t.ID, ServerName: t.Name, Alias: t.Alias,
				Status: "healthy", LastCheck: time.Now().UTC(),
			})
			updateMCPMetrics(t.Name, t.Alias, &MCPServerHealth{Status: "healthy"})
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(target MCPServerTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			c.runOne(target)
		}(t)
	}
	wg.Wait()

	// Remove health records for servers that are no longer in the active set.
	// This keeps the sync.Map and Prometheus metrics consistent with the
	// current server registry without requiring a restart.
	c.results.Range(func(key, value any) bool {
		id := key.(string)
		if _, ok := active[id]; !ok {
			c.results.Delete(key)
			h := value.(*MCPServerHealth)
			metrics.MCPServerHealthStatus.DeleteLabelValues(h.ServerName, h.Alias)
			metrics.MCPServerHealthLatency.DeleteLabelValues(h.ServerName, h.Alias)
		}
		return true
	})
}

// runOne resolves t's transport via transportFor and, when found, probes it
// with ListTools, storing the result using copy-on-write to avoid data races
// with concurrent readers.
//
// When transportFor reports no transport for t.ID — a cold start, or a
// server registered between two transport-cache refreshes — this probe cycle
// skips t entirely: no result is stored or updated, and t's existing record
// (if any) is left exactly as it was. A server that has never been probed
// therefore keeps reporting Status "unknown" via GetHealth's zero-value
// fallback, which is deliberately distinct from "unhealthy": VoidLLM has not
// checked it yet, which is not the same claim as having checked it and found
// it broken. The next probe cycle tries again once the transport cache has
// caught up.
func (c *MCPHealthChecker) runOne(t MCPServerTarget) {
	transport, ok := c.transportFor(t.ID)
	if !ok {
		c.log.LogAttrs(context.Background(), slog.LevelDebug, "mcp health probe skipped: transport not cached yet",
			slog.String("server_id", t.ID),
			slog.String("alias", t.Alias),
		)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	start := time.Now()
	listing, err := transport.ListTools(ctx)
	latencyMs := time.Since(start).Milliseconds()
	if latencyMs == 0 {
		latencyMs = 1
	}

	// Load existing state or seed a zero value so the copy-on-write always
	// starts from a consistent base.
	existing, _ := c.results.LoadOrStore(t.ID, &MCPServerHealth{
		ServerID:   t.ID,
		ServerName: t.Name,
		Alias:      t.Alias,
		Status:     "unknown",
	})
	old := existing.(*MCPServerHealth)

	updated := *old
	updated.ServerID = t.ID
	updated.ServerName = t.Name
	updated.Alias = t.Alias
	updated.LastCheck = time.Now().UTC()

	if err == nil {
		updated.Status = "healthy"
		updated.LatencyMs = latencyMs
		updated.ToolCount = len(listing.Tools)
		updated.LastError = ""
	} else {
		updated.Status = "unhealthy"
		// sanitizeError strips upstream-controlled text; ListTools itself
		// already reduces a JSON-RPC error to its numeric code rather than
		// the upstream's free-form message (see ListTools's doc), so this
		// never has upstream text to strip in the first place — but every
		// other failure returned by ListTools (a transport error, an HTTP
		// status) still goes through the same sanitizer for consistency.
		updated.LastError = sanitizeError(err)
		c.log.LogAttrs(ctx, slog.LevelDebug, "mcp health probe failed",
			slog.String("server_id", t.ID),
			slog.String("alias", t.Alias),
			slog.String("error", updated.LastError),
		)
	}

	c.results.Store(t.ID, &updated)
	updateMCPMetrics(t.Name, t.Alias, &updated)
}

// updateMCPMetrics refreshes the Prometheus gauges for the given MCP server
// after a probe cycle completes.
func updateMCPMetrics(name, alias string, mh *MCPServerHealth) {
	var statusVal float64
	if mh.Status == "healthy" {
		statusVal = 1
	}
	metrics.MCPServerHealthStatus.WithLabelValues(name, alias).Set(statusVal)

	if mh.LatencyMs > 0 {
		metrics.MCPServerHealthLatency.WithLabelValues(name, alias).Set(float64(mh.LatencyMs) / 1000)
	}
}
