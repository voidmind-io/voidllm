---
title: "MCP Gateway Overview"
description: "What is the MCP gateway, built-in tools, and architecture"
section: mcp
order: 1
---
# MCP Gateway

VoidLLM is an [MCP](https://modelcontextprotocol.io) (Model Context Protocol) gateway. It exposes built-in management tools, proxies requests to external MCP servers, and provides access control, usage tracking, and session management - all through a single endpoint.

## Why a gateway?

Without a gateway, every AI agent (Claude Code, Cursor, etc.) connects directly to every MCP server. Each connection needs its own auth, its own config, and there's no visibility into what tools are being called.

With VoidLLM:
- **One endpoint** per client - VoidLLM handles routing and auth to upstream servers
- **Scoped access control** - grant access to MCP servers per org, per team, or globally
- **Tool call logging** - every call tracked with metadata (server, tool, duration, status)
- **Session management** - automatic initialization and session ID forwarding

## Built-in Tools

VoidLLM ships with 6 management tools on `/api/v1/mcp/voidllm`:

| Tool | Description | Min Role |
|---|---|---|
| `list_models` | Models with health status | member |
| `get_model_health` | Health for a specific model or deployment | member |
| `get_usage` | Usage stats for the caller's org | member |
| `list_keys` | API keys visible to the caller | member |
| `create_key` | Create a temporary API key | member |
| `list_deployments` | Deployment details | system_admin |

## External Servers

Register external MCP servers and proxy tool calls through VoidLLM. See [Server Setup](servers.md) for configuration.

## Code Mode

AI agents write JavaScript to orchestrate multiple tool calls in a single WASM-sandboxed execution. See [Code Mode](code-mode.md) for setup.

## IDE Integration

Connect Claude Code, Cursor, or Windsurf to VoidLLM's MCP endpoints. See [IDE Integration](ide-integration.md) for step-by-step instructions.

## Privacy

MCP tool calls follow the same zero-knowledge principle as the LLM proxy: VoidLLM logs metadata (server, tool name, duration, status) but never the tool call arguments or results. Tool call content passes through memory and is never written to disk.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/mcp/voidllm` | Built-in management tools |
| `GET /api/v1/mcp/voidllm` | SSE transport (legacy clients) |
| `POST /api/v1/mcp/:alias` | External MCP server proxy |
| `GET /api/v1/mcp/:alias` | SSE transport for external servers |
| `POST /api/v1/mcp` | Code Mode (list_servers, search_tools, execute_code) |
