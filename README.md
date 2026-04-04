# VoidLLM

[![CI](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml/badge.svg)](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/voidmind-io/voidllm/graph/badge.svg?token=1OCK31RDMG)](https://codecov.io/gh/voidmind-io/voidllm)
[![Go Report Card](https://goreportcard.com/badge/github.com/voidmind-io/voidllm)](https://goreportcard.com/report/github.com/voidmind-io/voidllm)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/voidllm)](https://artifacthub.io/packages/search?repo=voidllm)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/voidmind-io/voidllm/badge)](https://securityscorecards.dev/viewer/?uri=github.com/voidmind-io/voidllm)
[![Snyk](https://snyk.io/test/github/voidmind-io/voidllm/badge.svg)](https://snyk.io/test/github/voidmind-io/voidllm)
[![Release](https://img.shields.io/github/v/release/voidmind-io/voidllm)](https://github.com/voidmind-io/voidllm/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/voidmind-io/voidllm)](go.mod)
[![License: BSL 1.1](https://img.shields.io/badge/License-BSL_1.1-blue.svg)](LICENSE)

**A privacy-first LLM proxy and AI gateway for teams that take control seriously.**

VoidLLM is a self-hosted LLM proxy that sits between your applications and LLM providers - OpenAI, Anthropic, Azure, Ollama, vLLM, or any custom endpoint. It gives you organization-wide access control, API key management, usage tracking, rate limiting, and multi-deployment load balancing. One Go binary, sub-2ms proxy overhead, zero knowledge of your prompts.

![VoidLLM Dashboard](docs/screenshots/VoidLLM-Dashboard.jpg)

<details>
<summary>More screenshots</summary>

![Usage Analytics](docs/screenshots/VoidLLM-Usage.jpg)
![API Keys](docs/screenshots/VoidLLM-Keys.jpg)
![Playground](docs/screenshots/VoidLLM-Playground.jpg)

</details>

> **Privacy-First by Design:** VoidLLM is a zero-knowledge LLM proxy - it never stores, logs, or persists any prompt or response content. Not as a setting you can toggle - by architecture. Only metadata is tracked: who made the request, which model, how many tokens, how long it took. Your data stays yours.

---

## Why VoidLLM?

| Problem | How VoidLLM solves it |
|---|---|
| Teams share raw API keys in Slack | Virtual keys with org/team/user scoping and RBAC |
| No visibility into who's spending what | Per-key, per-team, per-org usage tracking + cost estimation |
| One runaway script burns the monthly budget | Rate limits + token budgets enforced by the proxy at every level |
| Switching providers means changing every app | Model aliases - clients call `default`, the proxy routes it anywhere |
| Provider goes down, everything breaks | Multi-deployment load balancing with automatic failover |
| Existing proxies log your prompts | Zero-knowledge proxy architecture - content never touches disk |

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

### Binary (no Docker needed)

Download the latest binary for your platform from the [releases page](https://github.com/voidmind-io/voidllm/releases/latest):

```bash
# Linux
curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-linux-amd64.tar.gz | tar xz
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
./voidllm
```

Available for: Linux (amd64, arm64), Windows (amd64, arm64), macOS (amd64, arm64).

On first start, VoidLLM prints your credentials to stdout:

```
========================================
 BOOTSTRAP COMPLETE - COPY THESE NOW
========================================
  API Key:    vl_uk_a3f2...
  Email:      admin@voidllm.local
  Password:   <random>
========================================
```

Open `http://localhost:8080`, log in with the email and password above, and start proxying. The API key is used for SDK calls (`Authorization: Bearer vl_uk_...`). These credentials are shown once - save them.

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

Any OpenAI-compatible SDK works out of the box - just change the base URL to your VoidLLM proxy.

## Features

| Feature | Details |
|---|---|
| OpenAI-compatible proxy | `/v1/chat/completions`, embeddings, images, audio, streaming |
| Multi-provider routing | OpenAI, Anthropic, Azure, Ollama, vLLM, any custom endpoint |
| Load balancing | Round-robin, least-latency, weighted, priority across deployments |
| Automatic failover | Retry on 5xx/timeout, circuit breakers, health-aware routing |
| Web UI | Dashboard, playground, API keys, teams, models, usage, settings |
| RBAC | Org > Team > User > Key hierarchy, 4 roles |
| Rate limits | Requests per minute/day, most-restrictive-wins across levels |
| Token budgets | Daily/monthly limits, real-time enforcement |
| Usage tracking | Tokens, cost, duration, TTFT per request |
| Model aliases | Clients call `default`, you control where it routes |
| MCP gateway | Proxy external MCP servers with access control and session management |
| Code Mode | WASM-sandboxed JS for multi-tool orchestration |
| Prometheus metrics | Latency, tokens, active streams, routing, health |
| Database | SQLite (default) or PostgreSQL |
| Deployment | Docker, Helm chart, graceful shutdown |
| | |
| **Pro ($49/mo)** | **Everything above, plus:** |
| Cost reports | Model breakdown, daily trends |
| Usage export | CSV download |
| Data retention | Extended |
| Support | Priority email |
| | |
| **Enterprise ($149/mo)** | **Everything in Pro, plus:** |
| SSO / OIDC | Google, Azure AD, Okta, Keycloak, any provider |
| Per-org SSO | Each organization gets its own Identity Provider |
| Auto-provisioning | Users created from allowed email domains |
| Group sync | OIDC groups mapped to VoidLLM teams |
| Audit logs | Every admin action, filterable API + UI |
| OpenTelemetry | OTLP/gRPC export, request ID correlation |
| Support | Dedicated Slack |

**Founding Member ($999 one-time):** All Enterprise features, lifetime license, Product Advisory Board, direct founder access. Limited spots.

Flat pricing - no per-user fees, no per-request charges. Self-hosted on your infrastructure.

---

## MCP Gateway

VoidLLM is an [MCP](https://modelcontextprotocol.io) gateway - it exposes built-in management tools and proxies requests to external MCP servers with access control, usage tracking, and automatic session management.

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

Register external MCP servers via the Admin UI or API. VoidLLM proxies tool calls through `/api/v1/mcp/:alias` with scoped access control (global, org, or team level), automatic session management, usage tracking, and Prometheus metrics.

### Code Mode

Code Mode lets LLMs write JavaScript that orchestrates multiple MCP tool calls in a single execution - instead of one tool call per LLM turn. The JS runs in a WASM-sandboxed QuickJS runtime with no filesystem, no network, and no host access. Reduces token usage by 30-80%.

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

- **SSE transport not supported** - MCP servers using the deprecated SSE protocol (pre 2025-03-26 spec) are auto-detected and deactivated. Use servers that support Streamable HTTP.
- **No OAuth for upstream MCP servers** - servers requiring per-user OAuth (Jira, Slack, Google) are not yet supported. API key and header auth work.
- **Single instance only** - Code Mode's WASM runtime pool is in-memory. Multi-pod deployments require Redis support (coming soon).

---

## Documentation

**[Full documentation](docs/index.md)** | **[Blog](https://voidllm.ai/blog)** | **[FAQ](https://voidllm.ai/faq)**

| Topic | Guide |
|---|---|
| Getting Started | [Quick Start](docs/getting-started.md) |
| Configuration | [All YAML settings](docs/configuration.md) |
| Docker | [Docker deployment](docs/deployment/docker.md) |
| Kubernetes | [Helm chart](docs/deployment/kubernetes.md) |
| Providers | [OpenAI, Anthropic, Azure, Ollama, vLLM](docs/models/providers.md) |
| Load Balancing | [Strategies, failover, circuit breakers](docs/models/load-balancing.md) |
| MCP Gateway | [Overview](docs/mcp/overview.md) - [Servers](docs/mcp/servers.md) - [Code Mode](docs/mcp/code-mode.md) - [IDE Setup](docs/mcp/ide-integration.md) |
| RBAC | [Roles and permissions](docs/security/rbac.md) |
| Privacy | [Zero-knowledge architecture](docs/security/privacy.md) |
| API Reference | [Endpoints and error codes](docs/api/overview.md) |
| Enterprise | [License](docs/enterprise/license.md) - [SSO](docs/enterprise/sso.md) - [Audit](docs/enterprise/audit.md) - [OTel](docs/enterprise/otel.md) - [Pricing](docs/enterprise/pricing.md) |
| Troubleshooting | [Common issues](docs/troubleshooting.md) |

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

  # Load balanced - multiple deployments with failover
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
- **No prompt caching** - content passes through memory only
- **Usage events** contain only: who (key/org/team), what (model), how much (tokens/cost)
- There is no `enable_content_logging` option. It doesn't exist.
- Designed to support GDPR compliance - no personal data in prompts is stored or processed

## CLI Tools

```bash
# Bidirectional database migration
voidllm migrate --from sqlite:///data/voidllm.db --to postgres://user:pass@host/db

# License management (for Enterprise)
voidllm license verify < license.jwt
```

## License

[Business Source License 1.1](LICENSE) - source available, self-hosting permitted,
competing hosted services prohibited. Converts to Apache 2.0 four years after each release.

---

Built by [VoidMind](https://voidmind.io) · [voidllm.ai](https://voidllm.ai)

This project was built with significant assistance from AI (Claude by Anthropic).
