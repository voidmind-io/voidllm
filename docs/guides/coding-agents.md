---
title: "Coding Agents for Teams"
description: "Give every developer their own VoidLLM key, put budgets and rate limits on the team, and see which coding tools work through the proxy today."
section: guides
order: 1
---
# Coding Agents for Teams

How to run AI coding tools for a team through VoidLLM: one key per developer, one budget per team, a service account for CI, and an honest list of which tools can use the proxy today.

## What works today

VoidLLM serves the OpenAI-compatible API (`/v1/chat/completions` and the other endpoints listed in the [API reference](../api/overview.md)). It does not serve the Anthropic Messages API (`/v1/messages`) or the OpenAI Responses API (`/v1/responses`). Requests to those paths are rejected with `400 unsupported API endpoint`.

That decides which tools can use it as their LLM endpoint. This table only lists what was run against a local VoidLLM build, not what should work in theory:

| Client | Version | Result |
|---|---|---|
| `curl`, OpenAI model (`/v1/chat/completions`) | - | Works |
| `curl`, Anthropic model (`/v1/chat/completions`, translated to the Messages API) | - | Works |
| `curl`, `POST /v1/messages` | - | `400 unsupported API endpoint` |
| `curl`, `POST /v1/responses` | - | `400 unsupported API endpoint` |
| Claude Code, `ANTHROPIC_BASE_URL` pointed at VoidLLM | 2.1.330 | `API Error: 400 unsupported API endpoint` |
| Claude Code, VoidLLM as MCP server (`claude mcp list`) | 2.1.330 | Connected |
| Codex CLI, custom provider pointed at VoidLLM | 0.169.1 | `400 unsupported API endpoint` |
| Cursor | - | Not tested |
| Windsurf | - | Not tested |

Why the two CLI tools fail:

