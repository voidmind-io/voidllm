# VoidLLM Benchmark

Measures proxy overhead for LLM and MCP paths using embedded mock servers and the [Vegeta](https://github.com/tsenart/vegeta) load testing library.

## Usage

```bash
go run ./scripts/bench [scenario] [flags]
```

## Scenarios

| Scenario | RPS | Duration | What it measures |
|---|---|---|---|
| `quick` (default) | 500 | 15s | Sanity check — all paths |
| `sustained` | 5000 | 5 min | Memory leaks, GC pressure, connection exhaustion |
| `burst` | 200→10k→200 | 90s | Spike handling and recovery |
| `large-payload` | 100 | 60s | 100KB request bodies — allocation overhead |
| `mixed` | 500 total | 60s | 60% LLM + 30% MCP + 10% Code Mode (parallel) |
| `endurance` | 500 | 30 min | Long-running stability, goroutine leaks |
| `all` | varies | varies | Run all scenarios sequentially |

## Flags

```
--rps N          Override default RPS
--duration D     Override duration (e.g. 30s, 5m)
--json           JSON report output (pipe to file for CI)
```

## Examples

```bash
# Quick sanity check
go run ./scripts/bench quick

# Sustained heavy load
go run ./scripts/bench sustained --rps 2000 --duration 120s

# JSON output for CI
go run ./scripts/bench quick --json > bench-results.json

# All scenarios
go run ./scripts/bench all
```

## How it works

1. Starts embedded mock servers (LLM + MCP) on random ports
2. Builds and starts a VoidLLM proxy instance with in-memory SQLite
3. Runs Vegeta load tests against direct (calibration) and proxied paths
4. Reports overhead = proxied latency - direct latency

## Interpreting results

- **LLM Proxy overhead** target: <2ms (currently ~150-400µs)
- **MCP Proxy overhead** target: <2ms (currently ~400-800µs)
- **Success rate** should be 100% — any failures indicate a bug
- **P99** matters more than mean for production readiness

Higher values indicate lock contention, GC pressure, or the async usage logger
falling behind its channel buffer.
