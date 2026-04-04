---
title: "Docker Deployment"
description: "Deploy VoidLLM with Docker and Docker Compose"
section: deployment
order: 1
---
# Docker Deployment

## Minimal Setup

```bash
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)

docker run -d --name voidllm \
  -p 8080:8080 \
  -e VOIDLLM_ADMIN_KEY -e VOIDLLM_ENCRYPTION_KEY \
  -v voidllm_data:/data \
  ghcr.io/voidmind-io/voidllm:latest
```

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

Check the logs: `docker logs voidllm`

The **email and password** are for logging into the UI at `http://localhost:8080`. The **API key** (`vl_uk_...`) is for SDK calls. These are shown once - save them.

## With a Config File

```bash
docker run -d --name voidllm \
  -p 8080:8080 \
  -e VOIDLLM_ADMIN_KEY -e VOIDLLM_ENCRYPTION_KEY \
  -v $(pwd)/voidllm.yaml:/etc/voidllm/voidllm.yaml:ro \
  -v voidllm_data:/data \
  ghcr.io/voidmind-io/voidllm:latest
```

See [Configuration Reference](../configuration.md) for all YAML options.

## Docker Compose

```bash
cp voidllm.yaml.example voidllm.yaml
# Edit voidllm.yaml - configure your models

export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
docker-compose up -d
```

## Persistence

The `-v voidllm_data:/data` mount keeps your SQLite database across container restarts. Without it, you lose all users, keys, and usage data when the container stops.

You can also use a bind mount to a local directory:

```bash
docker run -p 8080:8080 \
  -v $(pwd)/data:/data \
  ...
```

This makes the database file visible at `./data/voidllm.db` - easier to back up and inspect.

The Docker image sets `VOIDLLM_DATABASE_DSN=/data/voidllm.db` by default. Override this environment variable to change the database location.

## Environment Variables

| Variable | Required | Description |
|---|---|---|
| `VOIDLLM_ADMIN_KEY` | First start only | Bootstrap admin key (min 32 chars). Ignored after first start. |
| `VOIDLLM_ENCRYPTION_KEY` | Yes | AES-256-GCM key for upstream API key encryption. |
| `VOIDLLM_DATABASE_DSN` | No | Override the database path (default: `/data/voidllm.db`). |
| `VOIDLLM_DATABASE_DRIVER` | No | Override the database driver (default: `sqlite`, alternative: `postgres`). |
| `VOIDLLM_LICENSE` | No | Enterprise license JWT. |

## Health Check

```bash
curl http://localhost:8080/healthz
# {"status":"ok","uptime_seconds":42,"version":"0.0.13"}
```
