# Changelog

All notable changes to VoidLLM are documented in this file.

## [0.0.12] - 2026-04-03

### Fixes
- Usage dashboard: handle NULL team_id/key_id/user_id in aggregation queries (#51)
- License set via UI now persists to database across restarts
- License startup log shows source (database, config, or none)
- Heartbeat User-Agent includes VoidLLM version
- Updated embedded license public key

### Documentation
- README feature list as two-column table, removed em dashes
- Corrected GDPR compliance language

---

## [0.0.11] - 2026-04-02

### Documentation
- Restructured docs into 24 files with subdirectories (deployment/, models/, mcp/, security/, enterprise/, api/)
- Added getting-started guide, troubleshooting, and docs index
- All doc files include Astro frontmatter for website rendering
- Docs now live at [voidllm.ai/docs](https://voidllm.ai/docs)

### Helm Chart
- Fixed Artifact Hub indexing (removed empty signKey annotation)

### CI
- Pinned all GitHub Actions to commit hashes
- Added Cosign image signing and SLSA provenance
- Removed unused @astrojs/tailwind dependency conflict

---

## [0.0.10] - 2026-04-01

### Helm Chart
- Published to [Artifact Hub](https://artifacthub.io/packages/helm/voidllm/voidllm)
- Chart README with quick start and configuration examples
- Added icon, keywords, license annotation, documentation links

### Documentation
- Bootstrap credentials clarified in README Quick Start
- Artifact Hub badge added to README
- New pricing: Pro $49/mo, Enterprise $149/mo, Founding Member $999 lifetime

### Fixes
- OTel service version now uses build-time version instead of hardcoded value

---

## [0.0.9] - 2026-03-30

### Docker, Helm & Configuration

- **Fixed image registry** — Docker Compose now uses `ghcr.io/voidmind-io/voidllm`
- **Helm chart updated** — correct registry, MCP, Code Mode, and health check settings in values + configmap
- **Istio support** — optional Gateway + VirtualService templates (`istio.enabled: true`)
- **MCP servers in Helm** — static MCP server definitions via `config.mcpServers`
- **Example config expanded** — MCP, Code Mode, logging, health check, and enterprise sections

---

## [0.0.8] — 2026-03-30

### Performance

- **sonic JSON engine** — faster JSON serialization across all hot paths
- **In-memory caches** — MCP server lookups, access checks, and transport pooling moved out of the DB hot path
- **MCP Proxy overhead reduced 36%** — 670µs → 427µs P50 at 1000 RPS

### MCP Access Management

- **Closed-by-default for global servers** — organizations must explicitly grant access to global MCP servers (org-scoped and team-scoped servers are unaffected)
- **MCP Access API** — `GET/PUT /orgs/:org_id/mcp-access` (and team/key variants) for managing server allowlists
- **Org MCP Access tab** — new tab in Organization settings to toggle global server access
- **Team MCP Access tab** — restrict MCP access within org allowlist per team

### Benchmark

- **Go benchmark CLI** — 6 scenarios (quick, sustained, burst, large-payload, mixed, endurance) using Vegeta load testing library
- **Benchmark results** — LLM Proxy 442µs P50, MCP Proxy 427µs P50, Code Mode 3.35ms pure JS / 32µs warm eval

### Breaking Changes

- MCP access at org level is now closed-by-default. Existing installations with global MCP servers must grant org-level access via the new UI or API after upgrading.
- ToolCache is keyed by server ID instead of alias.

---

## [0.0.7] — 2026-03-29

### Code Mode

- **WASM-sandboxed JS execution** — LLMs write JavaScript to orchestrate multiple MCP tool calls in one execution (QuickJS via Wazero, pure Go, no CGO)
- **3 Code Mode tools** — `list_servers`, `search_tools`, `execute_code` on dedicated `/api/v1/mcp` endpoint
- **ES6 Proxy dispatch** — dynamic tool routing, any tool name characters supported
- **TypeScript type declarations** — auto-generated from tool schemas, injected at `tools/list` time
- **Console capture** — `console.log/warn/error` output returned in execution results
- **Per-tool blocklist** — admins block specific tools from Code Mode via API and UI
- **Persistent tool cache** — tool schemas stored in DB, zero HTTP calls on startup, 24h background refresh
- **Execution history** — UUIDv7 per execution groups tool calls for tracing
- **SSE transport detection** — deprecated SSE servers auto-detected and deactivated
- **MCP server split** — Code Mode at `/api/v1/mcp`, management tools at `/api/v1/mcp/voidllm`
- **Tools list UI** — expanded row shows all tools per server with block/unblock buttons
- **Code Mode toggle** — per-server enable/disable in UI and API
- **Refresh tools endpoint** — force re-fetch tool schemas with 60s cooldown

---

## [0.0.6] — 2026-03-28

### MCP Gateway

- **Built-in MCP server** — 6 management tools (list_models, get_model_health, get_usage, list_keys, create_key, list_deployments)
- **MCP Gateway proxy** — register external MCP servers, proxy tool calls with auth and access control
- **Scoped access control** — global, org, and team-level MCP server registration
- **MCP access tables** — org/team/key allowlists (most-restrictive-wins)
- **Session management** — auto-initialize, Mcp-Session-Id forwarding, session re-init on expiry
- **YAML config sync** — MCP servers from `voidllm.yaml` synced to DB at startup
- **Async tool call logging** — fire-and-forget batch writes to `mcp_tool_calls`
- **MCP Servers UI** — CRUD, scope selector, auth type tabs, test connection, source badges
- **Prometheus metrics** — tool call counts, duration, transport errors

---

## [0.0.5] — 2026-03-26

### Multi-Deployment Load Balancing

- **Load balancing** — multi-deployment models with round-robin, least-latency, weighted, and priority routing strategies
- **Automatic failover** — retry on 5xx/timeout/connection error, per-deployment circuit breakers
- **Health-aware routing** — unhealthy deployments skipped, all-unhealthy fallback
- **Deployment CRUD** — Admin API + UI for managing deployments per model
- **Create Model dialog** — mode switch (Single Endpoint / Load Balanced)
- **Expandable deployment rows** — Models page shows per-deployment health, provider, base URL
- **Table component** — generic `renderExpandedRow` support
- **ARM64 Docker images** — multi-arch builds (linux/amd64 + linux/arm64)
- **Cross-compile Dockerfile** — builds in ~2.5 min instead of ~20 min
- **Flexible encryption key** — accepts base64 or any string >= 16 characters (SHA-256 derived)
- **Default config fallback** — start with just `VOIDLLM_ENCRYPTION_KEY` env var, no config file needed
- **Bootstrap log ordering** — credentials shown after server start, cleared from memory after print
- **Codecov integration** — coverage reporting in CI with badge
- **Admin API tests** — models, invites, model aliases, model access (3700+ lines)
- **Router tests** — 23 tests, 98.9% coverage
- **Deployment tests** — 11 DB + 14 API tests with IDOR checks

---

## [0.0.4] — 2026-03-24

### Model Types & Health Monitoring

- **Model types** — `model_type` field across full stack (chat, embedding, reranking, completion, image, audio, tts)
- **Playground tabs** — type-based tabs (Chat / Embedding / Completion), shown dynamically
- **Embedding interface** — text → vector display + cosine similarity comparison
- **Type badge** — color-coded type indicator on Models page
- **Type selector** — in Create and Edit Model dialogs
- **Health checker** — type-aware functional probe (skips non-chat types)
- **Upstream health monitoring** — 3-level probes (health, models, functional) with configurable intervals
- **Dashboard health section** — healthy/degraded/unhealthy model counts
- **Model performance table** — latency + throughput per model
- **Recharts integration** — AreaChart, DonutChart, HorizontalBar, MiniTable
- **Glassmorphism dialogs** — backdrop-blur, semi-transparent, purple border-top
- **Segmented pill tabs** — replaced underline tab styling
- **README badges** — Go Report Card, Release version, Go version

---

## [0.0.3] — 2026-03-23

### UI Redesign

- **Complete UI redesign** — premium dark theme across all pages
- **Dashboard** — stat cards with icons, usage charts, model performance, budget warnings
- **Playground** — split panel layout, advanced parameters, code blocks
- **Keys page** — stat cards, icon actions, key counts
- **GitHub Actions** — CI (Go + UI), Release (Docker to GHCR), CodeQL, OpenSSF Scorecard

---

## [0.0.2] — 2026-03-23

### Distributed Rate Limiting

- **Redis rate limiting** — Lua scripting for distributed rate limit enforcement
- **Checker interface** — pluggable rate limit backends (in-memory + Redis)

---

## [0.0.1] — 2026-03-23

### Initial Release

First tagged release. Includes all features developed during the pre-release phase:

#### Proxy
- OpenAI-compatible passthrough proxy (`/v1/*`)
- Streaming / SSE support with per-chunk usage extraction
- Provider adapters: Anthropic (full translation), Azure (URL mapping), vLLM, OpenAI, Ollama, custom
- Header sanitization, hop-by-hop stripping

#### Access Control
- Bearer token auth with HMAC-SHA256 hashing (O(1) lookup)
- Key types: user (`vl_uk_`), team (`vl_tk_`), service account (`vl_sa_`)
- RBAC: system_admin > org_admin > team_admin > member
- Org → Team → User → Key hierarchy
- Model access control (explicit-allow, most-restrictive-wins)
- Model aliases (org/team scoped)

#### Usage & Limits
- Async usage logging (buffered channel → batch DB write)
- Token counting from upstream responses (streaming included)
- Rate limits (requests per minute/day) at org, team, key level
- Token budgets (daily/monthly) with real-time enforcement
- Cost estimation per request (configurable per-model pricing)
- TTFT + TPS metrics per request

#### Web UI
- Dashboard, Playground, Keys, Teams, Users, Service Accounts, Models, Usage, Settings, License pages
- Login + session auth, invite token system, role-aware sidebar
- Cost reports, usage export (CSV/JSON)

#### Enterprise
- Audit logs — async middleware, filterable API + UI
- SSO / OIDC — Google, Azure AD, Okta, Keycloak, auto-provisioning, group sync
- License verification — Ed25519 JWT, offline-verifiable, feature gating
- OpenTelemetry tracing — OTLP/gRPC export

#### Infrastructure
- Graceful shutdown with phased drain
- Per-model timeouts and circuit breakers
- 14 Prometheus metrics
- Request ID correlation (UUIDv7)
- SQLite (default) + PostgreSQL support
- Redis (optional) for distributed rate limiting
- Bidirectional database migration tool
- Helm chart with PG/Redis subcharts
- 3-stage Dockerfile (Node → Go → Alpine)
- Key rotation with 24h grace period
