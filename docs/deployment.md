# Deployment Guide

## Docker

### Minimal Setup

```bash
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)

docker run -d --name voidllm \
  -p 8080:8080 \
  -e VOIDLLM_ADMIN_KEY -e VOIDLLM_ENCRYPTION_KEY \
  -v $(pwd)/voidllm.yaml:/etc/voidllm/voidllm.yaml:ro \
  -v voidllm_data:/data \
  voidllm:latest
```

### Docker Compose

```bash
cp voidllm.yaml.example voidllm.yaml
# Edit voidllm.yaml — configure your models

export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
docker-compose up -d
```

## Kubernetes (Helm)

### Basic Installation

```bash
helm install voidllm chart/voidllm/ \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32)
```

### With PostgreSQL

```bash
helm install voidllm chart/voidllm/ \
  --set postgresql.enabled=true \
  --set postgresql.auth.password=mysecretpassword \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32)
```

### With Redis (Enterprise, Multi-Instance)

```bash
helm install voidllm chart/voidllm/ \
  --set postgresql.enabled=true \
  --set redis.enabled=true \
  --set replicaCount=3 \
  --set secrets.license="eyJ..." \
  --set secrets.adminKey=... \
  --set secrets.encryptionKey=...
```

Redis enables distributed rate limiting and instant cache invalidation.
Requires an Enterprise license. Without Redis, run only one replica.

### Enterprise Features

```bash
helm install voidllm chart/voidllm/ \
  --set secrets.license="eyJ..." \
  --set config.settings.audit.enabled=true \
  --set config.settings.otel.enabled=true \
  --set config.settings.otel.endpoint=tempo:4317 \
  --set config.settings.sso.enabled=true \
  --set config.settings.sso.issuer=https://accounts.google.com \
  --set config.settings.sso.clientId=xxx \
  --set secrets.ssoClientSecret=yyy \
  --set config.settings.sso.redirectUrl=https://voidllm.company.com/api/v1/auth/oidc/callback
```

## Database

### SQLite (Default)

Zero configuration. Data is stored in a single file (`/data/voidllm.db` by default).
Suitable for single-instance deployments up to ~1000 requests/second.

### PostgreSQL

For production deployments with high write throughput or multi-instance setups:

```yaml
database:
  driver: postgres
  dsn: postgres://voidllm:${DB_PASSWORD}@postgres:5432/voidllm?sslmode=require
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 5m
```

### Migration (SQLite → PostgreSQL)

```bash
voidllm migrate \
  --from sqlite:///data/voidllm.db \
  --to postgres://user:pass@host:5432/voidllm
```

Bidirectional, transactional, batched. Safe to run against production data.

## User Onboarding

There are three ways to add users to VoidLLM:

### Invite Link (recommended)

1. Go to **Organization → Users** in the UI
2. Click **Invite User** — enter email and select a role
3. Copy the invite link and share it (Slack, email, etc.)
4. The recipient opens the link, sets their display name and password
5. They're added to the org with the assigned role

Invite links expire after 7 days. Admins can cancel pending invites.

Email-based invite delivery (SMTP) is planned for a future release.

### Manual Creation (system admin)

System admins can create users directly via **System → Users → Create User**
with email, display name, and password.

### SSO Auto-Provisioning (Enterprise)

With SSO enabled, users from allowed email domains are created automatically
on first login. See the [Enterprise Guide](enterprise.md#auto-provisioning).

---

## Health Checks

| Endpoint | Purpose | Expected |
|---|---|---|
| `GET /healthz` | Liveness | 200 always |
| `GET /readyz` | Readiness | 200 (503 during graceful drain) |
| `GET /metrics` | Prometheus | Prometheus text format |

### Kubernetes Probes

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  initialDelaySeconds: 5

readinessProbe:
  httpGet:
    path: /readyz
    port: 8080
  initialDelaySeconds: 5
```

## Graceful Shutdown

VoidLLM supports phased graceful shutdown for zero-downtime deployments:

1. **SIGTERM received** → `/readyz` returns 503 (K8s stops routing new traffic)
2. **Drain period** (configurable, default 25s) → in-flight requests complete
3. **Force cancel** → remaining requests aborted if drain times out
4. **Cleanup** → flush usage/audit buffers, close DB, stop background tasks

Configure the drain timeout:

```yaml
server:
  proxy:
    drain_timeout: 25s    # Must be less than K8s terminationGracePeriodSeconds
```

## Reverse Proxy

VoidLLM works behind any reverse proxy (Nginx, Traefik, Caddy, K8s Ingress):

```nginx
# Nginx example
location /v1/ {
    proxy_pass http://voidllm:8080;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;              # Required for SSE streaming
}
```

For streaming (SSE), ensure your reverse proxy does not buffer responses.

## Security Hardening

The Helm chart includes production security defaults:

- Non-root container user
- Read-only root filesystem
- All Linux capabilities dropped
- No privilege escalation
- Resource limits configured

For additional hardening, consider:
- Network policies to restrict pod-to-pod traffic
- TLS termination at the ingress level
- Separate admin port (`server.admin.port: 8443`) to isolate admin traffic
