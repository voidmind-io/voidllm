# Changelog

All notable changes to VoidLLM are documented in this file.

## [0.0.22] - 2026-07-21

### Features
- PII anonymization (opt-in, early/beta): structured PII (IBAN, email, phone, credit card, tax ID) in outbound prompts is replaced with deterministic per-organization pseudonyms before the request leaves to an external provider, and restored in the response, including token-by-token while streaming (message content and tool-call arguments). No prompt or response content, PII, or pseudonyms are ever persisted or logged. Structured detection is regex-based; unstructured PII (names, places, operator terms) is covered by the opt-in gazetteer below; transformer-based NER is planned. Off by default (#134, #135, #137)
- Gazetteer-based detection (opt-in): an in-process, language-neutral matcher catches unstructured PII the regex misses - names, places, company forms, and operator-supplied terms (customer/project/employee names). Curated German first-name and city packs are bundled; operators can load their own term lists for any language. No model/NER required. Off by default (#143, #145)
- Native tool-call support for the Anthropic and Gemini providers: OpenAI-style tool calls are translated to and from the Anthropic Messages and Gemini generateContent APIs across requests, responses, and streaming, including alongside the PII firewall (#138, #139)

### Fixes
- The PII firewall no longer rejects messages with `null` content (e.g. assistant messages carrying only `tool_calls`), so multi-turn tool conversations work with anonymization enabled (#136)
- Streaming tool calls are delivered more reliably: provider adapters can emit multiple events per upstream chunk and fail closed on a malformed tool stream instead of silently dropping a tool call (#144)
- The dashboard Playground works in dual-port mode. It previously issued a same-origin request to the proxy API, which is not served on the admin port, so every request failed with 405 - in the topology the hardening guide recommends. Requests now go through an authenticated same-origin route that hands off to the proxy internally, so nothing needs configuring and access control, limits and anonymization apply unchanged (#153)
- Streaming responses that end without their terminal are no longer recorded as successful. The proxy now tracks how a stream ended and reports a truncated one as incomplete, while a client that simply hangs up is no longer counted as an upstream failure. Clients also receive an explicit error event when a stream is cut short instead of the connection merely stopping (#153)
- The Playground error banner shows the server's message for non-JSON errors instead of a bare status code, which was empty over HTTP/2 (#154)

### Documentation
- PII anonymization is now documented, including what it does *not* protect: it removes the link between a statement and a named person, but the statement itself still reaches the provider (#155)

---

## [0.0.21] - 2026-06-19

### Features
- Usage analytics (LLM and MCP) and the audit log now resolve user, key, team, organization, and service-account IDs to human-readable names instead of showing raw UUIDs. Audit actor names are resolved org-scoped so cross-org identities are never exposed (#125)
- The in-app "update available" dialog now renders release notes as formatted markdown instead of raw text (#132)

### Fixes
- Documentation: the README and pricing pages now show prices in EUR (matching billing) and list usage export as a Community feature, and the README gained an architecture overview, a "who it's for" section, and a production checklist (#131)

### Security
- Rebuilt the published container image on patched Alpine (3.21.7) and Go (1.26.4) to clear base-image and toolchain advisories. No application code changed (#126)

### Dependencies
- Go toolchain 1.26.1 → 1.26.4 and Alpine base image 3.21.6 → 3.21.7 (#126)

---

## [0.0.20] - 2026-06-16

### Features
- Brute-force protection on the login endpoint: per-IP rate limiting plus per-account lockout after repeated failed attempts. In-memory, per-process (single-instance; distributed throttling will follow with Redis) (#104)

### Fixes
- Rate limiter now enforces each scope against its own limit. Previously the most-restrictive limit across key/team/org was applied to every counter, so an org or team was incorrectly capped at the smallest key limit and requests were rejected with 429 too early (#102)
- Users created via the admin UI now receive an organization membership and can sign in. User creation now requires an organization and creates the user and membership atomically; a guard turns the previous login crash for org-less users into a clean error (#100, #105)
- Profile password change now actually changes the password and verifies the current password first. The previous flow silently dropped the new password and reported success without changing anything (#99, #106)

### Security
- Audit log descriptions now redact sensitive fields (passwords, API keys, auth tokens, OAuth/client secrets) instead of persisting request bodies verbatim. A migration clears historical `audit_logs` rows that may contain such values. Operators upgrading from an earlier version should rotate any secrets that were entered through the admin API before this release (#103)
- Closed upstream dependency advisories in `react-router` (#111) and `vite` (#112)

### Dependencies
- github.com/valyala/fasthttp 1.70.0 → 1.71.0 (#111)
- react-router-dom 7.14.2 → 7.17.0 (#111)
- vite 7.3.2 → 7.3.5 (#112)
- github.com/redis/go-redis/v9 9.18.0 → 9.19.0 (#80)
- google.golang.org/grpc 1.80.0 → 1.81.0 (#82)
- github.com/bytedance/sonic 1.15.0 → 1.15.1 (#84)
- modernc.org/sqlite 1.50.0 → 1.50.1 (#88)
- js-yaml 4.1.1 → 4.2.0 (#109)
- tailwindcss 4.2.2 → 4.2.4 (#81)
- typescript-eslint 8.58.0 → 8.59.2 (#85)
- CI actions: docker/setup-buildx-action 4.1.0 (#97), docker/login-action 4.2.0 (#96), docker/build-push-action 7.2.0 (#95), codecov/codecov-action 6.0.1 (#93), sigstore/cosign-installer 4.1.2 (#89)

---

## [0.0.19] - 2026-05-14

### Fixes
- Admin TLS configuration (`server.admin.tls`) is now actually applied in dual-port mode. Previously `tls.enabled: true` was a silent no-op - the schema and validation existed but no listener consumed the cert/key. In single-port mode (admin sharing the proxy port) configuring TLS now emits a WARN since external termination is expected there. Thanks to @martinsotirov for the fix (#92)

---

## [0.0.18] - 2026-05-13

### Fixes
- Startup panic when Code Mode is disabled - default config caused a nil pointer dereference of `SchemaTTL` in `app.go`. Thanks to @kernelb00t for the report (#87) and @SAY-5 for the fix (#90)

---

## [0.0.17] - 2026-04-30

### Features
- Code Mode response unwrapping — MCP ToolResult wrapper is stripped before results reach JS, so scripts work with plain values not protocol envelopes (#73)
- Code Mode output schema inference — return types are learned from the first successful tool call and persisted, surfacing as TypeScript via `search_tools` (#73)
- Code Mode tool descriptions rewritten with STRONG PREFERENCE / WORKFLOW / PATTERNS guidance to push LLMs toward chained calls and reduce sequential round-trips (#73)

### Security
- pgx 5.9.1 → 5.9.2 — SQL injection via dollar-quoted placeholder (low) (#75)
- vite 7.3.1 → 7.3.2 — arbitrary file read via dev-server WebSocket (high), path traversal in optimized-deps `.map` (medium), `server.fs.deny` bypass with queries (high) (#53)
- fiber 3.1.0 → 3.2.0 — cache middleware default key generator ignores query string (medium) (#77)
- postcss 8.5.x → 8.5.12 — XSS via unescaped `</style>` (medium) (#78)

### Dependencies
- go-oidc 3.17.0 → 3.18.0 (#67)
- fasthttp 1.69.0 → 1.70.0 (#70)
- golang.org/x/crypto 0.49.0 → 0.50.0 (#71)
- modernc.org/sqlite 1.48.0 → 1.50.0 (#76)
- react-router-dom 7.13.2 → 7.14.2 (#68)
- docker/build-push-action 7.0.0 → 7.1.0 (#66)
- actions/setup-node 6.3.0 → 6.4.0 (#74)

---

## [0.0.16] - 2026-04-12

### Features
- Model fallback chains - cross-model failover when all deployments of the primary are unavailable (Enterprise) (#45)
  - Configurable chain depth via `settings.fallback_max_depth`
  - Per-hop access control enforcement
  - Cycle detection at config, API, and runtime
  - Usage events track both requested and served model name
  - UI: Fallback Model dropdown in model create and edit dialogs
  - UI: depth-0 warning when fallback is configured but disabled

### Fixes
- Flaky MCP usage day-grouping test near midnight UTC

---

## [0.0.15] - 2026-04-07

### Features
- Configurable data retention for usage events and audit logs (#46)
  - Opt-in background cleanup job with per-table retention durations
  - Dialect-aware SQL predicate for correct SQLite and PostgreSQL behavior
  - Batched deletes with single-column timestamp indexes for efficient cleanup on large tables
- Admin UI update notification via GitHub release check
- PostgreSQL migration locking via advisory lock prevents concurrent-migration races (#48)

### Improvements
- Batch dependency updates: grpc 1.80.0, OpenTelemetry 1.43.0, go-jose 4.1.4, vitest 4.1.2 (#63)
- GitHub Actions workflow bumps: cosign-installer 4.1.1, docker/login-action 4.1.0, setup-node 6.3.0

### Fixes
- ESLint setState-in-useEffect violation in update notification component

---

## [0.0.14] - 2026-04-05

### Features
- MCP OAuth Client Credentials auth type with auto-discovery (#49)
- Google Gemini and Vertex AI provider adapter (8 providers total)
- MCP usage dashboard with tabbed layout - Overview, LLM, MCP (#44)
- Binary deployment documentation for Linux, macOS, Windows

### Improvements
- MCP usage queries with chronological ordering for time-based grouping
- Cross-org data handling in usage dashboard
- Shared credentials warning banner in MCP server dialogs
- Windows binary pauses on error exit to show error message
- 42 new tests for MCP usage queries, handlers, and health checker

---

## [0.0.13] - 2026-04-04

### Features
- MCP server health indicators in UI with auto-refresh (#43)
- Standalone binary support for Windows, Linux, macOS (#50)
- Cross-platform binaries in GitHub Release pipeline
- License instance identification via heartbeat
- Bench metrics sampler with realistic streaming scenario

### Improvements
- Comprehensive logging review: audit coverage for MCP, SSO, license, settings
- Key cache log noise reduced (INFO to DEBUG)
- Rate limit and token budget violations now logged
- Migration execution logged at INFO
- Failed login attempts audited
- Default DB path: ./voidllm.db for standalone, /data/voidllm.db for Docker
- SSRF-safe dialer for MCP health probes
- Heartbeat dedup via timestamp (replaces lock mechanism)
- Bounded concurrency for MCP health probes
- Stale MCP health entries cleaned up automatically

---

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
