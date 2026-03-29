#!/bin/bash
# VoidLLM Proxy Overhead Benchmark
# Measures the latency added by the proxy using Vegeta load tester.
#
# Method:
#   1. Calibration: Vegeta → Mock LLM directly (baseline)
#   2. Overhead: Vegeta → VoidLLM → Mock LLM (proxy path)
#   3. Compare latencies to determine proxy overhead
#
# Prerequisites: go, vegeta (auto-installed if missing)
# Usage: ./scripts/bench/run.sh [rps] [duration]

set -euo pipefail

export PATH=$PATH:~/go/bin

RPS=${1:-500}
DURATION=${2:-15s}
MOCK_PORT=9999
MCP_MOCK_PORT=9998
PROXY_PORT=8081

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

echo -e "${BOLD}╔══════════════════════════════════════════════╗${RESET}"
echo -e "${BOLD}║  VoidLLM Proxy Overhead Benchmark            ║${RESET}"
echo -e "${BOLD}║  Rate: ${RPS} req/s  Duration: ${DURATION}              ║${RESET}"
echo -e "${BOLD}╚══════════════════════════════════════════════╝${RESET}"
echo ""

# ─── Cleanup ──────────────────────────────────────────────────────
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MCP_MOCK_PID" ] && kill "$MCP_MOCK_PID" 2>/dev/null || true
  [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
  rm -f /tmp/bench-target-*.txt /tmp/bench-proxy.yaml /tmp/bench-body*.json \
        /tmp/bench-mock-llm /tmp/bench-mock-mcp /tmp/bench-voidllm-proxy \
        /tmp/bench-voidllm.log /tmp/bench-calibration*.bin /tmp/bench-overhead*.bin
}
trap cleanup EXIT

# Kill any leftovers from previous runs
pkill -f "bench-mock-llm|bench-voidllm-proxy" 2>/dev/null || true
sleep 1

# ─── Pre-build binaries ──────────────────────────────────────────
echo -e "${DIM}Building binaries...${RESET}"
go build -o /tmp/bench-mock-llm scripts/bench/mock-llm.go 2>&1 || { echo "ERROR: mock-llm build failed"; exit 1; }
go build -o /tmp/bench-mock-mcp scripts/bench/mock-mcp.go 2>&1 || { echo "ERROR: mock-mcp build failed"; exit 1; }
go build -o /tmp/bench-voidllm-proxy ./cmd/voidllm 2>&1 || { echo "ERROR: voidllm build failed"; exit 1; }
echo -e "${DIM}Built.${RESET}"

# ─── Start Mock Servers ───────────────────────────────────────────
echo -e "${DIM}Starting mock LLM on :${MOCK_PORT} (10ms latency)...${RESET}"
/tmp/bench-mock-llm -port $MOCK_PORT -latency 10ms > /dev/null 2>&1 &
MOCK_PID=$!

echo -e "${DIM}Starting mock MCP on :${MCP_MOCK_PORT} (10ms latency)...${RESET}"
/tmp/bench-mock-mcp -port $MCP_MOCK_PORT -latency 10ms > /dev/null 2>&1 &
MCP_MOCK_PID=$!

sleep 2

if ! curl -s http://localhost:$MOCK_PORT/v1/models > /dev/null 2>&1; then
  echo "ERROR: Mock LLM failed to start"
  exit 1
fi
if ! curl -s -X POST http://localhost:$MCP_MOCK_PORT/ -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}' > /dev/null 2>&1; then
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

# Wait for proxy to be ready
for i in $(seq 1 15); do
  if curl -s http://localhost:$PROXY_PORT/healthz > /dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! curl -s http://localhost:$PROXY_PORT/healthz > /dev/null 2>&1; then
  echo "ERROR: VoidLLM failed to start"
  cat /tmp/bench-voidllm.log | tail -20
  exit 1
fi

# Get API key from bootstrap log (may have leading whitespace)
API_KEY=$(grep -oP 'vl_uk_[a-f0-9]+' /tmp/bench-voidllm.log | head -1)
if [ -z "$API_KEY" ]; then
  echo "ERROR: Could not find API key in VoidLLM logs"
  cat /tmp/bench-voidllm.log | tail -20
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
SANITY=$(curl -s http://localhost:$PROXY_PORT/v1/chat/completions \
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
vegeta attack -targets=/tmp/bench-target-direct.txt -rate=$RPS -duration=$DURATION \
  > /tmp/bench-calibration.bin
vegeta report -type=text < /tmp/bench-calibration.bin
echo ""

# ─── Phase 2: Through proxy ──────────────────────────────────────
echo -e "${CYAN}${BOLD}Phase 2: Overhead (Vegeta → VoidLLM → Mock LLM)${RESET}"
vegeta attack -targets=/tmp/bench-target-proxy.txt -rate=$RPS -duration=$DURATION \
  > /tmp/bench-overhead.bin
vegeta report -type=text < /tmp/bench-overhead.bin
echo ""

# ─── Phase 3: MCP Proxy ───────────────────────────────────────────
echo -e "${CYAN}${BOLD}Phase 3: MCP Calibration (direct → Mock MCP)${RESET}"

echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock_tool","arguments":{"input":"bench"}}}' > /tmp/bench-mcp-body.json

cat > /tmp/bench-target-mcp-direct.txt << EOF
POST http://localhost:$MCP_MOCK_PORT/
Content-Type: application/json
@/tmp/bench-mcp-body.json
EOF

vegeta attack -targets=/tmp/bench-target-mcp-direct.txt -rate=$RPS -duration=$DURATION \
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

vegeta attack -targets=/tmp/bench-target-mcp-proxy.txt -rate=$RPS -duration=$DURATION \
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
# ─── Compute overhead ─────────────────────────────────────────────
echo "Overhead (computed):"
python3 -c "
import re, subprocess

def parse_us(s):
    s = s.strip()
    if s.endswith('ms'):
        return float(s[:-2]) * 1000
    if s.endswith('µs') or s.endswith('us'):
        return float(s[:-2])
    if s.endswith('s'):
        return float(s[:-1]) * 1000000
    return float(s)

def extract(line):
    m = re.findall(r'[\d.]+(?:ms|µs|us|s)', line)
    return [parse_us(x) for x in m[:5]]

def compute(name, calib_file, proxy_file):
    c = subprocess.run(['vegeta', 'report', '-type=text'], stdin=open(calib_file,'rb'), capture_output=True, text=True).stdout
    p = subprocess.run(['vegeta', 'report', '-type=text'], stdin=open(proxy_file,'rb'), capture_output=True, text=True).stdout
    cl = [l for l in c.split('\n') if 'Latencies' in l][0]
    pl = [l for l in p.split('\n') if 'Latencies' in l][0]
    cv, pv = extract(cl), extract(pl)
    print(f'  {name}:')
    for i, label in enumerate(['mean', 'p50', 'p95', 'p99', 'max']):
        diff = pv[i] - cv[i]
        print(f'    {label:>4}: {diff:>8.0f}µs')

compute('LLM Proxy', '/tmp/bench-calibration.bin', '/tmp/bench-overhead.bin')
compute('MCP Proxy', '/tmp/bench-calibration-mcp.bin', '/tmp/bench-overhead-mcp.bin')
" 2>/dev/null || echo -e "${DIM}  (install python3 for auto-calculation)${RESET}"

echo ""
echo -e "${DIM}Raw data in /tmp/bench-*.bin${RESET}"
