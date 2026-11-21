---
title: "Pricing"
description: "Community, Pro, Enterprise, and Enterprise Plus plans, plus a 30-day trial"
section: enterprise
order: 5
---
# Pricing

Flat pricing - no per-user fees, no per-request charges, no per-token metering. Self-hosted on your infrastructure.

| | Community | Pro | Enterprise | Enterprise Plus |
|---|---|---|---|---|
| **Price** | Free | €99/mo | €399/mo | from about €1,000/mo, billed annually |
| **Annual** | - | €990/yr | €3,990/yr | - |
| **Use case** | Personal / evaluation | Teams & production | Organization-wide | Custom requirements (contact us) |

Want to try Pro or Enterprise features first? See [30-day trial](#30-day-trial).

## What's in each tier

### Community (Free)

Everything you need to run an LLM proxy in production:

- 1 organization, 3 teams, unlimited users
- Proxy with streaming + provider adapters (OpenAI, Anthropic, Azure, Ollama, vLLM, custom)
- Full Web UI + Playground
- RBAC (4 built-in roles)
- Usage tracking + dashboard
- Rate limiting + token budgets
- Load balancing (4 strategies) + automatic failover
- Circuit breakers + per-model timeouts
- MCP gateway + Code Mode
- Prometheus metrics
- Usage export (CSV / JSON)
- BSL 1.1 - free to self-host

No feature is time-limited or artificially crippled.

### Pro (€99/mo)

Everything in Community, plus:

- Unlimited organizations + teams
- Cost reports + budget alerts
- Cross-org analytics
- Priority email support (48h)

### Enterprise (€399/mo)

Everything in Pro, plus:

- SSO / OIDC (any provider) - [Setup guide](sso.md)
- Per-org SSO configuration
- Audit logs (API + UI) - [Setup guide](audit.md)
- OpenTelemetry tracing - [Setup guide](otel.md)
- Multi-instance support (Redis)
- Dedicated Slack support (24h)

### Enterprise Plus (from about €1,000/mo, billed annually)

Everything in Enterprise, with custom terms for larger deployments. Pricing depends on scope, so please [contact us](mailto:hello@voidmind.io?subject=VoidLLM%20Enterprise%20Plus).

## 30-day trial

You can try the paid features for 30 days at no cost and without payment data.

- Request it from the UI (System -> License) as a system admin, or at [voidllm.ai/trial](https://voidllm.ai/trial).
- The trial key is sent to your email address. Paste it into System -> License to activate it.
- Nothing is sent to VoidLLM until you submit the request form. The request contains your email, an optional company name, your consent choices, the instance ID, and the product version. No prompts, responses, or usage data are included.
- No payment details are collected.
- The key stops working after 30 days. VoidLLM then reverts to Community mode and keeps proxying. If audit logs, SSO, or OpenTelemetry were enabled, a restart is needed to fully switch them off.

## License activation

See the [License Activation guide](license.md) for setup instructions.
