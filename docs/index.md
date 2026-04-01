---
title: "Documentation"
description: "VoidLLM documentation home - guides, reference, and API docs"
section: root
order: 0
---
# VoidLLM Documentation

Privacy-first LLM proxy and AI gateway. Self-hosted, single binary, sub-500us overhead.

## Getting Started

- [Quick Start](getting-started.md) - from `docker run` to first proxied request
- [Configuration Reference](configuration.md) - all YAML settings with examples

## Deployment

- [Docker](deployment/docker.md) - Docker and Docker Compose
- [Kubernetes](deployment/kubernetes.md) - Helm chart, Istio, health probes
- [Reverse Proxy](deployment/reverse-proxy.md) - Nginx, Caddy, Traefik
- [Database](deployment/database.md) - SQLite, PostgreSQL, migration

## Models

- [Provider Setup](models/providers.md) - OpenAI, Anthropic, Azure, Ollama, vLLM, custom
- [Load Balancing](models/load-balancing.md) - strategies, failover, circuit breakers
- [Aliases](models/aliases.md) - logical names for models (per org/team)

## MCP Gateway

- [Overview](mcp/overview.md) - what is MCP, why a gateway
- [Server Setup](mcp/servers.md) - register servers, access control, auth
- [Code Mode](mcp/code-mode.md) - WASM-sandboxed multi-tool orchestration
- [IDE Integration](mcp/ide-integration.md) - Claude Code, Cursor, Windsurf

## Security

- [RBAC](security/rbac.md) - roles, permissions, access control hierarchy
- [Privacy](security/privacy.md) - zero-knowledge architecture, GDPR
- [Hardening](security/hardening.md) - security checklist, TLS, network policies

## Enterprise

- [License](enterprise/license.md) - activation, verification, graceful degradation
- [SSO / OIDC](enterprise/sso.md) - Google, Azure AD, any OIDC provider
- [Audit Logs](enterprise/audit.md) - admin action logging
- [OpenTelemetry](enterprise/otel.md) - distributed tracing
- [Pricing](enterprise/pricing.md) - Community, Pro, Enterprise, Founding Member

## API

- [Overview](api/overview.md) - authentication, endpoints, error codes
- [OpenAPI Spec](api/swagger.yaml) - full API specification

## Resources

- [Troubleshooting](troubleshooting.md) - common issues and solutions
- [Blog](https://voidllm.ai/blog) - architecture deep-dives, benchmarks, guides
- [FAQ](https://voidllm.ai/faq) - frequently asked questions
- [GitHub](https://github.com/voidmind-io/voidllm) - source code, issues, releases
- [Security Policy](https://github.com/voidmind-io/voidllm/blob/main/SECURITY.md) - vulnerability reporting
