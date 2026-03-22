# Changelog

All notable changes to VoidLLM are documented in this file.

## [0.3.0] — 2026-03-21

### v0.3 "Production Ready"

Large teams and enterprises can safely run VoidLLM in production.
First enterprise features ship.

#### Reliability
- **Graceful shutdown** — phased drain with configurable timeout, force-cancel, second-signal hard exit
- **Configurable timeouts per model** — `timeout` field on model config (YAML + API)
- **Circuit breaker** — per-model, Closed/Open/HalfOpen states, configurable threshold + timeout

#### Enterprise
- **Audit logs** — async middleware captures all admin API mutations, filterable API + UI, config-gated
- **SSO / OIDC** — generic OpenID Connect, works with Google/Azure AD/Okta/Keycloak/any OIDC provider, auto-provisioning, group sync
- **License verification** — Ed25519-signed JWT, offline-verifiable, feature-based gating
- **OpenTelemetry tracing** — OTLP/gRPC export, proxy spans, slog trace correlation

#### Observability
- **14 Prometheus metrics** — proxy requests, duration, TTFT, tokens, active streams, upstream errors, rate limit rejections, circuit breaker rejections, cache sizes, DB pool, usage buffer depth
- **Request ID** — UUIDv7 per request, in error responses, logs, usage events, audit events, X-Request-Id header

#### Key Management
- **Key rotation** — `POST /orgs/:id/keys/:id/rotate` with 24h grace period

#### Architecture (internal)
- **`apierror` package** — unified error responses across auth, admin, proxy
- **Handle() decomposition** — 370 → 70 lines, 8 private helper methods
- **Application struct** — main.go 780 → 70 lines, clean lifecycle management
- **N+1 fix** — ServiceAccounts use LEFT JOIN with counts
- **Single-parse body** — reduced JSON parse/serialize from 4x to 1x
- **HTTP/2 transport** — ForceAttemptHTTP2 for upstream connections
- **Cursor pagination** — audit logs switched from OFFSET/LIMIT to cursor-based
- **Static analysis clean** — staticcheck, golangci-lint, gosec all pass

---

## [0.2.0] — 2026-03-20

### v0.2 "Usable by Teams"

Team leads can manage their deployment without touching curl.
UI foundation, model management via API, PostgreSQL, Helm chart.

#### UI
- Dashboard, Keys, Teams, Users, Service Accounts, Models, Usage, Settings, License pages
- Login + session auth, invite token system, role-aware sidebar

#### Model Management
- Model CRUD via Admin API, activate/deactivate, aliases, access control
- Ollama provider support, Test Connection endpoint

#### Storage & Deployment
- PostgreSQL support (pgx/v5), bidirectional migration tool
- Redis support (pub/sub invalidation, rate limiting, token budgets)
- Helm chart with PG/Redis subcharts, security hardened
- OpenAPI spec (60+ annotated handlers, Swagger UI)
- 3-stage Dockerfile (Node→Go→Alpine)

---

## [0.1.0] — 2026-03-15

### v0.1 "It Works"

Working proxy with auth, org structure, usage tracking. No UI.

#### Core
- OpenAI-compatible passthrough proxy (/v1/*)
- Streaming / SSE support
- Provider adapters: Anthropic, Azure, vLLM, OpenAI, Ollama
- Auth: Bearer token, HMAC-SHA256 hashing, key types (user, team, SA)
- RBAC: system_admin > org_admin > team_admin > member
- Organizations, Teams, Users, Service Accounts CRUD
- API keys CRUD with create-once plaintext
- Token limits (daily/monthly) + rate limits (rpm/rpd)
- Async usage logging with token counting
- Health endpoints, Prometheus runtime metrics
- Structured JSON logging (slog)
- Docker + docker-compose