- Claude Code sends requests to `/v1/messages` when `ANTHROPIC_BASE_URL` is set. Anthropic documents this in its [gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol.md): a gateway must expose the Anthropic Messages format, and forward `anthropic-version` and `anthropic-beta` unchanged.
- Codex CLI documents `wire_api = "responses"` as the only supported value for a custom provider, see the [Codex configuration reference](https://developers.openai.com/codex/config-reference).

Cursor documents that requests made with your own API key are routed through Cursor's servers ([bring your own API key](https://cursor.com/help/models-and-usage/api-keys.md)). The current Cursor docs list OpenAI, Anthropic, Google, Azure and AWS Bedrock keys and do not describe a custom base URL. We did not test Cursor against VoidLLM, so check that your Cursor version has the setting before you plan around it.

Anything that speaks OpenAI Chat Completions and lets you set a base URL can use the setup below. If you try one, tell us what happened in a [GitHub issue](https://github.com/voidmind-io/voidllm/issues).

## The hierarchy

Access is organized as organization, team, user and key:

- Each developer is a user in your org and gets their own `vl_uk_` key. Their usage is attributed to them.
- Each team has its own limits.
- CI jobs use a service account with a `vl_sa_` key, so no pipeline runs on a person's key.

Limits (tokens per day and month, requests per minute and per day) can be set on the org, the team, the user's org membership and the key. Every level is checked on each request, and the most restrictive one wins. `0` means unlimited at that level. See [RBAC](../security/rbac.md) for the details.

## Setup

The examples use `$VOIDLLM` for your base URL and `$ADMIN_KEY` for a `vl_uk_` key with the org admin role. IDs come from the responses of the previous call.

### 1. Add models

```yaml
models:
  - name: gpt-4o-mini
    provider: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    aliases: [fast]
  - name: claude-haiku-4-5
    provider: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    aliases: [claude-fast]
```

Aliases mean the tools only need to know a stable name. See [Provider Setup](../models/providers.md) and [Aliases](../models/aliases.md).

### 2. Create the team with limits

```bash
curl -X POST $VOIDLLM/api/v1/orgs/$ORG_ID/teams \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Platform",
    "slug": "platform",
    "daily_token_limit": 2000000,
    "monthly_token_limit": 40000000,
    "requests_per_minute": 120
  }'
```

### 3. Create a user and a key per developer

```bash
# the user
curl -X POST $VOIDLLM/api/v1/users \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "dev@example.com",
    "display_name": "Dev One",
    "password": "a-long-initial-password",
    "org_id": "'$ORG_ID'",
    "role": "member"
  }'

# add them to the team
curl -X POST $VOIDLLM/api/v1/orgs/$ORG_ID/teams/$TEAM_ID/members \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"user_id": "'$USER_ID'", "role": "member"}'

# a key for them, counted against the team
curl -X POST $VOIDLLM/api/v1/orgs/$ORG_ID/keys \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "dev-laptop",
    "key_type": "user_key",
    "user_id": "'$USER_ID'",
    "team_id": "'$TEAM_ID'",
    "daily_token_limit": 500000,
    "requests_per_minute": 30
  }'
```

The key is only returned once, in the `key` field of that response. If you prefer not to set passwords yourself, use [invite links](../security/rbac.md#user-onboarding) instead.

A per-developer cap that covers all of a person's keys is set on the org membership: `PATCH /api/v1/orgs/$ORG_ID/members/$MEMBERSHIP_ID` with the same limit fields.

### 4. A service account for CI

```bash
curl -X POST $VOIDLLM/api/v1/orgs/$ORG_ID/service-accounts \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "ci-bot", "team_id": "'$TEAM_ID'"}'

curl -X POST $VOIDLLM/api/v1/orgs/$ORG_ID/keys \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ci-key",
    "key_type": "sa_key",
    "service_account_id": "'$SA_ID'",
    "monthly_token_limit": 5000000
  }'
```

For a team service account the team comes from the service account. Passing `team_id` on the key is rejected with `team_id must not be set for sa_key`.

### 5. Restrict models

```bash
curl -X PUT $VOIDLLM/api/v1/orgs/$ORG_ID/teams/$TEAM_ID/model-access \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": ["gpt-4o-mini"]}'
```

The same call exists for the org (`/orgs/$ORG_ID/model-access`) and for a single key (`/orgs/$ORG_ID/keys/$KEY_ID/model-access`). An empty list means no restriction at that level.

A key restricted to `gpt-4o-mini` that asks for another model gets:

```json
{"error":{"code":"model_access_denied","message":"model access denied", ...}}
```

with status 403, and `GET /v1/models` lists only the models that key may use.

### 6. Point a tool at VoidLLM

For an OpenAI-compatible client:

```bash
export OPENAI_BASE_URL=https://voidllm.example.com/v1
export OPENAI_API_KEY=vl_uk_...
```

Many tools read these two variables, others have the same two fields in their settings. Use an alias or model name from `GET /v1/models` as the model.

## What the limits look like in practice

These are the responses from the local test. The numbers are small on purpose.

- A key with `requests_per_minute` exhausted: `429` with `rate_limit_exceeded`.
- A key whose `daily_token_limit` is used up: `429` with `token_limit_exceeded`, message `token budget exceeded`.
- A key with no limit of its own, in a team whose `daily_token_limit` is used up: the same `429 token_limit_exceeded`.

Changes to a team's limits are not instant. In the test, the first request after lowering the team limit still passed, and the next ones after the cache refresh were rejected. The refresh interval is `cache.key_ttl` (default 30s, see the [configuration reference](../configuration.md)). Changes to a key's limits took effect on the next request.

Budgets are in tokens and requests, not currency. Cost figures in the usage views are estimates from the model prices you configure.

## Which headers reach the provider

VoidLLM does not pass client headers through. For the requests it forwards, it copies only `Content-Type`, `Accept`, `Accept-Language` and `X-Request-ID` from the client. The client's `Authorization` header is replaced with the upstream key from the model config, and `User-Agent` is set to `VoidLLM/0.1`.

For models with `provider: anthropic`, the request goes to `/v1/messages` with the upstream key in `x-api-key` and `anthropic-version: 2023-06-01`. A client-supplied `anthropic-version` or `anthropic-beta` header is not forwarded. We confirmed this against a local test upstream: the upstream saw `anthropic-version: 2023-06-01`, the copied `X-Request-ID`, and no `anthropic-beta` or custom headers.

This is one more reason Claude Code cannot go through VoidLLM today: Anthropic's guide says a gateway must forward `anthropic-beta` unchanged.

## Usage per developer

Org admins see usage in the UI under **Usage**, grouped by Model, Team, User, Key, Day or Hour. The same data is available from the API:

```bash
curl "$VOIDLLM/api/v1/orgs/$ORG_ID/usage?from=2026-11-01T00:00:00Z&to=2026-12-01T00:00:00Z&group_by=user" \
  -H "Authorization: Bearer $ADMIN_KEY"
```

Each row has the group label (the developer's display name), request count, prompt, completion and total tokens, an estimated cost and the average duration. Usage from a service-account key is listed in the same call, in a row without a user label.

## Claude Code as MCP client

Claude Code can use VoidLLM as an [MCP server](../mcp/ide-integration.md), which gives it the management tools and any MCP servers you register. That does not route its LLM requests through VoidLLM.

Current Claude Code requires a `type` on remote servers, a `url` entry without it is skipped. Share this `.mcp.json` in a repository:

```json
{
  "mcpServers": {
    "voidllm": {
      "type": "http",
      "url": "${VOIDLLM_URL:-http://localhost:8080}/api/v1/mcp/voidllm",
      "headers": {
        "Authorization": "Bearer ${VOIDLLM_API_KEY}"
      }
    }
  }
}
```

Each developer sets `VOIDLLM_API_KEY` to their own key, so the key never goes into the repository. Claude Code asks the developer to approve a project-scoped server the first time. The format and `${VAR}` expansion are described in the [Claude Code MCP docs](https://code.claude.com/docs/en/mcp.md). Tested with Claude Code 2.1.330: `claude mcp list` reports the server as connected.

If you manage Claude Code machines centrally, [managed settings](https://code.claude.com/docs/en/managed-settings.md) and `managed-mcp.json` are the documented way to distribute configuration. We have not tested a managed rollout with VoidLLM.

## Privacy

VoidLLM does not store prompts or responses. It reads the `model` field to route the request and streams everything else through memory. What it keeps is usage metadata: who, when, which model, token counts, duration. That metadata can be personal data when it is tied to a named developer, so tell your team what is recorded. The architecture is designed to support GDPR compliance, see [Privacy](../security/privacy.md).

Note that the provider still sees the prompts. Your agreement with OpenAI, Anthropic or whoever you route to applies to that content.

## Related

- [RBAC](../security/rbac.md) - roles, limits inheritance, model access
- [IDE Integration](../mcp/ide-integration.md) - Cursor, Windsurf, Claude Code
- [Provider Setup](../models/providers.md) - OpenAI, Anthropic and others
- [Configuration Reference](../configuration.md) - cache and usage settings
