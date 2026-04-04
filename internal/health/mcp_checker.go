package health

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
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

// MCPServerTarget holds the minimal fields needed to probe a single MCP server.
// The health checker receives targets via the servers callback, which the caller
// builds from the in-memory cache so that no database I/O occurs on the hot path.
type MCPServerTarget struct {
	// ID is the database ID of the MCP server (used as the sync.Map key).
	ID string
	// Name is the display name used in logs and health results.
	Name string
	// Alias is the routing alias used in Prometheus metric labels.
	Alias string
	// URL is the full endpoint URL that receives the JSON-RPC POST.
	URL string
	// AuthType is the authentication scheme: "none", "bearer", or "header".
	AuthType string
	// AuthToken is the plaintext (decrypted) authentication token. Empty when
	// AuthType is "none".
	AuthToken string
	// AuthHeader is the custom header name used when AuthType is "header".
	AuthHeader string
	// Source is the origin of the server definition: "api", "yaml", or "builtin".
	// Built-in servers are skipped during health probes.
	Source string
}

// MCPHealthChecker periodically probes registered MCP servers via a
// tools/list JSON-RPC request and stores the results in memory. All methods
// are safe for concurrent use.
type MCPHealthChecker struct {
	// servers is a callback that returns the current list of probe targets.
	// It is called at the start of every probe cycle so newly added or removed
	// servers are picked up without restarting the checker.
	servers  func() []MCPServerTarget
	results  sync.Map // serverID -> *MCPServerHealth (replaced atomically)
	interval time.Duration
	client   *http.Client
	log      *slog.Logger
}

// toolsListPayload is the JSON-RPC 2.0 request body sent to each MCP server.
var toolsListPayload = []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

// NewMCPHealthChecker constructs an MCPHealthChecker that calls servers to
// retrieve the current list of targets and probes each one at the given
// interval. interval must be positive; the caller is responsible for reading
// the configured value from config.MCPHealthConfig.
//
// When allowPrivateURLs is false the underlying TCP dialer refuses connections
// to loopback, private-range, link-local, and cloud metadata addresses,
// preventing DNS rebinding SSRF attacks. Set allowPrivateURLs to true only
// when MCP servers run on a private network (mirrors MCPConfig.AllowPrivateURLs).
func NewMCPHealthChecker(servers func() []MCPServerTarget, interval time.Duration, allowPrivateURLs bool, log *slog.Logger) *MCPHealthChecker {
	transport := mcp.NewSSRFSafeTransport(allowPrivateURLs)
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	return &MCPHealthChecker{
		servers:  servers,
		interval: interval,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
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

// runOne executes a single tools/list probe against target t and stores the
// result using copy-on-write to avoid data races with concurrent readers.
func (c *MCPHealthChecker) runOne(t MCPServerTarget) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	latencyMs, toolCount, err := probeMCPServer(ctx, c.client, t)

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
		updated.ToolCount = toolCount
		updated.LastError = ""
	} else {
		updated.Status = "unhealthy"
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

// probeMCPServer sends a tools/list JSON-RPC 2.0 POST request to the target
// URL, parses the response, and returns the round-trip latency in milliseconds
// and the number of tools reported. It returns a non-nil error on any
// connection failure or non-2xx HTTP status.
func probeMCPServer(ctx context.Context, client *http.Client, t MCPServerTarget) (latencyMs int64, toolCount int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(toolsListPayload))
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	setMCPAuthHeaders(req, t)

	start := time.Now()
	resp, err := client.Do(req)
	latencyMs = time.Since(start).Milliseconds()
	if latencyMs == 0 {
		latencyMs = 1
	}
	if err != nil {
		return 0, 0, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}

	// Parse the JSON-RPC response to count the available tools. The response
	// shape is: {"jsonrpc":"2.0","id":1,"result":{"tools":[...]}}
	// A parse failure is non-fatal — we still report the server as reachable
	// because it returned a 2xx status; toolCount just stays at zero.
	var rpcResp struct {
		Result struct {
			Tools []struct{} `json:"tools"`
		} `json:"result"`
	}
	_ = jsonx.NewDecoder(resp.Body).Decode(&rpcResp)
	return latencyMs, len(rpcResp.Result.Tools), nil
}

// setMCPAuthHeaders adds the appropriate authentication header to req based on
// the target's auth type. "bearer" uses the standard Authorization header;
// "header" uses the custom header name stored in t.AuthHeader.
func setMCPAuthHeaders(req *http.Request, t MCPServerTarget) {
	if t.AuthToken == "" {
		return
	}
	switch strings.ToLower(t.AuthType) {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+t.AuthToken)
	case "header":
		if t.AuthHeader != "" {
			req.Header.Set(t.AuthHeader, t.AuthToken)
		}
	}
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
