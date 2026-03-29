#!/bin/bash
# VoidLLM Proxy Overhead Benchmark
# Measures the latency added by the proxy using Vegeta load tester.
#
# Method:
#   1. Calibration: Vegeta → Mock LLM directly (baseline)
#   2. Overhead: Vegeta → VoidLLM → Mock LLM (proxy path)
#   3. MCP Calibration: Vegeta → Mock MCP directly (baseline)
#   4. MCP Overhead: Vegeta → VoidLLM → Mock MCP (proxy path)
#   5. Compare latencies to determine proxy overhead
#
# Prerequisites: go, vegeta (auto-installed if missing)
# Usage: ./scripts/bench/run.sh [rps] [duration]
#
# Defaults:
#   rps      = 500
#   duration = 15s
#   MOCK_PORT     = 9999
#   MCP_MOCK_PORT = 9998
#   PROXY_PORT    = 8081

set -euo pipefail

export PATH=$PATH:~/go/bin

RPS=${1:-500}
DURATION=${2:-15s}
MOCK_PORT=${MOCK_PORT:-9999}
MCP_MOCK_PORT=${MCP_MOCK_PORT:-9998}
PROXY_PORT=${PROXY_PORT:-8081}

MOCK_PID=""
MCP_MOCK_PID=""
PROXY_PID=""

BOLD='\033[1m'
DIM='\033[2m'
CYAN='\033[36m'
GREEN='\033[32m'
YELLOW='\033[33m'
RESET='\033[0m'

# Ensure vegeta is installed
if ! command -v vegeta &> /dev/null; then
  echo "Installing vegeta..."
  go install github.com/tsenart/vegeta@latest
fi

