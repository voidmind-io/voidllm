# VoidLLM + Open WebUI + Ollama

A chat UI for your team on a local model, with VoidLLM in between for keys,
budgets, model allowlists and usage tracking.

```
Browser -> Open WebUI (:3000) -> VoidLLM (:8080, team key) -> Ollama (internal only)
```

Ollama has no published port and sits on a network Open WebUI is not part
of. Open WebUI only knows VoidLLM's `/v1` endpoint and a VoidLLM team key, so
every request is authenticated, limited and counted per team.

Tested with: VoidLLM `0.0.31`, Open WebUI `v0.13.4`, Ollama `0.40.17`,
model `qwen2.5:0.5b`, Docker Compose 5.6.1 (2026-11-22).

Full background (per-team separation, budgets, what is and is not stored):
[docs/deployment/open-webui.md](../../docs/deployment/open-webui.md)

## 1. Secrets

```bash
cp .env.example .env
openssl rand -base64 32   # paste as VOIDLLM_ENCRYPTION_KEY
openssl rand -base64 32   # paste as VOIDLLM_ADMIN_KEY
openssl rand -base64 32   # paste as WEBUI_SECRET_KEY
```

`docker compose` refuses to start without `VOIDLLM_ENCRYPTION_KEY` and
`WEBUI_SECRET_KEY`. `VOIDLLM_ADMIN_KEY` is needed for the first start.

## 2. Start Ollama and VoidLLM (not Open WebUI yet)

```bash
docker compose up -d ollama voidllm
docker compose exec ollama ollama pull qwen2.5:0.5b
```

The model name in `voidllm.yaml` must match the Ollama tag exactly. To use
another model, pull it and add an entry with the same name.

## 3. Get the admin API key

On first start VoidLLM bootstraps a system admin and prints its credentials
once to stderr:

```bash
docker compose logs voidllm | grep -A4 "BOOTSTRAP COMPLETE"
```

Save the `API Key` (`vl_uk_...`) and the email/password (for the UI at
http://localhost:8080). `VOIDLLM_ADMIN_KEY` itself is only the bootstrap
trigger, not an API key.

```bash
export ADMIN=vl_uk_...   # the key from the log
```

## 4. Create a team and a team key

Everything below can also be done in the VoidLLM UI. The bootstrap created
an organization named "Default" (rename it in the UI, or set
`bootstrap.org_name` in `voidllm.yaml` before the first start). The
Community edition has one organization; teams are the unit you separate.

```bash
ORG=$(curl -s http://localhost:8080/api/v1/orgs \
  -H "Authorization: Bearer $ADMIN" | jq -r '.data[0].id')

TEAM=$(curl -s -X POST http://localhost:8080/api/v1/orgs/$ORG/teams \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"Support","slug":"support","daily_token_limit":200000,"requests_per_minute":60}' | jq -r .id)

# Optional: restrict the team to specific models (empty list = all models)
curl -s -X PUT http://localhost:8080/api/v1/orgs/$ORG/teams/$TEAM/model-access \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"models":["qwen2.5:0.5b"]}'

curl -s -X POST http://localhost:8080/api/v1/orgs/$ORG/keys \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d "{\"name\":\"open-webui-support\",\"key_type\":\"team_key\",\"team_id\":\"$TEAM\"}" | jq -r .key
```

The key (`vl_tk_...`) is shown only once. Put it into `.env` as
`OPENAI_API_KEY`.

Quick check that the key works end to end:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer vl_tk_..." -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"Say hi"}]}'
```

## 5. Start Open WebUI

```bash
docker compose up -d open-webui
```

Open http://localhost:3000 and create the first account (it becomes the Open
WebUI admin). `qwen2.5:0.5b` shows up in the model selector, served through
VoidLLM.

Open WebUI reads `OPENAI_API_BASE_URL` and `OPENAI_API_KEY` only on its first
start and then keeps them in its own database. To change the key later, use
Open WebUI's Admin Settings > Connections, or set `ENABLE_PERSISTENT_CONFIG=false`.

## 6. Check usage in VoidLLM

```bash
FROM=$(date -u -d '-1 day' +%Y-%m-%dT%H:%M:%SZ)   # macOS: date -u -v-1d +%Y-%m-%dT%H:%M:%SZ
TO=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)    # macOS: date -u -v+1H +%Y-%m-%dT%H:%M:%SZ
curl -s "http://localhost:8080/api/v1/orgs/$ORG/usage?group_by=team&from=$FROM&to=$TO" \
  -H "Authorization: Bearer $ADMIN"
```

Or open the Usage page in the VoidLLM UI. You see tokens, request counts and
durations per team and model. No prompt or response content.

## Clean up

```bash
docker compose down -v   # removes containers and all three volumes (chats, keys, models)
```
