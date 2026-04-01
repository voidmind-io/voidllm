---
title: "Privacy Architecture"
description: "Zero-knowledge proxy design, GDPR, and data minimization"
section: security
order: 2
---
# Privacy Architecture

VoidLLM is a zero-knowledge proxy. It never stores, logs, or persists any prompt or response content. This is not a configurable option - it is an architecture decision.

## What VoidLLM stores

| Data | Stored | Purpose |
|---|---|---|
| API key hash | Yes | Authentication (HMAC-SHA256, not reversible) |
| Upstream API keys | Yes | Encrypted at rest (AES-256-GCM) |
| User accounts | Yes | Email, display name, password hash |
| Usage events | Yes | Who, when, which model, token counts, cost, duration |
| MCP tool call logs | Yes | Server, tool name, duration, status |
| Audit logs | Yes (Enterprise) | Admin action metadata |

## What VoidLLM never stores

| Data | Stored | Why |
|---|---|---|
| Prompt content | Never | Not even temporarily - streams through memory |
| Response content | Never | Same - passes through and is forgotten |
| System prompts | Never | Not inspected |
| Function call content | Never | Proxied as opaque bytes |
| Images / files | Never | Proxied without inspection |

## How it works

The proxy reads exactly one field from the request body: `model`. This is used for routing. Everything else passes through as an opaque byte stream:

1. Request arrives with `Authorization: Bearer vl_uk_...`
2. VoidLLM validates the key (HMAC-SHA256 hash lookup in memory)
3. Reads the `model` field for routing
4. Rewrites the model name if using aliases
5. Forwards the request body to the upstream provider
6. Streams the response back to the client
7. Extracts token counts from the response header/body (metadata only)
8. Emits a usage event asynchronously (fire-and-forget, never blocks the response)

Content passes through process memory during streaming but is never written to disk, database, or log files.

## GDPR

The zero-knowledge architecture significantly reduces the GDPR compliance surface area:

- No personal data from prompts is processed or stored by VoidLLM
- Usage metadata (API key ID, org, team, model, tokens) may qualify as personal data if linkable to an individual
- VoidLLM acts as a data processor for usage metadata; you (the deployer) are the data controller
- Data processing agreements with upstream LLM providers (for prompt/response content) are between you and the provider - VoidLLM is not a party

## Data Minimization

VoidLLM implements data minimization (GDPR Article 5(1)(c)) and Privacy by Design (Article 25) by architecture:

- Only the minimum data needed for routing and usage tracking is collected
- Content data is never collected because there is no code path to collect it
- Usage events are the smallest possible record: a few integers and strings per request

## Comparison

| | VoidLLM | Typical proxy |
|---|---|---|
| Prompt logging | Never (no code path) | Usually default, opt-out |
| GDPR scope at proxy | Metadata only | Metadata + content |
| DPA complexity | Minimal | Significant |
| Data breach impact at proxy | Usage metadata | Potentially all prompts |
| Right to deletion scope | API key + usage records | All of the above + prompt content |
