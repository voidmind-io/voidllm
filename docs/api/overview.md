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
| `POST /v1/images/generations` | Image generation |
| `POST /v1/audio/transcriptions` | Audio transcription |
| `POST /v1/audio/speech` | Text to speech |
| `GET /v1/models` | List available models |

VoidLLM does not validate request bodies beyond extracting the `model` field. The upstream provider handles validation.

## Admin API (`/api/v1/*`)

Management endpoints for organizations, teams, users, keys, models, and MCP servers.

### Organizations
| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/orgs` | List organizations |
| `POST` | `/api/v1/orgs` | Create organization |
| `GET` | `/api/v1/orgs/:id` | Get organization |
| `PATCH` | `/api/v1/orgs/:id` | Update organization |

### Teams
| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/orgs/:org_id/teams` | List teams |
| `POST` | `/api/v1/orgs/:org_id/teams` | Create team |
| `GET` | `/api/v1/teams/:id` | Get team |
| `PATCH` | `/api/v1/teams/:id` | Update team |
| `DELETE` | `/api/v1/teams/:id` | Delete team |

### API Keys
| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/keys` | List keys |
| `POST` | `/api/v1/keys` | Create key |
| `DELETE` | `/api/v1/keys/:id` | Revoke key |
| `POST` | `/api/v1/keys/:id/rotate` | Rotate key (24h grace) |

### Models
| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/models` | List models |
| `POST` | `/api/v1/models` | Create model |
| `PATCH` | `/api/v1/models/:id` | Update model |
| `DELETE` | `/api/v1/models/:id` | Delete model |
| `POST` | `/api/v1/models/:id/test` | Test upstream connectivity |

### Usage
| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/orgs/:org_id/usage` | Org usage stats |
| `GET` | `/api/v1/usage/me` | Current user's usage |

### MCP
| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/v1/mcp/voidllm` | Built-in MCP tools |
| `POST` | `/api/v1/mcp/:alias` | Proxy to external MCP server |
| `POST` | `/api/v1/mcp` | Code Mode tools |

## Health Endpoints

| Endpoint | Auth | Description |
|---|---|---|
| `GET /healthz` | No | Liveness probe (always 200) |
| `GET /readyz` | No | Readiness probe (503 during drain) |
| `GET /metrics` | No | Prometheus metrics |

## Swagger UI

When VoidLLM is running, the full OpenAPI spec is available at:

- **Swagger UI:** `http://localhost:8080/api/docs`
- **OpenAPI JSON:** `http://localhost:8080/api/docs/swagger.json`

A static copy of the spec is also available in the repository: [swagger.yaml](swagger.yaml)

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
| 429 | `rate_limit_exceeded` | Rate limit or token budget exceeded |
| 502 | `upstream_unavailable` | LLM provider unreachable |
