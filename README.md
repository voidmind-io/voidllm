# VoidLLM

[![CI](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml/badge.svg)](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/voidmind-io/voidllm/graph/badge.svg?token=1OCK31RDMG)](https://codecov.io/gh/voidmind-io/voidllm)
[![Go Report Card](https://goreportcard.com/badge/github.com/voidmind-io/voidllm)](https://goreportcard.com/report/github.com/voidmind-io/voidllm)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/voidllm)](https://artifacthub.io/packages/search?repo=voidllm)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/voidmind-io/voidllm/badge)](https://securityscorecards.dev/viewer/?uri=github.com/voidmind-io/voidllm)
[![Release](https://img.shields.io/github/v/release/voidmind-io/voidllm)](https://github.com/voidmind-io/voidllm/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/voidmind-io/voidllm)](go.mod)
[![License: BSL 1.1](https://img.shields.io/badge/License-BSL_1.1-blue.svg)](LICENSE)

**A privacy-first LLM proxy and AI gateway for teams that take control seriously.**

VoidLLM is a self-hosted LLM proxy that sits between your applications and LLM providers — OpenAI, Anthropic, Azure, Ollama, vLLM, or any custom endpoint. It gives you organization-wide access control, API key management, usage tracking, rate limiting, and multi-deployment load balancing. One Go binary, sub-2ms proxy overhead, zero knowledge of your prompts.

![VoidLLM Dashboard](docs/screenshots/VoidLLM-Dashboard.jpg)

<details>
<summary>More screenshots</summary>

![Usage Analytics](docs/screenshots/VoidLLM-Usage.jpg)
![API Keys](docs/screenshots/VoidLLM-Keys.jpg)
![Playground](docs/screenshots/VoidLLM-Playground.jpg)

</details>

> **Privacy-First by Design:** VoidLLM is a zero-knowledge LLM proxy — it never stores, logs, or persists any prompt or response content. Not as a setting you can toggle — by architecture. Only metadata is tracked: who made the request, which model, how many tokens, how long it took. Your data stays yours. GDPR-compliant from day one.

---

## Why VoidLLM?

| Problem | How VoidLLM solves it |
|---|---|
| Teams share raw API keys in Slack | Virtual keys with org/team/user scoping and RBAC |
| No visibility into who's spending what | Per-key, per-team, per-org usage tracking + cost estimation |
| One runaway script burns the monthly budget | Rate limits + token budgets enforced by the proxy at every level |
| Switching providers means changing every app | Model aliases — clients call `default`, the proxy routes it anywhere |
| Provider goes down, everything breaks | Multi-deployment load balancing with automatic failover |
| Existing proxies log your prompts | Zero-knowledge proxy architecture — content never touches disk |

## Quick Start

```bash
# Generate required keys
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)

# Start the LLM proxy with Docker
docker run -p 8080:8080 \
  -e VOIDLLM_ADMIN_KEY -e VOIDLLM_ENCRYPTION_KEY \
  -v $(pwd)/voidllm.yaml:/etc/voidllm/voidllm.yaml:ro \
  -v voidllm_data:/data \
  ghcr.io/voidmind-io/voidllm:latest
```

Open `http://localhost:8080` — log in, create API keys, start proxying.

### One-Click Deploy

[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/deploy/wild-pure?referralCode=fw9l7c)

Keys are auto-generated. Open the URL Railway gives you and start adding models.

```bash
# Your apps just point at the proxy instead of the provider
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer vl_uk_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"hello"}]}'
```

Any OpenAI-compatible SDK works out of the box — just change the base URL to your VoidLLM proxy.

## Features

### LLM Proxy — Community (free)

- **OpenAI-compatible proxy** — `/v1/chat/completions`, embeddings, images, audio
- **Multi-provider routing** — Anthropic, Azure OpenAI, Ollama, vLLM, OpenAI, any custom endpoint
- **Load balancing** — multi-deployment models with round-robin, least-latency, weighted, or priority routing
- **Automatic failover** — retry on 5xx/timeout, per-deployment circuit breakers, health-aware routing
- **Full Web UI** — dashboard, playground, API key management, teams, models, usage, settings
- **Org → Team → User → Key hierarchy** with RBAC (system_admin, org_admin, team_admin, member)
- **Rate limiting** — requests per minute/day, most-restrictive-wins across org/team/key
- **Token budgets** — daily/monthly limits, enforced in real-time by the proxy
- **Usage tracking** — tokens, cost, duration, TTFT — async, never blocks the proxy
- **Model aliases** — clients call `default`, you decide where the proxy routes it
- **Per-model timeouts** and **circuit breakers** for upstream resilience
- **Prometheus metrics** — proxy latency, tokens, active streams, routing retries, health status
- **Streaming (SSE)** — transparent proxy pass-through with per-chunk usage extraction
- **SQLite or PostgreSQL** — zero-dep default or production-grade
- **Helm chart** — production-ready Kubernetes deployment
- **MCP gateway** — proxy and manage external MCP servers with access control, session management, and usage tracking
- **MCP tools** — built-in management tools for IDE integration (Claude Code, Cursor, Windsurf)
- **Code Mode** — LLMs write JavaScript to orchestrate multiple MCP tool calls in one WASM-sandboxed execution
- **Graceful shutdown** — phased drain, in-flight request tracking, K8s-ready

### Pro ($49/mo)

- Cost reports with model breakdown and daily trends
- Usage export (CSV)
- Extended data retention
- Priority email support

### Enterprise ($149/mo)

- **SSO / OIDC** - Google, Azure AD, Okta, Keycloak, any OIDC provider
- **Per-org SSO config** - each organization gets its own Identity Provider
- **Auto-provisioning** - users created automatically from allowed email domains
- **Group sync** - OIDC groups -> VoidLLM teams
- **Audit logs** - every admin action logged, filterable API + UI
- **OpenTelemetry tracing** - OTLP/gRPC export to Jaeger, Tempo, Datadog
- **Request ID correlation** - trace a single request across the proxy, logs, usage, audit, upstream
- Dedicated Slack support

### Founding Member ($999 one-time)

- All Enterprise features - current and future
- Lifetime license - no recurring fees
- Product Advisory Board membership
- Direct founder access + priority support
- Limited spots available

Flat pricing - no per-user fees, no per-request charges. Self-hosted on your infrastructure.

---

## MCP Gateway

VoidLLM is an [MCP](https://modelcontextprotocol.io) gateway — it exposes built-in management tools and proxies requests to external MCP servers with access control, usage tracking, and automatic session management.

### Built-in Tools

| Tool | Description |
|---|---|
| `list_models` | List models with health status (RBAC-scoped) |
| `get_model_health` | Health status for a specific model or deployment |
| `get_usage` | Usage stats for your key/team/org |
| `list_keys` | API keys visible to you |
| `create_key` | Create a temporary API key |
| `list_deployments` | Deployment details (system_admin only) |

### External MCP Servers

Register external MCP servers via the Admin UI or API. VoidLLM proxies tool calls through `/api/v1/mcp/:alias` with:
- **Scoped access control** — global, org, or team level
- **Session management** — automatic initialize + session ID forwarding
- **Usage tracking** — who called which tool, when, how long
- **Prometheus metrics** — tool call counts, duration, errors

### Code Mode

Code Mode lets LLMs write JavaScript that orchestrates multiple MCP tool calls in a single execution — instead of one tool call per LLM turn. The JS runs in a WASM-sandboxed QuickJS runtime with no filesystem, no network, and no host access. Reduces token usage by 30-80%.

```yaml
mcp:
  code_mode:
    enabled: true
    pool_size: 8          # concurrent WASM runtimes
    memory_limit_mb: 16   # per execution
    timeout: 30s          # per execution
    max_tool_calls: 50    # per execution
```

Code Mode exposes three tools on `/api/v1/mcp`:

| Tool | Description |
|---|---|
| `list_servers` | Discover available MCP servers and tool counts |
| `search_tools` | Find tools by keyword across all servers |
| `execute_code` | Run JS with MCP tools as `await tools.alias.toolName(args)` |

TypeScript type declarations are auto-generated from tool schemas and included in the `execute_code` description, so LLMs see available tools and argument types at `tools/list` time.

Admins can block specific tools from Code Mode via the per-tool blocklist API and UI.

### IDE Setup

```json
{
  "mcpServers": {
    "voidllm": {
      "type": "http",
      "url": "http://your-voidllm-instance:8080/api/v1/mcp",
      "headers": { "Authorization": "Bearer vl_uk_your_key" }
    }
  }
}
```

This connects your IDE (Claude Code, Cursor, Windsurf) to the Code Mode endpoint. Management tools (list_models, get_usage, etc.) are available at `/api/v1/mcp/voidllm`. External MCP servers at `/api/v1/mcp/:alias`.

### Known Limitations

- **SSE transport not supported** — MCP servers using the deprecated SSE protocol (pre 2025-03-26 spec) are auto-detected and deactivated. Use servers that support Streamable HTTP.
- **No OAuth for upstream MCP servers** — servers requiring per-user OAuth (Jira, Slack, Google) are not yet supported. API key and header auth work.
- **Single instance only** — Code Mode's WASM runtime pool is in-memory. Multi-pod deployments require Redis support (coming soon).

---

## Documentation

- **[Configuration Reference](docs/configuration.md)** — all YAML settings with examples
- **[Deployment Guide](docs/deployment.md)** — Docker, Helm, Kubernetes, PostgreSQL, Redis
- **[API Reference](docs/api.md)** — all proxy and admin API endpoints
- **[Enterprise Guide](docs/enterprise.md)** — SSO setup, license activation, audit logs, OTel

## Configuration

```yaml
server:
  proxy:
    port: 8080

models:
  # Single endpoint
  - name: dolphin-mistral
    provider: ollama
    base_url: http://localhost:11434/v1
    timeout: 30s
    aliases: [default]
    pricing:
      input_per_1m: 0.15
      output_per_1m: 0.60

  # Load balanced — multiple deployments with failover
  - name: gpt-4o
    strategy: round-robin
    aliases: [smart]
    deployments:
      - name: azure-east
        provider: azure
        base_url: https://eastus.openai.azure.com
        api_key: ${AZURE_EAST_KEY}
        azure_deployment: gpt-4o
        priority: 1
      - name: openai-fallback
        provider: openai
        base_url: https://api.openai.com/v1
        api_key: ${OPENAI_KEY}
        priority: 2

mcp_servers:
  - name: AWS Knowledge
    alias: aws
    url: https://knowledge-mcp.global.api.aws
    auth_type: none

settings:
  admin_key: ${VOIDLLM_ADMIN_KEY}
  encryption_key: ${VOIDLLM_ENCRYPTION_KEY}
  mcp:
    code_mode:
      enabled: true
```

Supported providers: `openai` · `anthropic` · `azure` · `vllm` · `ollama` · `custom`

Environment variables are interpolated with `${VAR}` syntax. Secrets never hardcoded.

## Deployment

### Docker Compose

```bash
cp voidllm.yaml.example voidllm.yaml
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
docker-compose up
```

### Kubernetes (Helm)

```bash
helm install voidllm chart/voidllm/ \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32) \
  --set config.models[0].name=my-model \
  --set config.models[0].provider=ollama \
  --set config.models[0].base_url=http://ollama:11434/v1
```

PostgreSQL and Redis are available as optional subcharts for production deployments.

### From Source

```bash
# Prerequisites: Go 1.23+, Node 20+
cd ui && npm ci && npm run build && cd ..
go run ./cmd/voidllm --config voidllm.yaml
```

## Privacy

This is not a feature toggle. It's an architectural decision that makes VoidLLM a privacy-first LLM proxy.

- **No request body** in logs, DB, or any persistent storage
- **No response body** in logs, DB, or any persistent storage
- **No prompt caching** — content passes through memory only
- **Usage events** contain only: who (key/org/team), what (model), how much (tokens/cost)
- There is no `enable_content_logging` option. It doesn't exist.
- **GDPR-compliant** by architecture, not by configuration

## CLI Tools

```bash
# Bidirectional database migration
voidllm migrate --from sqlite:///data/voidllm.db --to postgres://user:pass@host/db

# License management (for Enterprise)
voidllm license verify < license.jwt
```

## License

[Business Source License 1.1](LICENSE) — source available, self-hosting permitted,
competing hosted services prohibited. Converts to Apache 2.0 four years after each release.

---

Built by [VoidMind](https://voidmind.io) · [voidllm.ai](https://voidllm.ai)

This project was built with significant assistance from AI (Claude by Anthropic).
