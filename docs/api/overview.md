---
title: "API Reference"
description: "Authentication, endpoints, error codes, and Swagger UI"
section: api
order: 1
---
# API Reference

VoidLLM exposes two API surfaces: the **Proxy API** (OpenAI-compatible, for LLM requests) and the **Admin API** (for management).

## Authentication

All requests require an API key in the `Authorization` header:

```
Authorization: Bearer vl_uk_...
```

Key types: `vl_uk_` (user), `vl_tk_` (team), `vl_sa_` (service account), `vl_sk_` (session).

## Proxy API (`/v1/*`)

The proxy forwards requests to upstream LLM providers. Any OpenAI-compatible endpoint works:

| Endpoint | Description |
|---|---|
| `POST /v1/chat/completions` | Chat completions (streaming supported) |
| `POST /v1/completions` | Text completions |
| `POST /v1/embeddings` | Text embeddings |
| `POST /v1/rerank` | Document reranking (OpenAI-compatible backends such as vLLM; not available for Anthropic, Gemini, Vertex or Bedrock models) |
| `POST /v1/score` | Text pair scoring (OpenAI-compatible backends such as vLLM; not available for Anthropic, Gemini, Vertex or Bedrock models) |
| `POST /v1/images/generations` | Image generation |
| `POST /v1/audio/transcriptions` | Audio transcription |
| `POST /v1/audio/speech` | Text to speech |
| `GET /v1/models` | List available models |

Before forwarding, VoidLLM checks that the request body is within the size limit (`server.proxy.max_request_body`, default 20 MB) and parses it to read the `model` field, which is required. It then applies rate and token limits and resolves the model. It does not validate the rest of the body. The upstream provider handles that, except where an adapter translates the request for the provider. Paths other than those listed above are rejected with `400 unsupported API endpoint`.

## Admin API (`/api/v1/*`)

Management endpoints for organizations, teams, users, keys, models, and MCP servers. Roles are described in [RBAC](../security/rbac.md). Routes below are grouped as registered in `internal/api/admin/routes.go`.

### Current user
| Method | Endpoint |
|---|---|
| `GET` | `/api/v1/me` |
| `POST` | `/api/v1/me/password` |
| `GET` | `/api/v1/me/available-models` |
| `GET` | `/api/v1/usage/me` |
| `GET` | `/api/v1/dashboard/stats` |

### Organizations
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs` |
| `GET` | `/api/v1/orgs` |
| `GET` | `/api/v1/orgs/:org_id` |
| `PATCH` | `/api/v1/orgs/:org_id` |
| `DELETE` | `/api/v1/orgs/:org_id` |

### Users
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/users` |
| `GET` | `/api/v1/users` |
| `GET` | `/api/v1/users/:user_id` |
| `PATCH` | `/api/v1/users/:user_id` |
| `DELETE` | `/api/v1/users/:user_id` |

### Org memberships
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/members` |
| `GET` | `/api/v1/orgs/:org_id/members` |
| `PATCH` | `/api/v1/orgs/:org_id/members/:membership_id` |
| `DELETE` | `/api/v1/orgs/:org_id/members/:membership_id` |

### Teams
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/teams` |
| `GET` | `/api/v1/orgs/:org_id/teams` |
| `GET` | `/api/v1/orgs/:org_id/teams/:team_id` |
| `PATCH` | `/api/v1/orgs/:org_id/teams/:team_id` |
| `DELETE` | `/api/v1/orgs/:org_id/teams/:team_id` |

### Team memberships
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/teams/:team_id/members` |
| `GET` | `/api/v1/orgs/:org_id/teams/:team_id/members` |
| `PATCH` | `/api/v1/orgs/:org_id/teams/:team_id/members/:membership_id` |
| `DELETE` | `/api/v1/orgs/:org_id/teams/:team_id/members/:membership_id` |

### Service accounts
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/service-accounts` |
| `GET` | `/api/v1/orgs/:org_id/service-accounts` |
| `GET` | `/api/v1/orgs/:org_id/service-accounts/:sa_id` |
| `PATCH` | `/api/v1/orgs/:org_id/service-accounts/:sa_id` |
| `DELETE` | `/api/v1/orgs/:org_id/service-accounts/:sa_id` |

### Invites
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/invites` |
| `GET` | `/api/v1/orgs/:org_id/invites` |
| `DELETE` | `/api/v1/orgs/:org_id/invites/:invite_id` |

### API keys
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/keys` |
| `GET` | `/api/v1/orgs/:org_id/keys` |
| `GET` | `/api/v1/orgs/:org_id/keys/:key_id` |
| `PATCH` | `/api/v1/orgs/:org_id/keys/:key_id` |
| `DELETE` | `/api/v1/orgs/:org_id/keys/:key_id` |
| `POST` | `/api/v1/orgs/:org_id/keys/:key_id/rotate` |

### Models

`GET /api/v1/models/health` is open to any authenticated member. The other model routes require a system admin.

| Method | Endpoint |
|---|---|
| `GET` | `/api/v1/models/health` |
| `POST` | `/api/v1/models/test-connection` |
| `POST` | `/api/v1/models` |
| `GET` | `/api/v1/models` |
| `GET` | `/api/v1/models/:model_id` |
| `PATCH` | `/api/v1/models/:model_id` |
| `DELETE` | `/api/v1/models/:model_id` |
| `PATCH` | `/api/v1/models/:model_id/activate` |
| `PATCH` | `/api/v1/models/:model_id/deactivate` |
| `POST` | `/api/v1/models/:model_id/deployments` |
| `GET` | `/api/v1/models/:model_id/deployments` |
| `PATCH` | `/api/v1/models/:model_id/deployments/:deployment_id` |
| `DELETE` | `/api/v1/models/:model_id/deployments/:deployment_id` |