# Banner with dynamic padding so values don't corrupt the box.
banner_line="  Rate: ${RPS} req/s  Duration: ${DURATION}"
banner_width=46
pad=$(( banner_width - ${#banner_line} - 1 ))
printf "${BOLD}╔%s╗${RESET}\n" "$(printf '═%.0s' $(seq 1 $banner_width))"
printf "${BOLD}║  %-*s║${RESET}\n" "$banner_width" "VoidLLM Proxy Overhead Benchmark"
printf "${BOLD}║%s%*s║${RESET}\n" "$banner_line" "$pad" ""
printf "${BOLD}╚%s╝${RESET}\n" "$(printf '═%.0s' $(seq 1 $banner_width))"
echo ""

# ─── Cleanup ──────────────────────────────────────────────────────
cleanup() {
  [ -n "$MOCK_PID" ]     && kill "$MOCK_PID"     2>/dev/null || true
  [ -n "$MCP_MOCK_PID" ] && kill "$MCP_MOCK_PID" 2>/dev/null || true
  [ -n "$PROXY_PID" ]    && kill "$PROXY_PID"    2>/dev/null || true
  rm -f /tmp/bench-target-*.txt /tmp/bench-proxy.yaml /tmp/bench-body*.json \
        /tmp/bench-mock-llm /tmp/bench-mock-mcp /tmp/bench-voidllm-proxy \
        /tmp/bench-voidllm.log /tmp/bench-calibration*.bin /tmp/bench-overhead*.bin
}
trap cleanup EXIT

# Kill any leftovers from previous runs.
pkill -f "bench-mock-llm|bench-mock-mcp|bench-voidllm-proxy" 2>/dev/null || true
sleep 1

# ─── Pre-build binaries ──────────────────────────────────────────
echo -e "${DIM}Building binaries...${RESET}"
go build -o /tmp/bench-mock-llm scripts/bench/mock-llm.go 2>&1   || { echo "ERROR: mock-llm build failed"; exit 1; }
go build -o /tmp/bench-mock-mcp scripts/bench/mock-mcp.go 2>&1   || { echo "ERROR: mock-mcp build failed"; exit 1; }
go build -o /tmp/bench-voidllm-proxy ./cmd/voidllm 2>&1           || { echo "ERROR: voidllm build failed"; exit 1; }
echo -e "${DIM}Built.${RESET}"

# ─── Start Mock Servers ───────────────────────────────────────────
echo -e "${DIM}Starting mock LLM on :${MOCK_PORT} (10ms latency)...${RESET}"
/tmp/bench-mock-llm -port "$MOCK_PORT" -latency 10ms > /dev/null 2>&1 &
MOCK_PID=$!

echo -e "${DIM}Starting mock MCP on :${MCP_MOCK_PORT} (10ms latency)...${RESET}"
/tmp/bench-mock-mcp -port "$MCP_MOCK_PORT" -latency 10ms > /dev/null 2>&1 &
MCP_MOCK_PID=$!

# Retry loop for mock LLM health (up to 10 × 0.5s = 5s).
mock_ready=0
for i in $(seq 1 10); do
  if curl -sf "http://localhost:${MOCK_PORT}/v1/models" > /dev/null 2>&1; then
    mock_ready=1
    break
  fi
  sleep 0.5
done
if [ "$mock_ready" -eq 0 ]; then
  echo "ERROR: Mock LLM failed to start"
  exit 1
fi

# Retry loop for mock MCP health (up to 10 × 0.5s = 5s).
mcp_ready=0
for i in $(seq 1 10); do
  if curl -sf -X POST "http://localhost:${MCP_MOCK_PORT}/" \
       -H "Content-Type: application/json" \
       -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}' > /dev/null 2>&1; then
    mcp_ready=1
    break
  fi
  sleep 0.5
done
if [ "$mcp_ready" -eq 0 ]; then
  echo "ERROR: Mock MCP failed to start"
  exit 1
fi

# ─── Create bench config ─────────────────────────────────────────
cat > /tmp/bench-proxy.yaml << EOF
server:
  proxy:
    port: $PROXY_PORT

database:
  driver: sqlite
  dsn: file::memory:?cache=shared

models:
  - name: mock
    provider: custom
    base_url: http://localhost:$MOCK_PORT/v1
    aliases: [default]

mcp_servers:
  - name: bench-mcp
    alias: bench
    url: http://localhost:$MCP_MOCK_PORT
    auth_type: none

settings:
  admin_key: bench-admin-key-12345678901234567890
  encryption_key: bench-encryption-key-1234567890
  mcp:
    allow_private_urls: true
  health_check:
    health:
      enabled: false
    functional:
      enabled: false
  audit:
    enabled: false
EOF

# ─── Start VoidLLM ───────────────────────────────────────────────
echo -e "${DIM}Starting VoidLLM on :${PROXY_PORT}...${RESET}"
VOIDLLM_ADMIN_KEY=bench-admin-key-12345678901234567890 \
VOIDLLM_ENCRYPTION_KEY=bench-encryption-key-1234567890 \
/tmp/bench-voidllm-proxy --config /tmp/bench-proxy.yaml > /tmp/bench-voidllm.log 2>&1 &
PROXY_PID=$!

# Wait for proxy to be ready (up to 15 × 1s).
for i in $(seq 1 15); do
  if curl -sf "http://localhost:${PROXY_PORT}/healthz" > /dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! curl -sf "http://localhost:${PROXY_PORT}/healthz" > /dev/null 2>&1; then
  echo "ERROR: VoidLLM failed to start"
  tail -20 /tmp/bench-voidllm.log
  exit 1
fi

# Get API key from bootstrap log.
API_KEY=$(grep -oP 'vl_uk_[a-f0-9]+' /tmp/bench-voidllm.log | head -1)
if [ -z "$API_KEY" ]; then
  echo "ERROR: Could not find API key in VoidLLM logs"
  tail -20 /tmp/bench-voidllm.log
  exit 1
fi

echo -e "${GREEN}Both servers running. API Key: ${API_KEY:0:20}...${RESET}"
echo ""

# ─── Vegeta target files ─────────────────────────────────────────
echo '{"model":"mock","messages":[{"role":"user","content":"hello"}]}' > /tmp/bench-body.json

cat > /tmp/bench-target-direct.txt << EOF
POST http://localhost:$MOCK_PORT/v1/chat/completions
Content-Type: application/json
@/tmp/bench-body.json
EOF

cat > /tmp/bench-target-proxy.txt << EOF
POST http://localhost:$PROXY_PORT/v1/chat/completions
Content-Type: application/json
Authorization: Bearer $API_KEY
@/tmp/bench-body.json
EOF

# Quick sanity check
echo -e "${DIM}Sanity check...${RESET}"
SANITY=$(curl -s "http://localhost:${PROXY_PORT}/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock","messages":[{"role":"user","content":"hello"}]}')

if echo "$SANITY" | grep -q "error"; then
  echo "ERROR: Proxy sanity check failed: $SANITY"
  exit 1
fi
echo -e "${GREEN}Sanity check passed.${RESET}"
echo ""

# ─── Phase 1: Calibration ────────────────────────────────────────
echo -e "${CYAN}${BOLD}Phase 1: Calibration (direct → Mock LLM)${RESET}"
vegeta attack -targets=/tmp/bench-target-direct.txt -rate="$RPS" -duration="$DURATION" \
  > /tmp/bench-calibration.bin
vegeta report -type=text < /tmp/bench-calibration.bin
echo ""

# ─── Phase 2: Through proxy ──────────────────────────────────────
echo -e "${CYAN}${BOLD}Phase 2: Overhead (Vegeta → VoidLLM → Mock LLM)${RESET}"
vegeta attack -targets=/tmp/bench-target-proxy.txt -rate="$RPS" -duration="$DURATION" \
  > /tmp/bench-overhead.bin
vegeta report -type=text < /tmp/bench-overhead.bin
echo ""

# ─── Phase 3: MCP Calibration ────────────────────────────────────
echo -e "${CYAN}${BOLD}Phase 3: MCP Calibration (direct → Mock MCP)${RESET}"

echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock_tool","arguments":{"input":"bench"}}}' > /tmp/bench-mcp-body.json

cat > /tmp/bench-target-mcp-direct.txt << EOF
POST http://localhost:$MCP_MOCK_PORT/
Content-Type: application/json
@/tmp/bench-mcp-body.json
EOF

vegeta attack -targets=/tmp/bench-target-mcp-direct.txt -rate="$RPS" -duration="$DURATION" \
  > /tmp/bench-calibration-mcp.bin
vegeta report -type=text < /tmp/bench-calibration-mcp.bin
echo ""

echo -e "${CYAN}${BOLD}Phase 4: MCP Overhead (Vegeta → VoidLLM → Mock MCP)${RESET}"

cat > /tmp/bench-target-mcp-proxy.txt << EOF
POST http://localhost:$PROXY_PORT/api/v1/mcp/bench
Content-Type: application/json
Authorization: Bearer $API_KEY
@/tmp/bench-mcp-body.json
EOF

vegeta attack -targets=/tmp/bench-target-mcp-proxy.txt -rate="$RPS" -duration="$DURATION" \
  > /tmp/bench-overhead-mcp.bin
vegeta report -type=text < /tmp/bench-overhead-mcp.bin
echo ""

# ─── Summary ─────────────────────────────────────────────────────
echo -e "${YELLOW}${BOLD}━━━ Summary ━━━${RESET}"
echo ""
echo "LLM Proxy — Calibration (direct):"
vegeta report -type=text < /tmp/bench-calibration.bin | grep -E "Latencies|Success"
echo ""
echo "LLM Proxy — With Proxy:"
vegeta report -type=text < /tmp/bench-overhead.bin | grep -E "Latencies|Success"
echo ""
echo "MCP Proxy — Calibration (direct):"
vegeta report -type=text < /tmp/bench-calibration-mcp.bin | grep -E "Latencies|Success"
echo ""
echo "MCP Proxy — With Proxy:"
vegeta report -type=text < /tmp/bench-overhead-mcp.bin | grep -E "Latencies|Success"
echo ""

# ─── Compute overhead (awk — no external language dependency) ─────
echo "Overhead (computed):"

# Extracts the mean latency field from a vegeta text report.
# Vegeta prints: Latencies    [mean, 50, 95, 99, max]  <v1>, <v2>, ...
# We grab the first time value and normalise it to microseconds.
extract_mean_us() {
  local bin_file="$1"
  vegeta report -type=text < "$bin_file" | awk '
    /Latencies/ {
      # Grab the first time token after the brackets summary.
      # Format: "  Latencies     [mean, 50, 95, 99, max]  10.123ms, ..."
      match($0, /\[mean[^\]]+\][[:space:]]+([0-9.]+)(ms|µs|us|s)/, arr)
      if (RSTART == 0) {
        # Fallback: grab first numeric+unit token anywhere on the line.
        match($0, /([0-9.]+)(ms|µs|us|s)/, arr)
      }
      val  = arr[1] + 0
      unit = arr[2]
      if (unit == "ms")       print val * 1000
      else if (unit == "s")   print val * 1000000
      else                    print val   # µs or us
      exit
    }
  '
}

compute_overhead() {
  local label="$1"
  local calib_bin="$2"
  local proxy_bin="$3"

  calib_us=$(extract_mean_us "$calib_bin")
  proxy_us=$(extract_mean_us "$proxy_bin")

  awk -v label="$label" -v c="$calib_us" -v p="$proxy_us" 'BEGIN {
    diff = p - c
    printf "  %s: mean overhead = %.0fµs (calib %.0fµs → proxy %.0fµs)\n", label, diff, c, p
  }'
}

