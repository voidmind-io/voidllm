package main

import (
	"fmt"
	"strings"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// phase defines a single load-testing phase within a scenario.
type phase struct {
	Name     string
	Target   string // "llm-direct", "llm-proxy", "mcp-direct", "mcp-proxy", "codemode-proxy"
	Pacer    vegeta.Pacer
	Duration time.Duration
	BodySize int  // 0 = default (~64 bytes), >0 = large payload
	MaxRate  int  // peak RPS hint for sizing Workers/Connections (0 = use defaults)
	Stream   bool // when true, request body includes "stream":true for SSE responses
}

// scenario defines a complete benchmark run with one or more phases.
type scenario struct {
	Name            string
	Description     string
	Phases          []phase
	IncludeCodeMode bool // run go test -bench for WASM sandbox
}

// burstPacer implements vegeta.Pacer with a step function:
// low RPS → ramp to high → sustain high → ramp down → low RPS recovery.
type burstPacer struct {
	lowRate  vegeta.Rate
	highRate vegeta.Rate
	// Phase timing (cumulative from start):
	rampUpAt   time.Duration // when to start ramping up
	peakAt     time.Duration // when peak begins
	rampDownAt time.Duration // when to start ramping down
	recoveryAt time.Duration // when low rate resumes
}

func (p *burstPacer) currentRate(elapsed time.Duration) vegeta.Rate {
	switch {
	case elapsed < p.rampUpAt:
		return p.lowRate
	case elapsed < p.peakAt:
		progress := float64(elapsed-p.rampUpAt) / float64(p.peakAt-p.rampUpAt)
		freq := float64(p.lowRate.Freq) + progress*(float64(p.highRate.Freq)-float64(p.lowRate.Freq))
		return vegeta.Rate{Freq: max(1, int(freq)), Per: time.Second}
	case elapsed < p.rampDownAt:
		return p.highRate
	case elapsed < p.recoveryAt:
		progress := float64(elapsed-p.rampDownAt) / float64(p.recoveryAt-p.rampDownAt)
		freq := float64(p.highRate.Freq) - progress*(float64(p.highRate.Freq)-float64(p.lowRate.Freq))
		return vegeta.Rate{Freq: max(1, int(freq)), Per: time.Second}
	default:
		return p.lowRate
	}
}

func (p *burstPacer) Pace(elapsed time.Duration, hits uint64) (time.Duration, bool) {
	rate := p.currentRate(elapsed)
	return vegeta.ConstantPacer{Freq: rate.Freq, Per: rate.Per}.Pace(elapsed, hits)
}

func (p *burstPacer) Rate(elapsed time.Duration) float64 {
	rate := p.currentRate(elapsed)
	return vegeta.ConstantPacer{Freq: rate.Freq, Per: rate.Per}.Rate(elapsed)
}

func getScenario(name string, rpsOverride int, durationOverride time.Duration) *scenario {
	switch strings.ToLower(name) {
	case "quick":
		return scenarioQuick(rpsOverride, durationOverride)
	case "sustained":
		return scenarioSustained(rpsOverride, durationOverride)
	case "burst":
		return scenarioBurst(rpsOverride)
	case "large-payload":
		return scenarioLargePayload(rpsOverride, durationOverride)
	case "mixed":
		return scenarioMixed(rpsOverride, durationOverride)
	case "endurance":
		return scenarioEndurance(rpsOverride, durationOverride)
	case "realistic":
		return scenarioRealistic(rpsOverride, durationOverride)
	default:
		return nil
	}
}

func allScenarioNames() []string {
	return []string{"quick", "sustained", "burst", "large-payload", "mixed", "endurance", "realistic"}
}

func rps(rate, override int) int {
	if override > 0 {
		return override
	}
	return rate
}

func dur(d, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return d
}

func constantPacer(rate int) vegeta.Pacer {
	return vegeta.ConstantPacer{Freq: rate, Per: time.Second}
}

// ─── Scenarios ───────────────────────────────────────────────────

func scenarioQuick(rpsOvr int, durOvr time.Duration) *scenario {
	r := rps(500, rpsOvr)
	d := dur(15*time.Second, durOvr)
	return &scenario{
		Name:            "quick",
		Description:     "Quick sanity check — all paths, moderate load",
		IncludeCodeMode: true,
		Phases: []phase{
			{Name: "LLM Calibration", Target: "llm-direct", Pacer: constantPacer(r), Duration: d},
			{Name: "LLM Proxy", Target: "llm-proxy", Pacer: constantPacer(r), Duration: d},
			{Name: "MCP Calibration", Target: "mcp-direct", Pacer: constantPacer(r), Duration: d},
			{Name: "MCP Proxy", Target: "mcp-proxy", Pacer: constantPacer(r), Duration: d},
		},
	}
}

func scenarioSustained(rpsOvr int, durOvr time.Duration) *scenario {
	r := rps(5000, rpsOvr)
	d := dur(5*time.Minute, durOvr)
	return &scenario{
		Name:        "sustained",
		Description: "Sustained high load — memory leaks, GC pressure, connection exhaustion",
		Phases: []phase{
			{Name: "LLM Proxy", Target: "llm-proxy", Pacer: constantPacer(r), Duration: d},
			{Name: "MCP Proxy", Target: "mcp-proxy", Pacer: constantPacer(r), Duration: d},
		},
	}
}

func scenarioBurst(rpsOvr int) *scenario {
	peak := rps(5000, rpsOvr)
	base := peak / 25
	if base < 50 {
		base = 50
	}
	totalDuration := 90 * time.Second
	return &scenario{
		Name:        "burst",
		Description: fmt.Sprintf("Burst load — %d RPS → %d RPS spike → recovery", base, peak),
		Phases: []phase{
			{
				Name:   "LLM Burst",
				Target: "llm-proxy",
				Pacer: &burstPacer{
					lowRate:    vegeta.Rate{Freq: base, Per: time.Second},
					highRate:   vegeta.Rate{Freq: peak, Per: time.Second},
					rampUpAt:   30 * time.Second,
					peakAt:     40 * time.Second,
					rampDownAt: 50 * time.Second,
					recoveryAt: 60 * time.Second,
				},
				Duration: totalDuration,
				MaxRate:  peak,
			},
		},
	}
}

func scenarioLargePayload(rpsOvr int, durOvr time.Duration) *scenario {
	r := rps(100, rpsOvr)
	d := dur(60*time.Second, durOvr)
	return &scenario{
		Name:        "large-payload",
		Description: "Large request bodies (100KB) — memory allocation, GC overhead",
		Phases: []phase{
			{Name: "Large Calibration", Target: "llm-direct", Pacer: constantPacer(r), Duration: d, BodySize: 100 * 1024},
			{Name: "Large Proxy", Target: "llm-proxy", Pacer: constantPacer(r), Duration: d, BodySize: 100 * 1024},
		},
	}
}

func scenarioMixed(rpsOvr int, durOvr time.Duration) *scenario {
	base := rps(500, rpsOvr)
	d := dur(60*time.Second, durOvr)
	llmRate := int(float64(base) * 0.6) // 60%
	mcpRate := int(float64(base) * 0.3) // 30%
	cmRate := int(float64(base) * 0.1)  // 10%
	if cmRate < 1 {
		cmRate = 1
	}
	return &scenario{
		Name:        "mixed",
		Description: "Mixed workload — 60% LLM + 30% MCP + 10% Code Mode (parallel)",
		Phases: []phase{
			{Name: "LLM (60%)", Target: "llm-proxy", Pacer: constantPacer(llmRate), Duration: d},
			{Name: "MCP (30%)", Target: "mcp-proxy", Pacer: constantPacer(mcpRate), Duration: d},
			{Name: "Code Mode (10%)", Target: "codemode-proxy", Pacer: constantPacer(cmRate), Duration: d},
		},
	}
}

func scenarioEndurance(rpsOvr int, durOvr time.Duration) *scenario {
	r := rps(500, rpsOvr)
	d := dur(30*time.Minute, durOvr)
	return &scenario{
		Name:        "endurance",
		Description: "Long-running stability test — goroutine leaks, memory growth",
		Phases: []phase{
			{Name: "LLM Endurance", Target: "llm-proxy", Pacer: constantPacer(r), Duration: d},
		},
	}
}

func scenarioRealistic(rpsOvr int, durOvr time.Duration) *scenario {
	r := rps(50, rpsOvr)
	d := dur(2*time.Minute, durOvr)
	return &scenario{
		Name:        "realistic",
		Description: "Realistic streaming load - SSE responses with 30-50ms inter-token delay",
		Phases: []phase{
			{Name: "LLM Calibration (stream)", Target: "llm-direct", Pacer: constantPacer(r), Duration: d, Stream: true},
			{Name: "LLM Proxy (stream)", Target: "llm-proxy", Pacer: constantPacer(r), Duration: d, Stream: true},
		},
	}
}