### Model access
| Method | Endpoint |
|---|---|
| `GET` | `/api/v1/orgs/:org_id/model-access` |
| `PUT` | `/api/v1/orgs/:org_id/model-access` |
| `GET` | `/api/v1/orgs/:org_id/teams/:team_id/model-access` |
| `PUT` | `/api/v1/orgs/:org_id/teams/:team_id/model-access` |
| `GET` | `/api/v1/orgs/:org_id/keys/:key_id/model-access` |
| `PUT` | `/api/v1/orgs/:org_id/keys/:key_id/model-access` |

### Model aliases
| Method | Endpoint |
|---|---|
| `POST` | `/api/v1/orgs/:org_id/model-aliases` |
| `GET` | `/api/v1/orgs/:org_id/model-aliases` |
| `DELETE` | `/api/v1/orgs/:org_id/model-aliases/:alias_id` |
| `POST` | `/api/v1/orgs/:org_id/teams/:team_id/model-aliases` |
| `GET` | `/api/v1/orgs/:org_id/teams/:team_id/model-aliases` |
| `DELETE` | `/api/v1/orgs/:org_id/teams/:team_id/model-aliases/:alias_id` |

### Usage
| Method | Endpoint |
|---|---|
| `GET` | `/api/v1/usage` |
| `GET` | `/api/v1/orgs/:org_id/usage` |
| `GET` | `/api/v1/mcp-usage` |
| `GET` | `/api/v1/orgs/:org_id/mcp-usage` |

### Audit logs
| Method | Endpoint |
|---|---|
| `GET` | `/api/v1/audit-logs` |

### Other routes

These are registered in `internal/api/admin/routes.go` and are not in the [OpenAPI spec](swagger.yaml) yet:

- MCP server management: `/api/v1/mcp-servers`, `/api/v1/orgs/:org_id/mcp-servers`, `/api/v1/orgs/:org_id/teams/:team_id/mcp-servers`, and per-server routes under `/api/v1/mcp-servers/:server_id` (activate, test, blocklist, tools, refresh).
- MCP access control: `mcp-access` under the org, team and key paths, and `/api/v1/orgs/:org_id/available-mcp-servers`.
- Org SSO: `/api/v1/orgs/:org_id/sso` and `/sso/test`, plus `GET /api/v1/settings/sso` for the global configuration.
- License: `GET /api/v1/license`, `PUT /api/v1/settings/license`, `POST /api/v1/license/trial`.
- `GET /api/v1/system/update-check` for the cached update status, open to any authenticated key.

### Public routes

These need no API key:

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/v1/auth/login` | Log in with email and password |
| `GET` | `/api/v1/auth/providers` | Discover available login providers |
| `GET` | `/api/v1/invites/peek` | Look up an invite |
| `POST` | `/api/v1/invites/redeem` | Redeem an invite |
| `GET` | `/api/v1/auth/oidc/login` | Start OIDC login, only registered when an SSO provider is configured |
| `GET` | `/api/v1/auth/oidc/callback` | OIDC callback, only registered when an SSO provider is configured |

### MCP
| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/v1/mcp/voidllm` | Built-in MCP tools |
| `POST` | `/api/v1/mcp/:alias` | Proxy to external MCP server |
| `GET` | `/api/v1/mcp/:alias` | Server-sent event stream for the same endpoint |
| `POST` | `/api/v1/mcp` | Code Mode tools, only registered when Code Mode is enabled |
| `GET` | `/api/v1/mcp` | Server-sent event stream for Code Mode, only registered when Code Mode is enabled |

MCP requests are subject to an Origin check.

## Health Endpoints

| Endpoint | Auth | Description |
|---|---|---|
| `GET /healthz` | No | Liveness probe (always 200) |
| `GET /readyz` | No | Readiness probe, pings the database. Returns 503 while draining or when the database is unavailable |
| `GET /metrics` | No | Prometheus metrics |

## Swagger UI

When VoidLLM is running, the full OpenAPI spec is available at:

- **Swagger UI:** `http://localhost:8080/api/docs`
- **OpenAPI JSON:** `http://localhost:8080/api/docs/swagger.json`

A static copy of the spec is also available in the repository: [swagger.yaml](https://github.com/voidmind-io/voidllm/blob/main/docs/api/swagger.yaml)

## Error Responses

All errors follow a consistent format:

```json
{
  "error": {
    "code": "not_found",
    "message": "model not found",
    "request_id": "019d3ba2-8130-7d96-b60e-2e0da53e2650"
  }
}
```

| Status | Code | Meaning |
|---|---|---|
| 400 | `bad_request` | Invalid request body or parameters |
| 401 | `unauthorized` | Missing or invalid API key |
| 403 | `forbidden` | Insufficient permissions |
| 404 | `not_found` | Resource not found |
| 409 | `conflict` | Resource already exists |
| 403 | `model_access_denied` | The key may not use this model |
| 404 | `model_not_found` | The requested model does not exist (proxy API) |
| 413 | `request_too_large` | Request body exceeds the size limit (proxy API) |
| 429 | `rate_limit_exceeded` | Request rate limit exceeded |
| 429 | `token_limit_exceeded` | Daily or monthly token budget exceeded |
| 502 | `upstream_unavailable` | LLM provider unreachable |
