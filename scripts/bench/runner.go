package main

import (
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// phaseResult holds the metrics from a single benchmark phase.
type phaseResult struct {
	Name    string
	Metrics *vegeta.Metrics
}

// benchResult holds all results from a complete scenario run.
type benchResult struct {
	Scenario     string
	StartedAt    time.Time
	Duration     time.Duration
	PhaseResults []phaseResult
	CodeMode     *codeModeResult // nil if not measured
}

// codeModeResult holds Code Mode benchmark numbers (in-process, not HTTP).
type codeModeResult struct {
	PureJSMs       float64 `json:"pure_js_ms"`
	WithToolCallMs float64 `json:"with_tool_call_ms"`
	WarmEvalUs     float64 `json:"warm_eval_us"`
	PoolCycleMs    float64 `json:"pool_cycle_ms"`
}

// warmup sends a few requests to each target to prime caches and connection pools.
func warmup(endpoints *endpointSet) {
	targets := []vegeta.Target{
		{Method: "POST", URL: endpoints.mockLLM + "/v1/chat/completions",
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   []byte(`{"model":"mock","messages":[{"role":"user","content":"warmup"}]}`)},
		{Method: "POST", URL: endpoints.proxy + "/v1/chat/completions",
			Header: http.Header{"Content-Type": []string{"application/json"}, "Authorization": []string{"Bearer " + endpoints.apiKey}},
			Body:   []byte(`{"model":"mock","messages":[{"role":"user","content":"warmup"}]}`)},
	}

	atk := vegeta.NewAttacker(vegeta.Timeout(10 * time.Second))
	for _, t := range targets {
		targeter := vegeta.NewStaticTargeter(t)
		for res := range atk.Attack(targeter, vegeta.Rate{Freq: 50, Per: time.Second}, 1*time.Second, "warmup") {
			_ = res
		}
	}
}

// runScenario executes all phases of a scenario and returns results.
func runScenario(s *scenario, endpoints *endpointSet) *benchResult {
	result := &benchResult{
		Scenario:  s.Name,
		StartedAt: time.Now(),
	}

	// Check if mixed scenario (parallel phases)
	isMixed := s.Name == "mixed"

	if isMixed {
		results := runPhasesParallel(s.Phases, endpoints)
		result.PhaseResults = results
	} else {
		for _, p := range s.Phases {
			fmt.Printf("  ▸ %s\n", p.Name)
			metrics := runPhase(p, endpoints)
			result.PhaseResults = append(result.PhaseResults, phaseResult{
				Name:    p.Name,
				Metrics: metrics,
			})
		}
	}

	// Run Code Mode benchmarks for scenarios that include them.
	if s.IncludeCodeMode {
		fmt.Printf("  ▸ Code Mode (go test -bench)\n")
		result.CodeMode = runCodeModeBench()
	}

	result.Duration = time.Since(result.StartedAt)
	return result
}

// runPhase executes a single load test phase.
func runPhase(p phase, ep *endpointSet) *vegeta.Metrics {
	targeter := buildTargeter(p, ep)

	// Scale Workers and Connections to the peak rate.
	// Rule of thumb: maxWorkers = peakRPS * maxLatency(200ms) with a floor of 1000.
	maxWorkers := uint64(1000)
	maxConns := 1000
	if p.MaxRate > 0 {
		scaled := uint64(p.MaxRate) * 200 / 1000 // peakRPS * 0.2s
		if scaled > maxWorkers {
			maxWorkers = scaled
		}
		if int(scaled) > maxConns {
			maxConns = int(scaled)
		}
	}

	atk := vegeta.NewAttacker(
		vegeta.Workers(10),
		vegeta.MaxWorkers(maxWorkers),
		vegeta.Connections(maxConns),
		vegeta.Timeout(30*time.Second),
		vegeta.KeepAlive(true),
	)

	var metrics vegeta.Metrics
	for res := range atk.Attack(targeter, p.Pacer, p.Duration, p.Name) {
		metrics.Add(res)
	}
	metrics.Close()
	return &metrics
}

// runPhasesParallel runs multiple phases concurrently (for mixed workload).
func runPhasesParallel(phases []phase, ep *endpointSet) []phaseResult {
	var wg sync.WaitGroup
	results := make([]phaseResult, len(phases))

	for i, p := range phases {
		wg.Add(1)
		go func(idx int, ph phase) {
			defer wg.Done()
			fmt.Printf("  ▸ %s (parallel)\n", ph.Name)
			metrics := runPhase(ph, ep)
			results[idx] = phaseResult{Name: ph.Name, Metrics: metrics}
		}(i, p)
	}
	wg.Wait()
	return results
}

// buildTargeter creates a vegeta.Targeter for the given phase.
func buildTargeter(p phase, ep *endpointSet) vegeta.Targeter {
	var url string
	var headers http.Header
	var body []byte

	switch p.Target {
	case "llm-direct":
		url = ep.mockLLM + "/v1/chat/completions"
		headers = http.Header{"Content-Type": []string{"application/json"}}
		body = makeLLMBody(p.BodySize)

	case "llm-proxy":
		url = ep.proxy + "/v1/chat/completions"
		headers = http.Header{
			"Content-Type":  []string{"application/json"},
			"Authorization": []string{"Bearer " + ep.apiKey},
		}
		body = makeLLMBody(p.BodySize)

	case "mcp-direct":
		url = ep.mockMCP + "/"
		headers = http.Header{"Content-Type": []string{"application/json"}}
		body = []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock_tool","arguments":{"input":"bench"}}}`)

	case "mcp-proxy":
		url = ep.proxy + "/api/v1/mcp/bench-org"
		headers = http.Header{
			"Content-Type":  []string{"application/json"},
			"Authorization": []string{"Bearer " + ep.apiKey},
		}
		body = []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock_tool","arguments":{"input":"bench"}}}`)

	case "codemode-proxy":
		url = ep.proxy + "/api/v1/mcp"
		headers = http.Header{
			"Content-Type":  []string{"application/json"},
			"Authorization": []string{"Bearer " + ep.apiKey},
		}
		body = []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_code","arguments":{"code":"1+1"}}}`)
	}

	target := vegeta.Target{
		Method: "POST",
		URL:    url,
		Header: headers,
		Body:   body,
	}

	return vegeta.NewStaticTargeter(target)
}

// runCodeModeBench runs the Code Mode go test benchmarks as a subprocess
// and parses the output into a codeModeResult.
func runCodeModeBench() *codeModeResult {
	cmd := exec.Command("go", "test", "./internal/mcp/...",
		"-bench=Benchmark", "-benchtime=3s", "-count=1", "-timeout=120s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("  %sCode Mode bench failed: %v%s\n", dim, err, reset)
		return nil
	}

	result := &codeModeResult{}
	re := regexp.MustCompile(`^(Benchmark\S+)\s+\d+\s+([\d.]+)\s+ns/op`)

	for _, line := range strings.Split(string(out), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		ns, _ := strconv.ParseFloat(m[2], 64)
		ms := ns / 1e6
		us := ns / 1e3

		switch {
		case strings.Contains(name, "Execute_NoTools"):
			result.PureJSMs = ms
		case strings.Contains(name, "Execute_WithToolCall"):
			result.WithToolCallMs = ms
		case strings.Contains(name, "WarmEval"):
			result.WarmEvalUs = us
		case strings.Contains(name, "AcquireRelease"):
			result.PoolCycleMs = ms
		}
	}

	return result
}

// makeLLMBody creates an OpenAI chat completion request body.
// If size > 0, pads the system prompt to reach the target size.
func makeLLMBody(size int) []byte {
	if size <= 0 {
		return []byte(`{"model":"mock","messages":[{"role":"user","content":"hello"}]}`)
	}

	// Build a large system prompt to hit target body size
	padding := strings.Repeat("x", size)
	return []byte(fmt.Sprintf(
		`{"model":"mock","messages":[{"role":"system","content":"%s"},{"role":"user","content":"hello"}]}`,
		padding,
	))
}
