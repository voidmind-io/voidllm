# Contributing to VoidLLM

Thank you for your interest in contributing to VoidLLM! This guide will help you get started.

## Getting Started

### Prerequisites

- Go 1.23+
- Node.js 20+ (for UI development)
- Docker (optional, for Postgres/Redis)

### Setup

```bash
git clone https://github.com/voidmind-io/voidllm
cd voidllm

# Copy and edit config
cp voidllm.yaml.example voidllm.yaml

# Generate required keys
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)

# Run
go run ./cmd/voidllm --config voidllm.yaml

# Run tests
go test -race ./...
```

### Hot Reload (optional)

```bash
go install github.com/air-verse/air@latest
air
```

## Development Workflow

1. **Fork** the repository
2. **Create a branch** from `main`: `git checkout -b feat/my-feature`
3. **Write code** — follow the conventions below
4. **Write tests** — table-driven, parallel, real SQLite (no mocks)
5. **Run checks**: `go test -race ./... && go vet ./...`
6. **Commit** with a descriptive message (see Commit Messages below)
7. **Open a Pull Request** against `main`

## Code Conventions

### Go

- **No `// TODO` comments** — if it's not implemented, it's not in the code
- **No stubs or placeholder functions** — every function does what its signature promises
- **No `panic()`** — return errors
- **No `fmt.Println`** — use `slog` for logging
- **No `any` / `interface{}`** without explicit justification
- **Every exported symbol** has a godoc comment
- **Every error path** is handled — no `_ = someFunc()` on fallible operations
- **Error strings**: lowercase, no trailing punctuation
- **Error wrapping**: `fmt.Errorf("operation context: %w", err)`
- **Context**: `context.Context` is always the first parameter
- **Tests**: table-driven with `t.Parallel()`, real SQLite in-memory DB (no mocks for storage)

### SQL

- Parameterized queries only — **never** concatenate user input into SQL
- Use `dialect.Placeholder(n)` for cross-database compatibility
- `defer rows.Close()` after every `QueryContext`
- Check `rows.Err()` after every scan loop

### Security

- **API keys**: HMAC-SHA256 hashed, never stored in plaintext
- **Upstream API keys**: AES-256-GCM encrypted in the database
- **Headers**: allowlist approach (not blocklist) for forwarding
- **Secrets**: never in logs — use `slog.LogValuer` for redaction
- **Privacy**: no prompt or response content is ever stored or logged

## Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add team membership CRUD
fix: close cross-org IDOR in membership deletion
refactor: extract requireOrgAccess helper
docs: add deployment constraints to README
test: add concurrent CAS correctness test for rate limiter
```

## Project Structure

```
cmd/voidllm/          — Binary entrypoint (wiring only, no business logic)
internal/
  config/             — YAML config loading, validation
  logger/             — Structured logging setup
  db/                 — Database layer (SQLite, migrations, CRUD)
  cache/              — Generic in-memory cache (sync.Map)
  auth/               — Auth middleware, RBAC, bootstrap, key cache loader
  proxy/              — LLM proxy handler, model registry, provider adapters
  usage/              — Async usage event logger
  ratelimit/          — Rate limiting + token budget enforcement
  api/
    admin/            — Admin API handlers (CRUD for all entities)
    health/           — Health, readiness, and metrics endpoints
pkg/
  crypto/             — AES-256-GCM encryption utilities
  keygen/             — API key generation + HMAC hashing
enterprise/           — Proprietary (see enterprise/LICENSE.md)
ui/                   — Frontend SPA (coming in v0.2)
```

## What NOT to Contribute

- Load balancing / fallback between providers (premium feature)
- Per-model rate limits (premium feature)
- Prompt caching or storage
- Any code that stores or logs prompt/response content

## Questions?

Open an issue on [GitHub](https://github.com/voidmind-io/voidllm/issues).
