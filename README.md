# VoidLLM

[![CI](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml/badge.svg)](https://github.com/voidmind-io/voidllm/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/voidmind-io/voidllm)](https://goreportcard.com/report/github.com/voidmind-io/voidllm)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/voidmind-io/voidllm/badge)](https://securityscorecards.dev/viewer/?uri=github.com/voidmind-io/voidllm)
[![Release](https://img.shields.io/github/v/release/voidmind-io/voidllm)](https://github.com/voidmind-io/voidllm/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/voidmind-io/voidllm)](go.mod)
[![License: BSL 1.1](https://img.shields.io/badge/License-BSL_1.1-blue.svg)](LICENSE)

**A lightweight, privacy-first LLM proxy for teams that take control seriously.**

VoidLLM sits between your applications and LLM providers — self-hosted or managed — giving you organization-wide access control, usage tracking, and key management. One binary, sub-2ms overhead, zero knowledge of your prompts.

![VoidLLM Dashboard](docs/screenshots/VoidLLM-Dashboard.jpg)

<details>
<summary>More screenshots</summary>

![Usage Analytics](docs/screenshots/VoidLLM-Usage.jpg)
![API Keys](docs/screenshots/VoidLLM-Keys.jpg)
![Playground](docs/screenshots/VoidLLM-Playground.jpg)

</details>

> **Privacy by Design:** VoidLLM never stores, logs, or persists any prompt or response content. Not as a setting you can toggle — by architecture. The proxy is a zero-knowledge pass-through. Only metadata is tracked: who made the request, which model, how many tokens, how long it took. Your data stays yours. GDPR-compliant from day one.

---

## Why VoidLLM?

| Problem | VoidLLM Solution |
|---|---|
| Teams share raw API keys in Slack | Virtual keys with org/team/user scoping |
| No visibility into who's spending what | Per-key, per-team, per-org usage tracking + cost estimation |
| One runaway script burns the monthly budget | Rate limits + token budgets at every level |
| Switching providers means changing every app | Model aliases — clients call `default`, you route it anywhere |
| Existing proxies log your prompts | Zero-knowledge architecture — content never touches disk |

## Quick Start

```bash
# Generate required keys
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)

# Start with Docker
docker run -p 8080:8080 \
  -e VOIDLLM_ADMIN_KEY -e VOIDLLM_ENCRYPTION_KEY \
  -v $(pwd)/voidllm.yaml:/etc/voidllm/voidllm.yaml:ro \
  -v voidllm_data:/data \
  voidllm:latest
```

Open `http://localhost:8080` — log in, create keys, start proxying.

```bash
# Your apps just point here instead of the provider
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer vl_uk_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"hello"}]}'
```

Any OpenAI SDK works out of the box — just change the base URL.

## Features

### For Everyone (Community, free)

- **OpenAI-compatible proxy** — `/v1/chat/completions`, embeddings, images, audio
- **Provider adapters** — Anthropic, Azure OpenAI, Ollama, vLLM, OpenAI, any custom endpoint
- **Full Web UI** — dashboard, playground, key management, teams, models, usage, settings
- **Org → Team → User → Key hierarchy** with RBAC (system_admin, org_admin, team_admin, member)
- **Rate limiting** — requests per minute/day, most-restrictive-wins across org/team/key
- **Token budgets** — daily/monthly limits, enforced in real-time
- **Usage tracking** — tokens, cost, duration, TTFT — async, never blocks
- **Model aliases** — clients call `default`, you decide where it goes
- **Per-model timeouts** and **circuit breakers** for upstream resilience
- **14 Prometheus metrics** — proxy latency, tokens, active streams, cache, DB pool
- **Streaming (SSE)** — transparent pass-through with per-chunk usage extraction
- **SQLite or PostgreSQL** — zero-dep default or production-grade
- **Helm chart** — production-ready Kubernetes deployment
- **Graceful shutdown** — phased drain, in-flight request tracking, K8s-ready

### For Teams (Pro, $299/mo)

- Cost reports with model breakdown and daily trends
- Usage export (CSV)
- Extended data retention
- Priority email support

### For Enterprises (Enterprise, $799/mo)

- **SSO / OIDC** — Google, Azure AD, Okta, Keycloak, any OIDC provider
- **Per-org SSO config** — each organization gets its own Identity Provider
- **Auto-provisioning** — users created automatically from allowed email domains
- **Group sync** — OIDC groups → VoidLLM teams
- **Audit logs** — every admin action logged, filterable API + UI
- **OpenTelemetry tracing** — OTLP/gRPC export to Jaeger, Tempo, Datadog
- **Request ID correlation** — trace a single request across logs, usage, audit, upstream
- Dedicated Slack support

Flat pricing — no per-user fees, no per-request charges. Self-hosted on your infrastructure.

---

## Documentation

- **[Configuration Reference](docs/configuration.md)** — all YAML settings with examples
- **[Deployment Guide](docs/deployment.md)** — Docker, Helm, Kubernetes, PostgreSQL, Redis
- **[API Reference](docs/api.md)** — all endpoints, request/response formats
- **[Enterprise Guide](docs/enterprise.md)** — SSO setup, license activation, audit logs, OTel

## Configuration

```yaml
server:
  proxy:
    port: 8080

models:
  - name: dolphin-mistral
    provider: ollama
    base_url: http://localhost:11434/v1
    timeout: 30s
    aliases: [default]
    pricing:
      input_per_1m: 0.15
      output_per_1m: 0.60

  - name: claude-sonnet
    provider: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    timeout: 5m

settings:
  admin_key: ${VOIDLLM_ADMIN_KEY}
  encryption_key: ${VOIDLLM_ENCRYPTION_KEY}
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

This is not a feature toggle. It's an architectural decision.

- **No request body** in logs, DB, or any persistent storage
- **No response body** in logs, DB, or any persistent storage
- **No prompt caching** — content passes through memory only
- **Usage events** contain only: who (key/org/team), what (model), how much (tokens/cost)
- There is no `enable_content_logging` option. It doesn't exist.

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
