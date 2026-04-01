---
title: "Code Mode"
description: "WASM-sandboxed JavaScript execution for multi-tool orchestration"
section: mcp
order: 3
---
# Code Mode

Code Mode lets AI agents write JavaScript to orchestrate multiple MCP tool calls in a single execution. Instead of the agent calling tools one at a time (N round-trips), it writes a script that calls all the tools it needs and VoidLLM executes it in a WASM sandbox.

## Enable

```yaml
settings:
  mcp:
    code_mode:
      enabled: true
      memory_limit_mb: 16     # max memory per execution (default: 16)
      timeout: 30s            # execution timeout (default: 30s)
      pool_size: 8            # pre-warmed WASM runtimes (default: 8)
      max_tool_calls: 50      # max tool calls per execution (default: 50)
```

## How It Works

1. The AI agent receives a task that requires multiple tool calls
2. Instead of calling tools individually, it writes JavaScript via `execute_code`
3. VoidLLM executes the script in a QuickJS WASM sandbox (via Wazero, pure Go)
4. Tool calls inside the script go through a Go host function bridge - the WASM sandbox has no direct network access
5. Results are returned to the agent in a single response

## Three Tools

Code Mode exposes three tools on `/api/v1/mcp`:

| Tool | Description |
|---|---|
| `list_servers` | Available MCP servers with tool counts |
| `search_tools` | Find tools by keyword across servers |
| `execute_code` | Run JavaScript with MCP tools as async functions |

## Example Script

```javascript
// Fetch data from two different MCP servers
const docs = await tools.aws.search({ query: "IAM best practices" })
const related = await tools.exa.search({ query: docs[0].title })

console.log(`Found ${docs.length} docs and ${related.length} related results`)
return { docs: docs.slice(0, 3), related: related.slice(0, 5) }
```

Tools are available as `tools.<server_alias>.<tool_name>(args)`. They're async functions - use `await`.

`console.log/warn/error` output is captured and returned alongside the result.

## TypeScript Types

VoidLLM auto-generates TypeScript declarations from tool schemas and injects them at discovery time (`tools/list`). The AI agent sees typed function signatures with parameter names and return shapes before writing any code.

## Sandbox Restrictions

The WASM sandbox enforces:
- No filesystem access
- No network access (tool calls go through the Go host function, not HTTP)
- No `eval()` or `new Function()`
- Configurable memory limit (default 16MB)
- Configurable execution timeout (default 30s)
- Configurable max tool calls (default 50)

## Per-Tool Blocklist

Not every tool should be available in Code Mode. Admins can block specific tools per server - blocked tools are invisible to Code Mode's `search_tools` and `execute_code` but still work through the regular MCP proxy.

See [MCP Server Setup](servers.md#tool-blocklist) for configuration.

## Performance

A pool of pre-initialized WASM runtimes eliminates cold start:
- **Warm eval:** ~32 microseconds
- **Pure JS execution:** ~3.3ms (empty script)
- **With tool call:** ~3.4ms + upstream response time
- **Pool cycle:** ~1.8ms (return runtime to pool + get fresh one)

The bottleneck is always the upstream MCP server response time, not the sandbox.

## IDE Setup

Point your IDE at `/api/v1/mcp` to get Code Mode tools:

```json
{
  "mcpServers": {
    "voidllm-code": {
      "url": "https://your-voidllm/api/v1/mcp",
      "headers": {
        "Authorization": "Bearer <your-api-key>"
      }
    }
  }
}
```

See [IDE Integration](ide-integration.md) for detailed setup per IDE.
