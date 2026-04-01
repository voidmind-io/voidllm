---
title: "Provider Setup"
description: "Configure OpenAI, Anthropic, Azure, Ollama, vLLM, and custom providers"
section: models
order: 1
---
# Provider Setup

VoidLLM supports 6 providers. Each handles request/response translation differently.

## OpenAI

Direct passthrough - no translation needed.

```yaml
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_KEY}
    aliases: [default, smart]
```

## Anthropic

VoidLLM translates between the OpenAI request format and Anthropic's Messages API automatically. Your clients send OpenAI-format requests, VoidLLM handles the conversion.

```yaml
models:
  - name: claude-sonnet
    provider: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
```

Note: Claude Code talks directly to Anthropic's API for LLM access - you can't route its LLM requests through VoidLLM. But you can add VoidLLM as an [MCP server](../mcp/ide-integration.md) in Claude Code to manage your proxy and access external tools.

## Azure OpenAI

Azure uses deployment names instead of model names in the URL. VoidLLM handles the URL mapping.

```yaml
models:
  - name: gpt-4o-azure
    provider: azure
    base_url: https://mycompany.openai.azure.com
    api_key: ${AZURE_KEY}
    azure_deployment: my-gpt4o-deployment
    azure_api_version: "2024-10-21"
```

The `azure_deployment` is your Azure deployment name (not the model name). VoidLLM constructs the URL as `{base_url}/openai/deployments/{deployment}/chat/completions?api-version={version}`.

Azure uses the `api-key` header instead of `Authorization: Bearer`. VoidLLM handles this automatically.

## Ollama

Local Ollama instances work as passthrough.

```yaml
models:
  - name: llama3
    provider: ollama
    base_url: http://localhost:11434/v1
    aliases: [local]
```

No API key needed for local Ollama.

## vLLM

Self-hosted vLLM instances are OpenAI-compatible.

```yaml
models:
  - name: llama-70b
    provider: vllm
    base_url: http://vllm-large:8000/v1
    aliases: [large]
```

No API key needed - vLLM doesn't have built-in auth. Use network policies to restrict access to the vLLM endpoint.

## Custom (any OpenAI-compatible endpoint)

For any endpoint that speaks the OpenAI API format.

```yaml
models:
  - name: my-model
    provider: custom
    base_url: https://my-provider.com/v1
    api_key: ${MY_KEY}
```

This works with any service that implements `/v1/chat/completions` in OpenAI format: Together AI, Fireworks, Groq, Replicate, local inference servers, etc.

## Model Types

Models can be typed to distinguish their capabilities:

```yaml
models:
  - name: gpt-4o
    type: chat              # default
    provider: openai
    base_url: https://api.openai.com/v1

  - name: text-embedding-3
    type: embedding
    provider: openai
    base_url: https://api.openai.com/v1
```

Supported types: `chat` (default), `embedding`, `completion`, `image`, `audio_transcription`, `tts`.

The type affects:
- Which models appear in the Playground (tabs per type)
- Health check probe format (embeddings use `/embeddings`, not `/chat/completions`)
- Create Model dialog in the UI

## Per-Model Timeout

Override the global timeout for specific models:

```yaml
models:
  - name: slow-model
    provider: custom
    base_url: https://slow-provider.com/v1
    timeout: 120s           # this model gets 2 minutes instead of the global default
```

## Upstream API Key Storage

Upstream provider API keys are encrypted at rest with AES-256-GCM. The encryption key is derived from `VOIDLLM_ENCRYPTION_KEY`. Keys are decrypted in memory only when needed for upstream requests.
