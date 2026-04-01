---
title: "Model Aliases"
description: "Logical model names scoped per org and team"
section: models
order: 3
---
# Model Aliases

Aliases let clients use logical names like `default` or `fast` instead of specific model names. When you swap providers, clients don't need to change their code.

## How It Works

```yaml
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_KEY}
    aliases: [default, smart]
```

A client sends `model: "default"` - VoidLLM resolves it to `gpt-4o` and routes accordingly. Later, if you switch to Claude, update the config and `default` now points to a different model. Zero client changes.

## Scope and Resolution

When a client sends `model: "default"`, VoidLLM resolves the alias in this order:

1. **Team alias** - checked first (if the key is team-scoped)
2. **Org alias** - checked second
3. **Global alias** - from the YAML config (`aliases` field on the model)

The first match wins. This means a team can override what `default` means for their scope without affecting other teams.

**Example:** Global `default = gpt-4o`. Team A sets `default = claude-sonnet`. Team A gets Claude, everyone else gets GPT-4o.

Global aliases are set in the YAML config or via the Create/Edit Model dialog in the UI. Org and team aliases are currently API-only.

## API

```bash
# Set org alias
curl -X PUT https://voidllm.example.com/api/v1/orgs/{org_id}/model-aliases \
  -H "Authorization: Bearer vl_uk_..." \
  -d '{"alias": "default", "model_name": "claude-sonnet"}'

# Set team alias
curl -X PUT https://voidllm.example.com/api/v1/orgs/{org_id}/teams/{team_id}/model-aliases \
  -H "Authorization: Bearer vl_uk_..." \
  -d '{"alias": "fast", "model_name": "llama-70b"}'
```

## Common Patterns

| Alias | Use case |
|---|---|
| `default` | General purpose, the model most people should use |
| `fast` | Low-latency, cheaper model for quick tasks |
| `smart` | High-capability model for complex reasoning |
| `embedding` | Text embedding model |
| `code` | Code-optimized model |
