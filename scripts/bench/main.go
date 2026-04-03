// VoidLLM Benchmark CLI
//
// Measures proxy overhead for LLM and MCP paths using embedded mock servers
// and the Vegeta load testing library.
//
// Usage:
//
//	go run ./scripts/bench [scenario] [flags]
//
// Scenarios:
//
//	quick          500 RPS, 15s — sanity check (default)
//	sustained      5000 RPS, 5 min — memory leaks, GC pressure
//	burst          200→10k→200 RPS — spike and recovery
//	large-payload  100KB bodies, 100 RPS — allocation overhead
//	mixed          60% LLM + 30% MCP + 10% Code Mode (parallel)
//	endurance      500 RPS, 30 min — long-running stability
//	realistic      50 RPS, 2 min — SSE streaming with 30-50ms inter-token delay
//	all            Run all scenarios sequentially
//
// Flags:
//
//	--rps N           Override default RPS for the scenario
//	--duration D      Override default duration (e.g. 30s, 5m)
//	--json            Output JSON report instead of text
//	--metrics-out F   Write metrics samples JSON to file F
package main

import (
	"bufio"
	"bytes"
	"context"
	stdjson "encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// endpointSet holds URLs and credentials for all benchmark targets.
type endpointSet struct {
	mockLLM string
	mockMCP string
	proxy   string
	apiKey  string
}

func main() {
	scenarioName := "quick"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		scenarioName = os.Args[1]
	}

	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	rpsOverride := fs.Int("rps", 0, "override default RPS")
	durStr := fs.String("duration", "", "override duration (e.g. 30s, 5m)")
	jsonOutput := fs.Bool("json", false, "JSON report output")
	metricsOut := fs.String("metrics-out", "", "path to write metrics JSON (default: no metrics)")

	// Parse flags after scenario name
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	fs.Parse(args)

	var durationOverride time.Duration
	if *durStr != "" {
		d, err := time.ParseDuration(*durStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid duration: %s\n", *durStr)
			os.Exit(1)
		}
		durationOverride = d
	}

	if !*jsonOutput {
		printBanner(scenarioName)
	}

	// Resolve the single-scenario case early so mock server selection can
	// depend on the scenario name. (The "all" path always uses the default mock.)
	var earlySingle *scenario
	if scenarioName != "all" {
		earlySingle = getScenario(scenarioName, *rpsOverride, durationOverride)
		if earlySingle == nil {
			fmt.Fprintf(os.Stderr, "unknown scenario: %s\nAvailable: %s\n", scenarioName, strings.Join(allScenarioNames(), ", "))
			os.Exit(1)
		}
	}

	// ─── Start mock servers ──────────────────────────────────────
	if !*jsonOutput {
		fmt.Printf("%sStarting mock servers...%s\n", dim, reset)
	}

	mockLLM, err := startMockLLMStreaming(10 * time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error starting mock LLM: %v\n", err)
		os.Exit(1)
	}
	defer mockLLM.Close()

	mockMCP, err := startMockMCP(10 * time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error starting mock MCP: %v\n", err)
		os.Exit(1)
	}
	defer mockMCP.Close()

	// ─── Build and start VoidLLM ─────────────────────────────────
	if !*jsonOutput {
		fmt.Printf("%sBuilding VoidLLM...%s\n", dim, reset)
	}

	proxyBin, err := buildProxy()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building VoidLLM: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(proxyBin)

	if !*jsonOutput {
		fmt.Printf("%sStarting VoidLLM proxy...%s\n", dim, reset)
	}

	proxyAddr, apiKey, proxyCmd, err := startProxy(proxyBin, mockLLM.URL(), mockMCP.URL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error starting proxy: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		proxyCmd.Process.Signal(os.Interrupt)
		proxyCmd.Wait()
	}()

	endpoints := &endpointSet{
		mockLLM: mockLLM.URL(),
		mockMCP: mockMCP.URL(),
		proxy:   proxyAddr,
		apiKey:  apiKey,
	}

	// Grant MCP access: register the bench MCP server as org-scoped via API
	// (YAML servers are global and require explicit org_mcp_access grants).
	if err := registerOrgMCPServer(endpoints, mockMCP.URL()); err != nil {
		if !*jsonOutput {
			fmt.Fprintf(os.Stderr, "%sWARN: could not register org MCP server: %v%s\n", dim, err, reset)
		}
	}

	if !*jsonOutput {
		fmt.Printf("%s%s✓ All servers running%s\n", green, bold, reset)
		fmt.Printf("%sWarming up...%s\n\n", dim, reset)
	}

	warmup(endpoints)

	// ─── Run scenario(s) ─────────────────────────────────────────
	if scenarioName == "all" {
		var allResults []*benchResult
		for _, name := range allScenarioNames() {
			s := getScenario(name, *rpsOverride, durationOverride)
			if !*jsonOutput {
				fmt.Printf("%s%s━━━ %s: %s ━━━%s\n\n", cyan, bold, s.Name, s.Description, reset)
			}

			var mc *sampler
			if *metricsOut != "" {
				mc = startSampler(proxyAddr+"/metrics", 1*time.Second)
			}

			result := runScenario(s, endpoints)
			allResults = append(allResults, result)

			if mc != nil {
				samples := mc.Stop()
				writeMetricsJSON(*metricsOut, name, samples)
			}

			if !*jsonOutput {
				printTextReport(result)
				fmt.Println()
			}
		}
		if *jsonOutput {
			for _, r := range allResults {
				printJSONReport(r)
			}
		}
		return
	}

	s := earlySingle

	if !*jsonOutput {
		fmt.Printf("%s%s%s\n\n", dim, s.Description, reset)
	}

	var metricsCollector *sampler
	if *metricsOut != "" {
		metricsCollector = startSampler(proxyAddr+"/metrics", 1*time.Second)
	}

	result := runScenario(s, endpoints)

	if metricsCollector != nil {
		samples := metricsCollector.Stop()
		writeMetricsJSON(*metricsOut, s.Name, samples)
	}

	if *jsonOutput {
		printJSONReport(result)
	} else {
		printTextReport(result)
	}
}

