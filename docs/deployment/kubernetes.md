---
title: "Kubernetes Deployment"
description: "Deploy VoidLLM with Helm, Istio, and health probes"
section: deployment
order: 2
---
# Kubernetes Deployment (Helm)

## Basic Installation

```bash
helm repo add voidllm https://voidmind-io.github.io/voidllm
helm repo update

helm install voidllm voidllm/voidllm \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32)
```

Check bootstrap credentials in the pod logs:

```bash
kubectl logs deploy/voidllm | grep "BOOTSTRAP"
```

## With PostgreSQL

The Helm chart includes a Bitnami PostgreSQL subchart. When enabled, VoidLLM automatically switches from SQLite to PostgreSQL - no manual config needed.

```bash
helm install voidllm voidllm/voidllm \
  --set postgresql.enabled=true \
  --set postgresql.auth.password=$(openssl rand -base64 16) \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32)
```

The password must be set explicitly - VoidLLM and the PostgreSQL subchart share this value to authenticate. Default username is `voidllm`, default database is `voidllm`.

Pod-to-pod traffic within the cluster is unencrypted (`sslmode=disable`). If you need encrypted database connections, use an external PostgreSQL with a custom DSN:

```bash
helm install voidllm voidllm/voidllm \
  --set config.database.driver=postgres \
  --set config.database.dsn="postgres://user:pass@external-db:5432/voidllm?sslmode=require"
```

## With Redis (Multi-Instance)

Redis enables distributed rate limiting and instant cache invalidation. Requires an Enterprise license. Without Redis, run only one replica.

```bash
helm install voidllm voidllm/voidllm \
  --set postgresql.enabled=true \
  --set postgresql.auth.password=$(openssl rand -base64 16) \
  --set redis.enabled=true \
  --set replicaCount=3 \
  --set secrets.license="eyJ..." \
  --set secrets.adminKey=$(openssl rand -base64 32) \
  --set secrets.encryptionKey=$(openssl rand -base64 32)
```

Multi-instance requires both PostgreSQL (shared state) and Redis (distributed rate limiting + cache invalidation).

**Note:** Schema migrations currently run on every pod startup. With multiple replicas, pods may briefly race during rolling updates. PostgreSQL's transaction isolation prevents corruption, but you may see transient errors in logs. A dedicated migration hook is planned ([#48](https://github.com/voidmind-io/voidllm/issues/48)).

## Enterprise Features

Enterprise features are disabled by default and must be explicitly enabled. Add these to your existing `helm install` or `helm upgrade`:

**Audit Logging:**
```bash
--set secrets.license="eyJ..." \
--set config.settings.audit.enabled=true
```

**OpenTelemetry Tracing:**
```bash
--set config.settings.otel.enabled=true \
--set config.settings.otel.endpoint=tempo:4317
```

**SSO / OIDC:**
```bash
--set config.settings.sso.enabled=true \
--set config.settings.sso.issuer=https://accounts.google.com \
--set config.settings.sso.clientId=xxx \
--set secrets.ssoClientSecret=yyy \
--set config.settings.sso.redirectUrl=https://voidllm.company.com/api/v1/auth/oidc/callback
```

All three require a license key (`secrets.license`). See the [Enterprise docs](../enterprise/license.md) for activation.

See the full [values.yaml](https://github.com/voidmind-io/voidllm/blob/main/chart/voidllm/values.yaml) for all Helm configuration options.

## Istio Support

```yaml
istio:
  enabled: true
  virtualService:
    hosts:
      - voidllm.example.com
  gateway:
    servers:
      - port:
          number: 443
          name: https
          protocol: HTTPS
        tls:
          mode: SIMPLE
          credentialName: voidllm-tls
        hosts:
          - voidllm.example.com
```

## Health Probes

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

| Endpoint | Purpose | Expected |
|---|---|---|
| `GET /healthz` | Liveness | 200 always |
| `GET /readyz` | Readiness | 200 (503 during graceful drain) |
| `GET /metrics` | Prometheus | Prometheus text format |

## Graceful Shutdown

VoidLLM supports phased graceful shutdown for zero-downtime deployments:

1. **SIGTERM received** - `/readyz` returns 503 (K8s stops routing new traffic)
2. **Drain period** (configurable, default 25s) - in-flight requests complete
3. **Force cancel** - remaining requests aborted if drain times out
4. **Cleanup** - flush usage/audit buffers, close DB, stop background tasks

```yaml
server:
  proxy:
    drain_timeout: 25s    # Must be less than K8s terminationGracePeriodSeconds
```
