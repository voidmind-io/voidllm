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

On the proxy routes (`/v1/*`) the key can instead be sent in an `x-api-key` header, which is what Anthropic SDKs send by default. `x-api-key` is only read when the request has no `Authorization` header at all; an `Authorization` header, even an empty one, always takes precedence. The Admin API accepts only `Authorization: Bearer`.

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
| `POST /v1/messages` | Anthropic Messages API, native passthrough (streaming supported). `provider: anthropic` only, see [Native endpoints](#native-endpoints) |
| `POST /v1/messages/count_tokens` | Anthropic token counting, native passthrough. `provider: anthropic` only |
| `POST /v1/responses` | OpenAI Responses API, native passthrough (streaming supported). `provider: openai`, `custom`, `vllm` or `ollama` only |

Before forwarding, VoidLLM checks that the request body is within the size limit (`server.proxy.max_request_body`, default 20 MB) and parses it to read the `model` field, which is required. It then applies rate and token limits and resolves the model. It does not validate the rest of the body. The upstream provider handles that, except where an adapter translates the request for the provider. Paths other than those listed above are rejected with `400 unsupported API endpoint`.

### Native endpoints

`/v1/messages`, `/v1/messages/count_tokens` and `/v1/responses` are forwarded in their own wire format, without translation. They are meant for clients that speak these APIs natively, such as Claude Code and Codex CLI (see [Coding Agents for Teams](../guides/coding-agents.md)).

| Endpoint | Supported providers | Upstream URL |
|---|---|---|
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | `anthropic` | `{base_url}/v1/messages`, `{base_url}/v1/messages/count_tokens` |
| `POST /v1/responses` | `openai`, `custom`, `vllm`, `ollama` | `{base_url}/responses` |

- **Other providers:** a request for a model whose provider does not support the endpoint is rejected with `400 endpoint not supported for this model`. For a model with several deployments, only deployments with a supporting provider are used, and fallback models without one are skipped.
- **Methods and sub-resources:** only `POST` is accepted. Other methods and sub-resources such as `GET` or `DELETE /v1/responses/{id}` are rejected with `400 unsupported API endpoint` before any limit is charged.
- **Request body:** forwarded as sent, except that the `model` field is rewritten when an alias was used and `/v1/responses` gets a `store: false` default (see below).
- **Headers:** `anthropic-version` (form `YYYY-MM-DD`, otherwise `2023-06-01` is sent) and `anthropic-beta` are forwarded on the Messages endpoints, `OpenAI-Beta` on `/v1/responses`. Beta headers are forwarded only when they are a comma-separated list of lowercase tokens without spaces, at most 512 bytes, and dropped otherwise. The upstream key from the model config is sent as `x-api-key` (Messages) or `Authorization: Bearer` (Responses). Upstream `Request-Id` and `anthropic-ratelimit-*` response headers are passed back to the client.
- **Limits:** rate limits, token budgets and model access apply exactly as on `/v1/chat/completions`.

#### `/v1/responses` is stateless

VoidLLM does not proxy the server-side state of the Responses API:

- When the request has no `store` field, VoidLLM sets `store: false`, so the upstream does not persist the response. A `store` value sent by the client is kept.
- A request with a non-null `previous_response_id` or `conversation`, or with `background: true`, is rejected with `400 unsupported_parameter`. Send the full context in `input` instead.
- `GET /v1/responses/{id}` and `DELETE /v1/responses/{id}` are not supported (`400 unsupported API endpoint`).

#### Usage and cost

Usage is recorded the same way as for chat completions: prompt and completion tokens, cache read and cache write tokens (Messages) or cached input tokens (Responses), the estimated cost from the model's pricing, duration and, for streams, TTFT. If a client disconnects during a stream, VoidLLM keeps reading the upstream stream for up to 30 seconds without forwarding it, so the final token counts are still recorded and charged.

`/v1/messages/count_tokens` creates no usage record. It has no cost and does not use up token budget, but it counts against request rate limits, and a key whose budget is exhausted is rejected there too.

No prompt or response content is stored, logged or persisted on these endpoints, the same as on all others.

#### PII filtering on native endpoints

[PII anonymization](../security/pii.md) only understands the chat completions format, so native requests are never anonymized. Instead, when PII anonymization is enabled, a native request is only sent to deployments for which anonymization does not apply (for example a private destination by default, or one with `pii_filter: false`). If none of the model's deployments qualify, the request is rejected with `400 not available while PII filtering is enabled for this model`. Nothing is forwarded in that case.

#### Errors on native endpoints

- **`/v1/messages` and `/v1/messages/count_tokens`:** errors created by VoidLLM (authentication, limits, unknown model, unsupported endpoint and so on) use the Anthropic error envelope, so Anthropic SDKs can parse them: `{"type":"error","error":{"type":"rate_limit_error","message":"token budget exceeded"}}`. The `error.type` follows the status code: `authentication_error` (401), `permission_error` (403), `not_found_error` (404), `request_too_large` (413), `rate_limit_error` (429), `overloaded_error` (503), `invalid_request_error` (other 4xx), `api_error` (5xx).
- **`/v1/responses`:** VoidLLM errors use the standard envelope described under [Error Responses](#error-responses).
- **Upstream errors** are passed to the client unchanged, with the upstream status code.
- **Streams:** if a stream breaks off, VoidLLM ends it with an `error` event in the format of the endpoint (Anthropic or Responses).

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

All errors follow a consistent format, except VoidLLM errors on `/v1/messages` and `/v1/messages/count_tokens`, which use the Anthropic envelope (see [Errors on native endpoints](#errors-on-native-endpoints)):

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
| 400 | `unsupported_parameter` | `previous_response_id`, `conversation` or `background: true` on `/v1/responses` |
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

### Limit rejections

A `429` caused by a VoidLLM limit carries a `Retry-After` header with the number of seconds until the limit resets (with Redis-backed request rate limits, the window length as an upper bound). For a token budget that is the next midnight UTC (daily) or the 1st of the next month UTC (monthly). A token budget rejection also sets `x-should-retry: false`, because retrying before the reset cannot succeed. On `/v1/responses` its error additionally has `"type": "insufficient_quota"`, which OpenAI clients such as Codex CLI treat as final:

```json
{"error":{"type":"insufficient_quota","code":"token_limit_exceeded","message":"token budget exceeded","request_id":"..."}}
```
