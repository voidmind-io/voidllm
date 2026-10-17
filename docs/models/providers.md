---
title: "Provider Setup"
description: "Configure OpenAI, Anthropic, Azure, AWS Bedrock, Ollama, vLLM, and custom providers"
section: models
order: 1
---
# Provider Setup

VoidLLM supports 7 providers. Each handles request/response translation differently.

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

## AWS Bedrock

VoidLLM translates OpenAI chat completion requests into the Bedrock Converse API (`/converse` and `/converse-stream`) and translates the responses back, including streaming and tool calling. Clients keep using the OpenAI format.

```yaml
models:
  - name: claude-sonnet-bedrock
    provider: bedrock
    region: eu-central-1
    bedrock_model_id: eu.anthropic.claude-sonnet-4-5-20250929-v1:0
    api_key: ${BEDROCK_API_KEY}
    aliases: [default]
```

Bedrock can also sit next to another provider as a failover deployment of the same model:

```yaml
models:
  - name: claude-sonnet
    strategy: priority
    deployments:
      - name: anthropic-direct
        provider: anthropic
        base_url: https://api.anthropic.com
        api_key: ${ANTHROPIC_KEY}
        priority: 1
      - name: bedrock-frankfurt
        provider: bedrock
        region: eu-central-1
        bedrock_model_id: eu.anthropic.claude-sonnet-4-5-20250929-v1:0
        api_key: ${BEDROCK_API_KEY}
        priority: 2
```

### Fields

| Field | Required | Description |
|---|---|---|
| `region` | yes | AWS region, e.g. `eu-central-1`, `us-gov-west-1`. Must look like a region name (`eu-central-1`); anything else, such as a value containing a dot or a slash, is rejected. |
| `bedrock_model_id` | yes | Base model ID, cross-region inference profile ID, or ARN. Up to 2048 characters from letters, digits, `.`, `_`, `:`, `/`, `-`. |
| `api_key` | yes in practice | Bedrock API key, sent as `Authorization: Bearer`. |
| `base_url` | no | Endpoint override. Empty means the regional endpoint (see below). Must start with `http://` or `https://`. |

`region` and `bedrock_model_id` are also available on deployments and in the Admin API (`region`, `bedrock_model_id`). Both are required even when `base_url` is set. Bedrock supports `chat` models only; any other `type` is rejected.

### Endpoint

With an empty `base_url`, VoidLLM derives the endpoint from the region at request time: `https://bedrock-runtime.<region>.amazonaws.com`. The derived URL is not stored, so changing `region` changes the endpoint. Set `base_url` to override it, for example:

- FIPS: `https://bedrock-runtime-fips.us-east-1.amazonaws.com`
- PrivateLink: `https://vpce-....bedrock-runtime.<region>.vpce.amazonaws.com`
- A partition or gateway outside `amazonaws.com`

The request path is always `/model/<model id>/converse` or `/model/<model id>/converse-stream`. The model ID is sent as one percent-encoded path segment (`:` becomes `%3A`, `/` becomes `%2F`), so ARNs and inference profile IDs work unchanged.

### Model IDs

- Base model: `anthropic.claude-sonnet-4-5-20250929-v1:0`
- Cross-region inference profile: `eu.anthropic.claude-sonnet-4-5-20250929-v1:0`, `us.` and `global.` prefixes work the same way
- ARN of an inference profile or foundation model

Many newer models can only be called through an inference profile, not by their base model ID.

### Authentication

Only Bedrock API keys (bearer tokens) are supported. SigV4 signing, IAM roles, the AWS credential chain, IRSA, and AssumeRole are not supported yet.

- Long-term API keys do not expire on their own (unless you set an expiry when creating them) and are the recommended choice for now.
- Short-term API keys expire within 12 hours at most. When one expires, requests fail with `expired_api_key` until you update the key.

