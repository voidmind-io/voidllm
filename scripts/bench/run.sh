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
PROXY_PORT=8081

MOCK_PID=""
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
  [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
  rm -f /tmp/bench-target-*.txt /tmp/bench-proxy.yaml /tmp/bench-body.json \
        /tmp/bench-mock-llm /tmp/bench-voidllm-proxy /tmp/bench-voidllm.log \
        /tmp/bench-calibration.bin /tmp/bench-overhead.bin
}
trap cleanup EXIT

# Kill any leftovers from previous runs
pkill -f "bench-mock-llm|bench-voidllm-proxy" 2>/dev/null || true
sleep 1

# ─── Pre-build binaries ──────────────────────────────────────────
echo -e "${DIM}Building binaries...${RESET}"
go build -o /tmp/bench-mock-llm scripts/bench/mock-llm.go 2>&1 || { echo "ERROR: mock build failed"; exit 1; }
go build -o /tmp/bench-voidllm-proxy ./cmd/voidllm 2>&1 || { echo "ERROR: voidllm build failed"; exit 1; }
echo -e "${DIM}Built.${RESET}"

# ─── Start Mock LLM ──────────────────────────────────────────────
echo -e "${DIM}Starting mock LLM on :${MOCK_PORT} (10ms latency)...${RESET}"
/tmp/bench-mock-llm -port $MOCK_PORT -latency 10ms > /dev/null 2>&1 &
MOCK_PID=$!
sleep 2

if ! curl -s http://localhost:$MOCK_PORT/v1/models > /dev/null 2>&1; then
  echo "ERROR: Mock LLM failed to start"
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

settings:
  admin_key: bench-admin-key-12345678901234567890
  encryption_key: bench-encryption-key-1234567890
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

# ─── Summary ─────────────────────────────────────────────────────
echo -e "${YELLOW}${BOLD}━━━ Summary ━━━${RESET}"
echo ""
echo "Calibration (direct):"
vegeta report -type=text < /tmp/bench-calibration.bin | grep -E "Latencies|Success"
echo ""
echo "With Proxy:"
vegeta report -type=text < /tmp/bench-overhead.bin | grep -E "Latencies|Success"
echo ""
# ─── Compute overhead ─────────────────────────────────────────────
echo "Proxy Overhead (computed):"
python3 -c "
import re, sys

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
    return [parse_us(x) for x in m[:5]]  # mean, 50, 95, 99, max

calib = open('/tmp/bench-calibration.bin', 'rb')
proxy = open('/tmp/bench-overhead.bin', 'rb')
import subprocess
c = subprocess.run(['vegeta', 'report', '-type=text'], stdin=calib, capture_output=True, text=True).stdout
p = subprocess.run(['vegeta', 'report', '-type=text'], stdin=proxy, capture_output=True, text=True).stdout

cl = [l for l in c.split('\n') if 'Latencies' in l][0]
pl = [l for l in p.split('\n') if 'Latencies' in l][0]

cv = extract(cl)
pv = extract(pl)

labels = ['mean', 'p50', 'p95', 'p99', 'max']
for i, label in enumerate(labels):
    diff = pv[i] - cv[i]
    print(f'  {label:>4}: {diff:>8.0f}µs  (proxy: {pv[i]:.0f}µs - direct: {cv[i]:.0f}µs)')
" 2>/dev/null || echo -e "${DIM}  (install python3 for auto-calculation)${RESET}"

echo ""
echo -e "${DIM}Raw data: /tmp/bench-calibration.bin, /tmp/bench-overhead.bin${RESET}"
echo -e "${DIM}HDR histogram: vegeta report -type=hdrplot < /tmp/bench-overhead.bin${RESET}"