compute_overhead "LLM Proxy" /tmp/bench-calibration.bin     /tmp/bench-overhead.bin
compute_overhead "MCP Proxy" /tmp/bench-calibration-mcp.bin /tmp/bench-overhead-mcp.bin

# ─── Phase 5: Code Mode (go test -bench) ─────────────────────────
echo ""
echo -e "${CYAN}${BOLD}Phase 5: Code Mode (WASM sandbox)${RESET}"
echo -e "${DIM}Running go test -bench (in-process, not HTTP)...${RESET}"
echo ""

BENCH_OUTPUT=$(go test ./internal/mcp/... -bench=Benchmark -benchtime=3s -count=1 -timeout=120s 2>&1)

# Extract ns/op values and convert to human-readable
echo "$BENCH_OUTPUT" | awk '
  /^Benchmark/ && /ns\/op/ {
    name = $1
    ns   = $3
    ms   = ns / 1000000
    us   = ns / 1000
    sub(/^Benchmark/, "", name)
    sub(/-[0-9]+$/, "", name)
    if (ms >= 1) printf "  %-30s %8.2f ms\n", name, ms
    else         printf "  %-30s %8.0f µs\n", name, us
  }
'
echo ""

# ─── Final Summary ───────────────────────────────────────────────
echo -e "${YELLOW}${BOLD}━━━ Complete Benchmark Summary ━━━${RESET}"
echo ""
compute_overhead "LLM Proxy" /tmp/bench-calibration.bin     /tmp/bench-overhead.bin
compute_overhead "MCP Proxy" /tmp/bench-calibration-mcp.bin /tmp/bench-overhead-mcp.bin
echo ""
echo "  Code Mode (go test -bench):"
echo "$BENCH_OUTPUT" | awk '
  /^BenchmarkExecute_NoTools/ && /ns\/op/ {
    printf "    Pure JS:           %8.2f ms\n", $3/1000000
  }
  /^BenchmarkExecute_WithToolCall/ && /ns\/op/ {
    printf "    With Tool Call:    %8.2f ms\n", $3/1000000
  }
  /^BenchmarkRuntimePool_AcquireRelease/ && /ns\/op/ {
    printf "    Pool Cycle:        %8.2f ms\n", $3/1000000
  }
  /^BenchmarkCodeMode_WarmEval/ && /ns\/op/ {
    printf "    Warm JS Eval:      %8.0f µs\n", $3/1000
  }
'

echo ""
echo -e "${DIM}Raw data in /tmp/bench-*.bin${RESET}"
