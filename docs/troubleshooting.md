---
title: "Troubleshooting"
description: "Common issues and solutions for VoidLLM"
section: root
order: 3
---
# Troubleshooting

## Startup Issues

### "admin key must be at least 32 characters"
Your `VOIDLLM_ADMIN_KEY` is too short. Generate one:
```bash
export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
```

### "VOIDLLM_ADMIN_KEY is set but database already has keys, ignoring"
This is normal on subsequent starts. The admin key is only used on first boot to create the bootstrap user. After that, it's ignored.

### "VOIDLLM_ENCRYPTION_KEY" missing
The encryption key is required for all deployments. It encrypts upstream API keys in the database:
```bash
export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
```

### Can't find bootstrap credentials
VoidLLM prints credentials to stdout on first start only. Check container logs:
```bash
docker logs voidllm | grep "BOOTSTRAP"
kubectl logs deploy/voidllm | grep "BOOTSTRAP"
```

If you missed them, delete the database and restart to re-bootstrap.

## Proxy Issues

### 401 Unauthorized
- API key is wrong, expired, or revoked
- Check key format: must start with `vl_uk_`, `vl_tk_`, `vl_sa_`, or `vl_sk_`
- Session keys (`vl_sk_`) expire after 24 hours

### 404 Model not found
- The model name or alias doesn't exist in VoidLLM
- Check available models: `GET /api/v1/models` or the UI Models page
- If using aliases, make sure the alias is configured for the caller's org/team

### 502 Upstream unavailable
- The upstream LLM provider is unreachable
- Check the model's `base_url` in configuration
- Test connectivity: the model's health status on the Models page
- If using load balancing, check individual deployment health

### 429 Rate limit exceeded
- The caller has exceeded their rate limit (RPM/RPD) or token budget
- Check limits on the key, team, and org level
- Most-restrictive-wins: the tightest limit anywhere in the hierarchy applies

### Streaming responses cut off
- Reverse proxy may be buffering responses - set `proxy_buffering off` in Nginx
- Upstream timeout may be too short - increase `write_timeout` in server config
- Check per-model timeout if set

## MCP Issues

### "access denied to MCP server"
Global MCP servers are closed by default. An org admin must grant access:
- UI: Organization -> MCP Servers tab -> toggle access
- API: `PUT /api/v1/orgs/{org_id}/mcp-access`

System admins bypass access checks.

### MCP tools not showing in Code Mode
- The server may not have `code_mode_enabled: true`
- Tools may not be cached yet - click "Refresh Tools" on the MCP Servers page
- Check if tools are blocked via the per-tool blocklist

### "server uses deprecated SSE transport"
VoidLLM does not support the pre-2025-03-26 SSE MCP transport. The upstream server needs to support Streamable HTTP (the current MCP spec).

## UI Issues

### Can't log in
- Check email and password (case-sensitive)
- Session keys expire after 24 hours - you may need to log in again
- If SSO is enabled, use the "Sign in with SSO" button instead

### Missing features in UI
- Some features require an Enterprise license (SSO, audit logs, OTel)
- Check System -> License for your current plan
- The UI shows upgrade prompts for locked features

## Database Issues

### SQLite "database is locked"
- Only one VoidLLM instance can write to SQLite at a time
- For multi-instance deployments, use PostgreSQL
- Check that no other process is accessing the database file

### PostgreSQL connection errors
- Verify DSN format: `postgres://user:pass@host:5432/dbname?sslmode=require`
- Check network connectivity between VoidLLM and PostgreSQL
- Verify credentials and database permissions

## Performance

### High latency
- Check the `/metrics` endpoint for proxy latency percentiles
- VoidLLM adds < 500us overhead - if latency is high, the upstream provider is slow
- Check circuit breaker status on the Models page - a tripped breaker adds retry latency

### High memory usage
- Check for large request/response bodies (VoidLLM buffers bodies in memory during proxying)
- If using Code Mode, reduce `pool_size` to limit WASM runtime memory

## Getting Help

- [GitHub Issues](https://github.com/voidmind-io/voidllm/issues) - bug reports and feature requests
- [Security](mailto:security@voidmind.io) - vulnerability reports
- [Contact](mailto:hello@voidmind.io) - general inquiries
