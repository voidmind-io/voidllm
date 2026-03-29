# VoidLLM Proxy Overhead Benchmark

## What it measures

End-to-end proxy overhead — the extra latency VoidLLM adds on top of a direct
call to the upstream LLM or MCP server. Auth, routing, usage logging, and header
rewriting are all exercised by the hot path. Streaming is not tested here.

## How to run

```
./scripts/bench/run.sh [rps] [duration]
```

Defaults: 500 req/s for 15 s. Ports are configurable via environment variables:

```
MOCK_PORT=9999 MCP_MOCK_PORT=9998 PROXY_PORT=8081 ./scripts/bench/run.sh 200 30s
```

Prerequisites: `go` in PATH. `vegeta` is installed automatically via `go install`
if not already present.

## How to interpret results

The script runs four phases and prints a vegeta text report for each:

1. **Calibration** — direct to mock LLM (baseline, ~10 ms mock latency)
2. **LLM Overhead** — through the VoidLLM proxy
3. **MCP Calibration** — direct to mock MCP server
4. **MCP Overhead** — through the VoidLLM MCP proxy

The final "Overhead (computed)" section subtracts calibration mean from proxy
mean to isolate the cost of the proxy itself.

## What "good" looks like

- Mean overhead **< 2 ms** on a modern laptop at 500 req/s.
- p99 overhead **< 5 ms**.
- Success rate **100 %** for both LLM and MCP phases.

Higher values indicate lock contention in the in-memory cache, GC pressure, or
a slow SQL write in the async usage logger falling behind its channel buffer.
