# Configuration Reference

VoidLLM is configured via a YAML file. By default it looks for `voidllm.yaml` in the current directory, or pass `--config /path/to/config.yaml`.

Environment variables are interpolated with `${VAR}` syntax. Use this for secrets:

```yaml
settings:
  admin_key: ${VOIDLLM_ADMIN_KEY}
  encryption_key: ${VOIDLLM_ENCRYPTION_KEY}
```

## Server

```yaml
server:
  proxy:
    port: 8080              # Proxy port — LLM clients connect here
    read_timeout: 30s
    write_timeout: 120s     # High for streaming responses
    idle_timeout: 60s
    drain_timeout: 25s      # Graceful shutdown drain window (5s–120s)

  # Optional: separate admin port for UI + Admin API
  admin:
    port: 0                 # 0 = everything on proxy port (default)
    tls:
      enabled: false
      cert: /certs/tls.crt
      key: /certs/tls.key
```

## Database

```yaml
database:
  driver: sqlite            # sqlite (default) or postgres
  dsn: /data/voidllm.db    # SQLite file path or PostgreSQL DSN

  # PostgreSQL example:
  # driver: postgres
  # dsn: postgres://user:${DB_PASSWORD}@host:5432/voidllm?sslmode=require

  # Connection pool (PostgreSQL only)
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 5m
```

## Models

```yaml
models:
  - name: dolphin-mistral         # Unique model name
    provider: ollama              # openai, anthropic, azure, vllm, ollama, custom
    base_url: http://localhost:11434/v1
    api_key: ${OLLAMA_KEY}        # Optional, depends on provider
    timeout: 30s                  # Per-model upstream timeout (default: 5min)
    aliases:                      # Alternative names clients can use
      - dolphin
      - default
    max_context_tokens: 32000     # Informational, shown in UI
    pricing:
      input_per_1m: 0.15         # Cost per 1M input tokens (for usage tracking)
      output_per_1m: 0.60

  # Azure requires deployment name + API version
  - name: gpt-4o
    provider: azure
    base_url: https://mycompany.openai.azure.com
    api_key: ${AZURE_KEY}
    azure_deployment: gpt-4o
    azure_api_version: "2024-10-21"
```

Models can also be created via the Admin API and stored in the database.
DB models take precedence over YAML models on name collision.

## Settings

```yaml
settings:
  # Required: bootstrap admin key (≥32 chars)
  admin_key: ${VOIDLLM_ADMIN_KEY}

  # Required: AES-256-GCM encryption key (base64, 32 bytes)
  # Generate: openssl rand -base64 32
  encryption_key: ${VOIDLLM_ENCRYPTION_KEY}

  # Enterprise license key
  license: ${VOIDLLM_LICENSE}
  # Or as a file path:
  # license_file: /etc/voidllm/license.jwt

  # First-run bootstrap
  bootstrap:
    org_name: "My Company"
    org_slug: "my-company"          # Auto-derived from name if empty
    admin_email: "admin@company.com"

  # Usage logging
  usage:
    buffer_size: 1000               # Events buffered before flush
    flush_interval: 5s
    drop_on_full: true              # Drop events when buffer full (never blocks proxy)

  # Token counting
  token_counting:
    enabled: true

  # Soft limit warning threshold
  soft_limit_threshold: 0.9        # Warn at 90% of limit
```

## Cache

```yaml
cache:
  key_ttl: 30s              # How often to refresh the key cache from DB
  model_ttl: 60s            # Model access cache refresh
  alias_ttl: 60s            # Alias cache refresh
```

## Circuit Breaker

Per-model circuit breaker for upstream provider errors. When a model's upstream
returns consecutive failures, the circuit opens and requests are rejected
immediately — preventing cascading failures.

```yaml
settings:
  circuit_breaker:
    enabled: false
    threshold: 5             # Consecutive failures before circuit opens
    timeout: 30s             # How long circuit stays open
    half_open_max: 1         # Probe requests in half-open state
```

---

## Enterprise Features

The following features require a license key. See the
[Enterprise Guide](enterprise.md) for setup instructions.

### Redis (Enterprise)

Required for multi-instance deployments. Enables distributed rate limiting
and instant cache invalidation across instances. Single-instance deployments
don't need Redis.

```yaml
redis:
  enabled: false
  url: redis://:${REDIS_PASSWORD}@redis:6379/0
  key_prefix: voidllm:
```

### Audit Logging (Enterprise)

Requires a license with the `audit_logs` feature.

```yaml
settings:
  audit:
    enabled: true
    buffer_size: 500
    flush_interval: 5s
```

### OpenTelemetry (Enterprise)

Requires a license with the `otel_tracing` feature.

```yaml
settings:
  otel:
    enabled: true
    endpoint: "tempo:4317"   # OTLP gRPC endpoint
    insecure: true           # Set false for TLS
    sample_rate: 1.0         # 0.0 = no traces, 1.0 = all traces
```

### SSO / OIDC (Enterprise)

Requires a license with the `sso_oidc` feature. Global config deployed via YAML,
per-org config managed via the UI.

```yaml
settings:
  sso:
    enabled: true
    issuer: "https://accounts.google.com"
    client_id: ${VOIDLLM_SSO_CLIENT_ID}
    client_secret: ${VOIDLLM_SSO_CLIENT_SECRET}
    redirect_url: "https://voidllm.company.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "email", "profile"]
    allowed_domains: ["company.com"]
    default_role: "member"
    auto_provision: true
    group_sync: false
    group_claim: "groups"
```

Works with any OIDC provider: Google, Azure AD, Okta, Auth0, Keycloak, OneLogin, etc.
