---
title: "IDE Integration"
description: "Connect Claude Code, Cursor, and Windsurf to VoidLLM"
section: mcp
order: 4
---
# IDE Integration

Connect AI coding assistants to VoidLLM - as an LLM proxy, an MCP server, or both.

## As LLM Proxy

VoidLLM exposes an OpenAI-compatible `/v1` endpoint. Any tool that lets you set a custom OpenAI base URL works.

### Cursor

Settings -> Models -> OpenAI API Base:
```
https://your-voidllm/v1
```
API Key: `vl_uk_...`

### Windsurf

Settings -> Custom Provider:
```
Base URL: https://your-voidllm/v1
API Key: vl_uk_...
```

### Claude Code

Claude Code uses the Anthropic API format, not OpenAI. It cannot use VoidLLM as an LLM proxy directly. But it works great as an MCP server (see below).

## As MCP Server

### Management Tools

Access VoidLLM's built-in management tools (list models, check health, create keys, etc.):

```json
{
  "mcpServers": {
    "voidllm": {
      "url": "https://your-voidllm/api/v1/mcp/voidllm",
      "headers": {
        "Authorization": "Bearer vl_uk_..."
      }
    }
  }
}
```

### Code Mode

Access Code Mode tools (list_servers, search_tools, execute_code) for multi-tool orchestration:

```json
{
  "mcpServers": {
    "voidllm-code": {
      "url": "https://your-voidllm/api/v1/mcp",
      "headers": {
        "Authorization": "Bearer vl_uk_..."
      }
    }
  }
}
```

### External MCP Servers

Each registered MCP server gets its own endpoint. Connect to a specific server:

```json
{
  "mcpServers": {
    "aws": {
      "url": "https://your-voidllm/api/v1/mcp/aws",
      "headers": {
        "Authorization": "Bearer vl_uk_..."
      }
    }
  }
}
```

The advantage: VoidLLM handles upstream auth, enforces access control, and logs every tool call. Your IDE only needs one credential - your VoidLLM API key.

## Copy from UI

The MCP Servers page has a copy button on every server that generates the exact JSON config for your IDE. Click the chevron on any server row to see it.

## Config File Locations

| IDE | Config file |
|---|---|
| Claude Code | `~/.claude/mcp.json` or project `.mcp.json` |
| Cursor | Cursor Settings -> MCP |
| Windsurf | Windsurf Settings -> MCP |

## Troubleshooting

**401 Unauthorized** - your API key is wrong or expired.

**Model not found** - the model name or alias doesn't exist in VoidLLM.

**MCP server access denied** - global MCP servers are closed by default. An org admin needs to grant access in Organization -> MCP Servers.

**Connection refused** - VoidLLM isn't reachable. Check firewall, ports, and that VoidLLM is running.
