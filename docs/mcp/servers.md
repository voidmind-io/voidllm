---
title: "MCP Server Setup"
description: "Register external MCP servers with access control and auth"
section: mcp
order: 2
---
# MCP Server Setup

Register external MCP servers to proxy tool calls through VoidLLM with access control, auth management, and usage tracking.

## Registration

Three ways to register MCP servers:

### YAML Config

```yaml
mcp_servers:
  - name: AWS Knowledge
    alias: aws
    url: https://knowledge-mcp.global.api.aws
    auth_type: none

  - name: Internal Tools
    alias: tools
    url: https://internal-mcp.company.com
    auth_type: bearer
    auth_token: ${MCP_TOOLS_TOKEN}
```

YAML servers are synced to the database at startup. Changes require a restart.

### Admin API

```bash
curl -X POST https://voidllm.example.com/api/v1/mcp-servers \
  -H "Authorization: Bearer vl_uk_..." \
  -H "Content-Type: application/json" \
  -d '{
    "name": "GitHub MCP",
    "alias": "github",
    "url": "https://mcp.github.com/sse",
    "auth_type": "bearer",
    "auth_token": "ghp_..."
  }'
```

API-created servers are stored in the database and can be managed without restarts.

### Admin UI

Navigate to MCP Servers in the sidebar. Click "Add Server", fill in name, alias, URL, and auth type. The UI also supports testing connectivity, managing tool blocklists, and toggling Code Mode per server.

## Scope

Servers can be registered at three levels:

| Scope | Visible to | Created by |
|---|---|---|
| **Global** | All orgs (with access grant) | system_admin |
| **Org** | Members of that org | org_admin |
| **Team** | Members of that team | team_admin |

Org-scoped and team-scoped servers are automatically accessible to their scope. Global servers require explicit access grants.

## Access Control

Global MCP servers are **closed by default** - organizations must explicitly grant access.

### Granting Access (UI)

1. Go to Organization -> MCP Servers tab
2. Toggle access for each global server
3. Save

### Granting Access (API)

```bash
curl -X PUT https://voidllm.example.com/api/v1/orgs/{org_id}/mcp-access \
  -H "Authorization: Bearer vl_uk_..." \
  -H "Content-Type: application/json" \
  -d '{"servers": ["server-uuid-1", "server-uuid-2"]}'
```

Team-level restrictions work the same way - teams can only restrict within what the org allows, never expand.

System admins bypass access checks entirely.

## Auth Types

| Type | Header sent to upstream | Config |
|---|---|---|
| `none` | No auth header | `auth_type: none` |
| `bearer` | `Authorization: Bearer <token>` | `auth_type: bearer`, `auth_token: <token>` |
| `header` | Custom header + value | `auth_type: header`, `auth_header: X-API-Key`, `auth_token: <key>` |

Auth tokens are encrypted at rest with AES-256-GCM.

## Tool Blocklist

Admins can block specific tools per server. Blocked tools are invisible to Code Mode and return an error when called directly.

Manage via UI (MCP Servers -> expand server -> toggle tools) or API:

```bash
# Block a tool
curl -X POST https://voidllm.example.com/api/v1/mcp-servers/{id}/blocklist \
  -H "Authorization: Bearer vl_uk_..." \
  -d '{"tool_name": "dangerous_tool"}'

# Unblock
curl -X DELETE https://voidllm.example.com/api/v1/mcp-servers/{id}/blocklist \
  -d '{"tool_name": "dangerous_tool"}'
```

## Configuration Options

```yaml
settings:
  mcp:
    call_timeout: 30s           # timeout per proxied tool call
    allow_private_urls: false   # block MCP servers on localhost/private IPs
```

## Client Config Snippets

The MCP Servers page includes a copy button that generates the exact JSON config for your IDE. Click the chevron on any server to see the config snippet.

## Known Limitations

- **SSE transport not supported** - MCP servers using the deprecated SSE protocol (pre 2025-03-26 spec) are auto-detected and deactivated. Use Streamable HTTP instead.
- **No per-user OAuth** - upstream auth is service-level (one token per server), not per-user. Per-user OAuth via the MCP spec's Third-Party Authorization Flow is on the [roadmap](https://github.com/voidmind-io/voidllm/blob/main/docs/milestones.md).
