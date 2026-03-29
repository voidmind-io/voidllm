package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	cyan   = "\033[36m"
	green  = "\033[32m"
	yellow = "\033[33m"
	reset  = "\033[0m"
)

// printTextReport prints a human-readable benchmark report.
func printTextReport(result *benchResult) {
	fmt.Printf("\n%s%s━━━ VoidLLM Benchmark: %s ━━━%s\n\n", yellow, bold, result.Scenario, reset)

	// Phase details
	for _, pr := range result.PhaseResults {
		m := pr.Metrics
		fmt.Printf("%s%s▸ %s%s\n", cyan, bold, pr.Name, reset)
		printMetricsLine(m)
		fmt.Println()
	}

	// Overhead calculation (if we have calibration + proxy pairs)
	calibLLM := findPhase(result, "LLM Calibration", "Large Calibration")
	proxyLLM := findPhase(result, "LLM Proxy", "Large Proxy", "LLM Burst", "LLM Endurance", "LLM (60%)")
	calibMCP := findPhase(result, "MCP Calibration")
	proxyMCP := findPhase(result, "MCP Proxy", "MCP (30%)")

	if calibLLM != nil && proxyLLM != nil {
		overhead := proxyLLM.Latencies.P50 - calibLLM.Latencies.P50
		fmt.Printf("%s%sOverhead:%s\n", yellow, bold, reset)
		fmt.Printf("  LLM Proxy:  %s (P50)\n", formatDuration(overhead))
	}
	if calibMCP != nil && proxyMCP != nil {
		overhead := proxyMCP.Latencies.P50 - calibMCP.Latencies.P50
		fmt.Printf("  MCP Proxy:  %s (P50)\n", formatDuration(overhead))
	}

	// Code Mode results
	if result.CodeMode != nil {
		cm := result.CodeMode
		fmt.Printf("\n%s%sCode Mode:%s\n", cyan, bold, reset)
		fmt.Printf("  Pure JS:        %8.2f ms\n", cm.PureJSMs)
		fmt.Printf("  With Tool Call: %8.2f ms\n", cm.WithToolCallMs)
		fmt.Printf("  Warm Eval:      %8.0f µs\n", cm.WarmEvalUs)
		fmt.Printf("  Pool Cycle:     %8.2f ms\n", cm.PoolCycleMs)
	}

	fmt.Printf("\n%sDuration: %s%s\n", dim, result.Duration.Round(time.Second), reset)
}

// printMetricsLine prints a compact one-line summary of vegeta metrics.
func printMetricsLine(m *vegeta.Metrics) {
	fmt.Printf("  P50: %-10s  P95: %-10s  P99: %-10s  Success: %.1f%%  Rate: %.0f/s  Requests: %d\n",
		formatDuration(m.Latencies.P50),
		formatDuration(m.Latencies.P95),
		formatDuration(m.Latencies.P99),
		m.Success*100,
		m.Rate,
		m.Requests,
	)
}

// findPhase finds the first matching phase result by name.
func findPhase(result *benchResult, names ...string) *vegeta.Metrics {
	for _, pr := range result.PhaseResults {
		for _, name := range names {
			if pr.Name == name {
				return pr.Metrics
			}
		}
	}
	return nil
}

// formatDuration formats a duration for display (µs or ms).
func formatDuration(d time.Duration) string {
	us := d.Microseconds()
	if us < 1000 {
		return fmt.Sprintf("%dµs", us)
	}
	return fmt.Sprintf("%.2fms", float64(us)/1000)
}

// ─── JSON Report ─────────────────────────────────────────────────

type jsonReport struct {
	Scenario  string                 `json:"scenario"`
	Timestamp string                 `json:"timestamp"`
	Duration  string                 `json:"duration"`
	Results   map[string]jsonMetrics `json:"results"`
	CodeMode  *codeModeResult        `json:"code_mode,omitempty"`
}

type jsonMetrics struct {
	MeanUs   float64 `json:"mean_us"`
	P50Us    float64 `json:"p50_us"`
	P95Us    float64 `json:"p95_us"`
	P99Us    float64 `json:"p99_us"`
	MaxUs    float64 `json:"max_us"`
	Success  float64 `json:"success"`
	Rate     float64 `json:"rate"`
	Requests uint64  `json:"requests"`
}

func printJSONReport(result *benchResult) {
	report := jsonReport{
		Scenario:  result.Scenario,
		Timestamp: result.StartedAt.UTC().Format(time.RFC3339),
		Duration:  result.Duration.Round(time.Second).String(),
		Results:   make(map[string]jsonMetrics),
		CodeMode:  result.CodeMode,
	}

	for _, pr := range result.PhaseResults {
		m := pr.Metrics
		report.Results[pr.Name] = jsonMetrics{
			MeanUs:   float64(m.Latencies.Mean.Microseconds()),
			P50Us:    float64(m.Latencies.P50.Microseconds()),
			P95Us:    float64(m.Latencies.P95.Microseconds()),
			P99Us:    float64(m.Latencies.P99.Microseconds()),
			MaxUs:    float64(m.Latencies.Max.Microseconds()),
			Success:  m.Success,
			Rate:     m.Rate,
			Requests: m.Requests,
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(report)
}