Example IAM policy for the identity that owns the key:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AllowBearerToken",
      "Effect": "Allow",
      "Action": "bedrock:CallWithBearerToken",
      "Resource": "*"
    },
    {
      "Sid": "AllowConverse",
      "Effect": "Allow",
      "Action": [
        "bedrock:InvokeModel",
        "bedrock:InvokeModelWithResponseStream"
      ],
      "Resource": [
        "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
        "arn:aws:bedrock:eu-*::foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0"
      ]
    }
  ]
}
```

Notes:

- Converse needs `bedrock:InvokeModel`; ConverseStream needs `bedrock:InvokeModelWithResponseStream`. VoidLLM uses both.
- A cross-region inference profile needs the foundation-model ARN in every destination region the profile can route to, hence the `eu-*` wildcard above.
- Model access must be enabled for the model in your AWS account.
- A service control policy can deny `bedrock:CallWithBearerToken` (condition key `bedrock:bearerTokenType`). If every request returns `permission_denied`, check this first.

### Request translation

| OpenAI request | Behavior |
|---|---|
| `messages` | Required and non-empty. A conversation that contains only system messages is rejected. |
| `system` / `developer` messages | Become Converse `system` blocks, one per text part. Never joined. |
| `user`, `assistant` messages | Text parts become separate text blocks. Consecutive messages with the same role are merged into one message by appending blocks. |
| `tools`, `tool_calls`, `tool` messages | Supported. Tool names and tool call IDs must match `[a-zA-Z0-9_-]{1,64}`. Tool parameters must be a JSON object. A tool message with empty content is sent as an empty JSON result. |
| `tool_choice` | `auto`, `required`, and a specific function are supported. `"none"` removes the tools from the request. `null` is treated as absent. |
| `parallel_tool_calls` | Must be a boolean if present. Accepted but has no effect. |
| `max_tokens`, `max_completion_tokens` | Must be a positive integer. Sent as `maxTokens`. No default is injected. `null` is treated as absent. |
| `temperature` | Negative values are rejected. Values above 1 are clamped to 1. |
| `top_p` | Must be between 0 and 1. |
| `stop` | Sent as `stopSequences`. |
| `stream`, `stream_options.include_usage` | Streaming uses ConverseStream. With `include_usage`, a final usage chunk is emitted. |
| `cache_control` on a text part | Adds a Converse `cachePoint` after that part. `ttl: "1h"` is sent as a one-hour cache point; `"5m"` or no ttl uses the default lifetime. Support depends on the model. A `cache_control` on a tool result part gets no cache point. |
| Top-level `system`, `stop_sequences` | Rejected. Use a `system` message and `stop`. |

Only text content parts are supported. `image_url`, `input_audio`, and `file` parts are rejected.

Everything else is dropped silently, including `n`, `response_format`, `user`, `metadata`, `logprobs`, `top_logprobs`, `seed`, `top_k`, `reasoning_effort`, `functions`, `function_call`, and any native Bedrock field such as `inferenceConfig`, `toolConfig`, `guardrailConfig`, or `additionalModelRequestFields`. Request bodies are rebuilt from a fixed set of fields, so nothing from the client is forwarded verbatim.

### Response translation

- Text blocks are joined into `message.content`. `toolUse` blocks become `tool_calls`; `content` is `null` when only tool calls are returned.
- Reasoning blocks and other unknown block types are dropped.
- `stopReason` maps to `finish_reason`: `end_turn` and `stop_sequence` to `stop`, `max_tokens` and `model_context_window_exceeded` to `length`, `tool_use` to `tool_calls`, `guardrail_intervened` and `content_filtered` to `content_filter`. Unknown values map to `stop`.
- Bedrock reports `inputTokens` without cached tokens. VoidLLM reports `prompt_tokens` as input plus cache read plus cache write tokens, and `total_tokens` as prompt plus completion. Cache counts appear in `prompt_tokens_details`.
- The response `id` is generated by VoidLLM and `model` is the model name the client sent.

Streaming responses from Bedrock use the binary AWS event-stream format. VoidLLM decodes it and sends standard OpenAI SSE to the client without buffering the whole response. A corrupt or truncated stream, or an exception event inside the stream, ends the stream with an error instead of passing partial data on.

### Errors

Bedrock error responses are mapped to the standard VoidLLM error shape. The HTTP status code from Bedrock is kept, so retry, cooldown, and failover decisions are unchanged.

| Bedrock exception | Error code | Message |
|---|---|---|
| `ValidationException` | `invalid_request` | Bedrock rejected the request as invalid |
| `UnrecognizedClientException`, `InvalidSignatureException`, other 401 | `invalid_api_key` | upstream rejected the Bedrock API key |
| `ExpiredTokenException` | `expired_api_key` | Bedrock API key has expired |
| `AccessDeniedException` with an authentication failure (an invalid or unknown API key returns 403) | `invalid_api_key` | upstream rejected the Bedrock API key |
| `AccessDeniedException`, other 403 | `permission_denied` | Bedrock API key is not authorized for this model |
| `ValidationException` reporting that the account has no Bedrock model access | `permission_denied` | Bedrock model access is not enabled for this AWS account |
| `ResourceNotFoundException` | `model_not_found` | Bedrock model or inference profile not found |
| `ThrottlingException`, other 429 | `rate_limit_exceeded` | Bedrock rate limit exceeded |
| `ServiceQuotaExceededException` | `quota_exceeded` | Bedrock service quota exceeded |
| `ModelNotReadyException` | `model_not_ready` | Bedrock model is not ready |
| `ModelTimeoutException` | `upstream_timeout` | Bedrock model timed out |
| `ModelErrorException` | `upstream_model_error` | Bedrock model returned an error |
| `InternalServerException` | `upstream_error` | Bedrock internal error |
| `ServiceUnavailableException` | `upstream_unavailable` | Bedrock service unavailable |
| Anything else | by status: 400 `invalid_request`, 404 `not_found`, otherwise `upstream_error` | Fixed message |

No Bedrock message text is forwarded, logged, or embedded in the response. The upstream message is only checked against fixed phrases to tell the cases above apart, and every client-facing message is fixed text, because Bedrock errors can contain account IDs and principal ARNs.

### Health checks

The chat health probe sends a Converse request with a single user message and `maxTokens: 1`, so each probe is a real, billable model invocation (one output token). Models-list and embeddings probes do not apply to Bedrock. Keep this in mind when you set a short health check interval.

### PII anonymization

Bedrock endpoints are public hosts, so when [PII anonymization](../security/pii.md) is enabled they are treated as external providers and requests are pseudonymized by default. The decision is made from the endpoint host without a DNS lookup: named hosts (including PrivateLink `vpce-...` names) count as public, only literal private IP addresses and `localhost` count as private.

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