// writeMetricsJSON marshals the collected metric samples to a JSON file at path.
// The output includes the scenario name alongside the sample array for context.
func writeMetricsJSON(dir, scenario string, samples []metricSample) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating metrics dir %s: %v\n", dir, err)
		return
	}
	path := filepath.Join(dir, scenario+".json")
	out := struct {
		Scenario string         `json:"scenario"`
		Samples  []metricSample `json:"samples"`
	}{Scenario: scenario, Samples: samples}
	data, err := stdjson.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling metrics: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing metrics to %s: %v\n", path, err)
		return
	}
	fmt.Printf("Metrics written to %s\n", path)
}

// ─── VoidLLM Proxy Management ────────────────────────────────────

func buildProxy() (string, error) {
	f, err := os.CreateTemp("", "bench-voidllm-proxy-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	out := f.Name()
	f.Close()
	cmd := exec.Command("go", "build", "-o", out, "./cmd/voidllm")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	return out, nil
}

func startProxy(bin, llmURL, mcpURL string) (addr, apiKey string, cmd *exec.Cmd, err error) {
	proxyPort := "8081"
	addr = "http://127.0.0.1:" + proxyPort

	configContent := fmt.Sprintf(`server:
  proxy:
    port: %s
database:
  driver: sqlite
  dsn: file::memory:?cache=shared
models:
  - name: mock
    provider: custom
    base_url: %s/v1
    aliases: [default]
mcp_servers:
  - name: bench-mcp
    alias: bench
    url: %s
    auth_type: none
settings:
  admin_key: bench-admin-key-12345678901234567890
  encryption_key: bench-encryption-key-1234567890
  mcp:
    allow_private_urls: true
    code_mode:
      enabled: true
  health_check:
    health:
      enabled: false
    functional:
      enabled: false
  audit:
    enabled: false
`, proxyPort, llmURL, mcpURL)

	configFile, err := os.CreateTemp("", "bench-proxy-*.yaml")
	if err != nil {
		return "", "", nil, fmt.Errorf("create config: %w", err)
	}
	configPath := configFile.Name()
	if _, err := configFile.WriteString(configContent); err != nil {
		configFile.Close()
		return "", "", nil, fmt.Errorf("write config: %w", err)
	}
	configFile.Close()
	defer os.Remove(configPath)

	cmd = exec.Command(bin, "--config", configPath)
	cmd.Env = append(os.Environ(),
		"VOIDLLM_ADMIN_KEY=bench-admin-key-12345678901234567890",
		"VOIDLLM_ENCRYPTION_KEY=bench-encryption-key-1234567890",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return "", "", nil, fmt.Errorf("start: %w", err)
	}

	// Read output to find API key via channel (no data race).
	keyCh := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scanner := bufio.NewScanner(stdout)
	go func() {
		defer close(keyCh)
		found := false
		for scanner.Scan() {
			line := scanner.Text()
			if !found && strings.Contains(line, "vl_uk_") {
				for _, p := range strings.Fields(line) {
					if strings.HasPrefix(p, "vl_uk_") {
						keyCh <- p
						found = true
						break
					}
				}
			}
			// Keep draining stdout to prevent proxy from blocking on pipe writes.
		}
	}()

	select {
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return "", "", nil, fmt.Errorf("proxy startup timeout")
	case key, ok := <-keyCh:
		if !ok {
			cmd.Process.Kill()
			cmd.Wait()
			return "", "", nil, fmt.Errorf("proxy exited without emitting API key")
		}
		// Wait for the server to be fully ready after key is printed.
		time.Sleep(2 * time.Second)
		return addr, key, cmd, nil
	}
}

// registerOrgMCPServer creates the bench MCP server as org-scoped via the
// Admin API. Org-scoped servers don't need explicit org_mcp_access grants
// (visibility = access), avoiding the closed-by-default restriction on global servers.
func registerOrgMCPServer(ep *endpointSet, mcpURL string) error {
	// 1. Get bootstrap org ID
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest("GET", ep.proxy+"/api/v1/orgs", nil)
	if err != nil {
		return fmt.Errorf("build orgs request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ep.apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("list orgs: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("list orgs: status %d: %s", resp.StatusCode, body)
	}

	var orgsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := stdjson.Unmarshal(body, &orgsResp); err != nil {
		return fmt.Errorf("parse orgs response: %w", err)
	}
	if len(orgsResp.Data) == 0 {
		return fmt.Errorf("no organizations found")
	}
	orgID := orgsResp.Data[0].ID

	// 2. Create org-scoped MCP server
	createBody, _ := stdjson.Marshal(map[string]string{
		"name":      "bench-mcp",
		"alias":     "bench-org",
		"url":       mcpURL,
		"auth_type": "none",
	})
	req, err = http.NewRequest("POST", ep.proxy+"/api/v1/orgs/"+orgID+"/mcp-servers", bytes.NewReader(createBody))
	if err != nil {
		return fmt.Errorf("build create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ep.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("create mcp server: %w", err)
	}
	defer resp.Body.Close()

	// 409 = alias already exists (idempotent re-run), treat as success.
	if resp.StatusCode != 201 && resp.StatusCode != 409 {
		body, _ = io.ReadAll(resp.Body)
		return fmt.Errorf("create mcp server: status %d: %s", resp.StatusCode, body)
	}

	return nil
}

func printBanner(scenario string) {
	line := fmt.Sprintf("VoidLLM Benchmark — %s", scenario)
	width := len(line) + 4
	border := strings.Repeat("═", width)
	fmt.Printf("\n%s%s╔%s╗%s\n", bold, yellow, border, reset)
	fmt.Printf("%s%s║  %s  ║%s\n", bold, yellow, line, reset)
	fmt.Printf("%s%s╚%s╝%s\n\n", bold, yellow, border, reset)
}
