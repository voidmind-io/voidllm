---
title: "Open WebUI"
description: "Put VoidLLM between Open WebUI and Ollama for per-team keys, budgets and usage tracking"
section: deployment
order: 5
---
# Open WebUI + Ollama

[Open WebUI](https://docs.openwebui.com/) is a popular self-hosted chat UI. Pointed directly at
Ollama, everyone who can log in shares the same unmetered model access. With VoidLLM in between,
each Open WebUI connection uses a VoidLLM key, so you get per-team token budgets, rate limits,
model allowlists and usage reports, without touching Open WebUI itself.

```
Browser -> Open WebUI -> VoidLLM /v1 (team key) -> Ollama /v1
```

A ready-to-run Docker Compose recipe lives in
[`examples/open-webui/`](https://github.com/voidmind-io/voidllm/tree/main/examples/open-webui).
It was tested end to end with VoidLLM 0.0.31, Open WebUI v0.13.4 and Ollama 0.40.17.

## How the pieces connect

**Ollama as a VoidLLM provider.** Ollama exposes an OpenAI-compatible API under `/v1`, and VoidLLM
passes requests through unchanged with the `ollama` provider:

```yaml
models:
  - name: qwen2.5:0.5b          # must match the Ollama model tag
    provider: ollama
    base_url: http://ollama:11434/v1
    aliases: [default]
```

VoidLLM forwards the model name upstream, so `name` must be the exact Ollama tag. Add one entry per
model you pull. See [Provider Setup](../models/providers.md#ollama).

**Do not expose Ollama.** Ollama has no authentication. In the recipe it has no published port and
sits on a `backend` network that Open WebUI is not attached to; VoidLLM joins both `backend` and
`frontend`. The only path to Ollama is through VoidLLM. If Open WebUI could reach Ollama directly, an
Open WebUI admin could add it as a connection and bypass every limit. `ENABLE_OLLAMA_API=false` also
stops Open WebUI from looking for a local Ollama.

**VoidLLM as Open WebUI's OpenAI connection.** Open WebUI talks to any OpenAI-compatible endpoint.
Configure it with these environment variables
([Open WebUI env reference](https://docs.openwebui.com/reference/env-configuration/#openai)):

| Variable | Value |
|---|---|
| [`OPENAI_API_BASE_URL`](https://docs.openwebui.com/reference/env-configuration/#openai_api_base_url) | `http://voidllm:8080/v1` |
| [`OPENAI_API_KEY`](https://docs.openwebui.com/reference/env-configuration/#openai_api_key) | a VoidLLM team key, `vl_tk_...` |
| [`ENABLE_OLLAMA_API`](https://docs.openwebui.com/reference/env-configuration/#enable_ollama_api) | `false` |

For several connections, Open WebUI also accepts semicolon-separated
[`OPENAI_API_BASE_URLS`](https://docs.openwebui.com/reference/env-configuration/#openai_api_base_urls)
and [`OPENAI_API_KEYS`](https://docs.openwebui.com/reference/env-configuration/#openai_api_keys).

Open WebUI lists models by calling `GET /v1/models` on VoidLLM. VoidLLM answers with the models the
key is allowed to use, so a team's model allowlist directly controls what appears in Open WebUI's
model selector.

> **Open WebUI keeps its first-start config.** With the default `ENABLE_PERSISTENT_CONFIG=true`,
> Open WebUI reads these variables on its first start and then uses the values stored in its own
> database. Create the VoidLLM key before the first start of Open WebUI. To change it later, use
> Open WebUI's Admin Settings > Connections, or set `ENABLE_PERSISTENT_CONFIG=false`.
>
> **Set `WEBUI_SECRET_KEY`.** Without it, the Open WebUI container generates a session key outside its
> data volume, so every container recreate logs all users out.

## Create a team and a key

After VoidLLM's first start, take the admin API key from the bootstrap output in the container log
(see [Getting Started](../getting-started.md)), then create a team and a team key in the
organization the bootstrap created. This works the same in the VoidLLM UI (Teams, Keys).

```bash
ORG=$(curl -s http://localhost:8080/api/v1/orgs \
  -H "Authorization: Bearer $ADMIN" | jq -r '.data[0].id')

TEAM=$(curl -s -X POST http://localhost:8080/api/v1/orgs/$ORG/teams \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"Support","slug":"support","daily_token_limit":200000,"requests_per_minute":60}' | jq -r .id)

curl -s -X POST http://localhost:8080/api/v1/orgs/$ORG/keys \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d "{\"name\":\"open-webui-support\",\"key_type\":\"team_key\",\"team_id\":\"$TEAM\"}" | jq -r .key
```

The returned `vl_tk_...` key is shown once. Use it as `OPENAI_API_KEY` for Open WebUI.

The Community edition allows one organization and three teams; see [Pricing](../enterprise/pricing.md)
for more.

## Separating teams

### Option A: one Open WebUI per team (recommended)

Run one Open WebUI instance per team, each with its own team key and its own data volume. All
instances point at the same VoidLLM. This gives you:

- **Separate budgets and rate limits** per team, enforced by VoidLLM.
- **Separate model lists**, from each team's model allowlist.
- **Separate usage reports** per team in VoidLLM.
- **Separate chat histories**, since each Open WebUI has its own database.

In Compose this is a second `open-webui` service with a different port, data volume and
`OPENAI_API_KEY`, on the same `frontend` network.

What a shared team key does not give you: VoidLLM attributes all requests from that Open WebUI to the
team key, not to the individual Open WebUI user. A team key is a shared team credential with the full
team budget (see [RBAC](../security/rbac.md#limits-inheritance)). If you need per-person accounting,
see option B.

### Option B: per-user keys

Each user creates their own VoidLLM user key (`vl_uk_...`) and uses it with their own connection, so
VoidLLM tracks usage and applies [user-level limits](../security/rbac.md#limits-inheritance) per
person.

In older Open WebUI releases this worked through "Direct Connections" in the user settings. **Open
WebUI v0.13.4 removed Direct Connections** (including `ENABLE_DIRECT_CONNECTIONS`) and replaced them
with its [Tether](https://github.com/open-webui/tether) desktop app: per the
[v0.13.4 release notes](https://github.com/open-webui/open-webui/releases/tag/v0.13.4), a user adds an
OpenAI-compatible server and key in Tether on their own computer, and the models appear only for that
user. That would let a user enter `https://your-voidllm/v1` with their own `vl_uk_` key. We have not
tested the Tether path with VoidLLM; the recipe only covers option A.

## Budgets and model allowlists

Both are set in VoidLLM, per org, team, user and key, and the most restrictive value wins:

```bash
# Team budget and rate limits
curl -s -X PATCH http://localhost:8080/api/v1/orgs/$ORG/teams/$TEAM \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"daily_token_limit":200000,"monthly_token_limit":4000000,"requests_per_minute":60}'

# Team model allowlist (empty list = all models the org may use)
curl -s -X PUT http://localhost:8080/api/v1/orgs/$ORG/teams/$TEAM/model-access \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"models":["qwen2.5:0.5b"]}'
```

When a team runs out of budget, VoidLLM answers with HTTP 429 (`token_limit_exceeded`), and Open WebUI
passes the 429 on to the user. See [RBAC](../security/rbac.md) for the full inheritance rules.

## Usage

```bash
curl -s "http://localhost:8080/api/v1/orgs/$ORG/usage?group_by=team&from=2026-11-01T00:00:00Z&to=2026-12-01T00:00:00Z" \
  -H "Authorization: Bearer $ADMIN"
```

Or use the Usage page in the VoidLLM UI. Usage records contain who (org, team, key), which model,
token counts, cost and duration.

## What VoidLLM does not do

- **VoidLLM does not store chat content.** Prompts and responses pass through memory only and are
  never written to VoidLLM's logs or database. See [Privacy](../security/privacy.md).
- **Open WebUI does store chats.** Open WebUI keeps chat histories, uploaded files and user accounts
  in its own database (the `open_webui_data` volume in the recipe). That data is outside VoidLLM's
  control. Back it up, secure it and apply your retention rules to it like any other application
  database; see Open WebUI's
  [chat data privacy docs](https://docs.openwebui.com/security/chat-data-privacy-and-encryption/).
- **VoidLLM does not manage Open WebUI users.** Logins, roles and groups inside Open WebUI are
  configured in Open WebUI. VoidLLM only sees the key on each request.
- **Ollama traffic inside the Compose network is plain HTTP.** Keep Ollama on a private network. Put
  a [reverse proxy](reverse-proxy.md) with TLS in front of VoidLLM and Open WebUI when they are
  reachable beyond localhost.
