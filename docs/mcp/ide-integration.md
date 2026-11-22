---
title: "IDE Integration"
description: "Connect Claude Code, Cursor, and Windsurf to VoidLLM"
section: mcp
order: 4
---
# IDE Integration

Connect AI coding assistants to VoidLLM - as an LLM proxy, an MCP server, or both.

## As LLM Proxy

VoidLLM exposes an OpenAI-compatible `/v1` endpoint. A tool that lets you set a custom OpenAI base URL and API key can use it.

### Cursor and Windsurf

Not verified. We have not tested either tool against VoidLLM. The current Cursor docs describe adding your own OpenAI, Anthropic, Google, Azure and AWS Bedrock keys and do not describe a custom base URL; they also say requests made with your own key are routed through Cursor's servers. Windsurf's documentation has moved to the Devin docs and we could not confirm a custom provider setting there. If your version has a custom OpenAI base URL setting, the values would be:

```
Base URL: https://your-voidllm/v1
API Key: vl_uk_...
```

### Claude Code

Claude Code uses the Anthropic API format, not OpenAI. It cannot use VoidLLM as an LLM proxy directly. But it works as an MCP server (see below).

## As MCP Server

VoidLLM serves MCP over Streamable HTTP. Each client has its own config format, so the snippets below differ. The endpoints are:

| Endpoint | Gives you |
|---|---|
| `/api/v1/mcp/voidllm` | Built-in management tools (list models, check health, create keys, etc.) |
| `/api/v1/mcp` | Code Mode tools (`list_servers`, `search_tools`, `execute_code`), when Code Mode is enabled |
| `/api/v1/mcp/<alias>` | One registered MCP server |

### Claude Code

Claude Code requires `"type": "http"` for a remote server. An entry with a `url` and no `type` is skipped. Put this in a project `.mcp.json`, or add it with `claude mcp add --transport http`:

```json
{
  "mcpServers": {
    "voidllm": {
      "type": "http",
      "url": "https://your-voidllm/api/v1/mcp/voidllm",
      "headers": {
        "Authorization": "Bearer vl_uk_..."
      }
    }
  }
}
```

See the [Claude Code MCP docs](https://code.claude.com/docs/en/mcp.md).

### Cursor

Cursor reads `url` and `headers` from `.cursor/mcp.json` (project) or `~/.cursor/mcp.json` (global), see the [Cursor MCP docs](https://cursor.com/help/customization/mcp.md):

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

### Windsurf

Not verified. Windsurf's MCP documentation has moved to the Devin docs, which describe a different config location (`.devin/mcp_config.json`) and do not show a remote server entry we could confirm. Check the current Windsurf or Devin documentation for the remote server format, and use the URL and `Authorization` header from the examples above.

### Other endpoints

Use the same entry with a different URL. For Code Mode use `https://your-voidllm/api/v1/mcp` (name the server `voidllm-code`), and for a registered server `https://your-voidllm/api/v1/mcp/aws` (replace `aws` with its alias).

The advantage: VoidLLM handles upstream auth, enforces access control, and logs every tool call. Your IDE only needs one credential - your VoidLLM API key.

## Copy from UI

The MCP Servers page has a copy button on every server. Click the chevron on any server row to see it. The generated JSON has `url` and `headers`, which is the Cursor format. For Claude Code add `"type": "http"` to the entry.

## Config File Locations

| IDE | Config file |
|---|---|
| Claude Code | project `.mcp.json`, or `~/.claude.json` through `claude mcp add --scope user` |
| Cursor | `.cursor/mcp.json` or `~/.cursor/mcp.json` |
| Windsurf | Not verified, see above |

## Troubleshooting

**401 Unauthorized** - your API key is wrong or expired.

**Model not found** - the model name or alias doesn't exist in VoidLLM.

**MCP server access denied** - global MCP servers are closed by default. An org admin needs to grant access in Organization -> MCP Servers.

**Connection refused** - VoidLLM isn't reachable. Check firewall, ports, and that VoidLLM is running.
