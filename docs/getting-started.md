---
title: "Getting Started"
description: "From docker run to your first proxied LLM request in 3 minutes"
section: root
order: 1
---
# Getting Started

VoidLLM runs as a single binary with the admin UI embedded. No separate frontend server, no Node.js, no extra containers.

## Quick Start (Docker)

```bash
docker run -p 8080:8080 \
  -v voidllm_data:/data \
  -e VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32) \
  -e VOIDLLM_ADMIN_KEY=my-admin-key-at-least-32-chars!! \
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

- **Email + Password** - for logging into the UI at `http://localhost:8080`
- **API Key** (`vl_uk_...`) - for SDK calls and MCP connections
- These are shown once - save them

## Add a Model

Edit `voidllm.yaml` or use the UI (Models -> Create Model):

```yaml
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_KEY}
    aliases: [default]
```

See [Provider Setup](models/providers.md) for all supported providers.

## Send Your First Request

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer vl_uk_your_key_here" \
  -H "Content-Type: application/json" \
  -d '{"model": "default", "messages": [{"role": "user", "content": "hello"}]}'
```

VoidLLM resolves `default` to whatever model you configured with that alias, forwards the request, and streams the response back. Under 500 microseconds of overhead.

## Connect Your IDE

### Cursor / Windsurf (LLM Proxy)

Change the base URL to your VoidLLM instance:
```
Base URL: http://localhost:8080/v1
API Key: vl_uk_...
```

### Claude Code (MCP Server)

Add to your MCP config:
```json
{
  "mcpServers": {
    "voidllm": {
      "url": "http://localhost:8080/api/v1/mcp/voidllm",
      "headers": {
        "Authorization": "Bearer vl_uk_..."
      }
    }
  }
}
```

See [IDE Integration](mcp/ide-integration.md) for detailed setup.

## Explore the UI

Open `http://localhost:8080` and explore:

- **Dashboard** - request stats, token usage, model health
- **Keys** - create and manage API keys
- **Models** - add models, configure aliases, view health
- **Usage** - track consumption by team, user, model
- **MCP Servers** - register external MCP servers
- **Playground** - test models directly in the browser

## Next Steps

- [Configuration Reference](configuration.md) - all YAML settings
- [Deployment Guide](deployment/docker.md) - Docker, Kubernetes, PostgreSQL
- [Load Balancing](models/load-balancing.md) - multi-deployment failover
- [MCP Gateway](mcp/overview.md) - proxy external MCP servers
- [RBAC](security/rbac.md) - org/team/user/key access control
