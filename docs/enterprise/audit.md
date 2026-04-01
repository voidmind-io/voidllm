---
title: "Audit Logging"
description: "Track admin actions with async audit log"
section: enterprise
order: 3
---
# Audit Logging

Every administrative action is automatically logged when audit logging is enabled. Requires an Enterprise license.

## Enable

```yaml
settings:
  audit:
    enabled: true
    buffer_size: 500      # events buffered before flush (default: 500)
    flush_interval: 5s    # flush to DB at least every N seconds
```

Audit events are written asynchronously - they never block the admin API.

## View

**UI:** Navigate to Security -> Audit Log. Filter by action, resource type, actor, or time range.

**API:**

```bash
curl -H "Authorization: Bearer vl_uk_..." \
  "https://voidllm.example.com/api/v1/audit-logs?resource_type=key&action=create&limit=50"
```

## What's Logged

| Action | Resources |
|---|---|
| `create` | org, team, user, key, model, service_account, membership, invite |
| `update` | org, team, user, key, model, membership |
| `delete` | org, team, user, key, model, service_account, membership |
| `revoke` | key |
| `rotate` | key |
| `login` | session |
| `activate` / `deactivate` | model |

Each event records:
- **Who** - actor ID, actor type (user/service_account/system), API key ID
- **What** - action, resource type, resource ID, description (JSON of what changed)
- **When** - UTC timestamp
- **Where** - client IP address
- **Correlation** - request ID (links to usage log and proxy access log)

## What's NOT Logged

Audit logs contain only administrative metadata. They never include:
- Prompt or response content
- Request or response bodies from the proxy
- User passwords or API key plaintext

This is consistent with VoidLLM's [zero-knowledge architecture](../security/privacy.md).

## Retention

Audit logs are stored in the database indefinitely. A configurable retention job is planned for a future release. For now, you can export and archive logs via the API.
