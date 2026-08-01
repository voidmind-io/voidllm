---
title: "Security Hardening"
description: "Container security, TLS, network policies, and checklist"
section: security
order: 3
---
# Security Hardening

## Container Security

The Helm chart and Docker image include production security defaults:

- Non-root container user (`voidllm:voidllm`, UID 1000)
- Read-only root filesystem
- All Linux capabilities dropped
- No privilege escalation
- Resource limits configured

## Network

- **Separate admin port** - isolate admin traffic from proxy traffic:
  ```yaml
  server:
    admin:
      port: 8443
  ```
- **TLS on admin port** - for encrypted admin API access:
  ```yaml
  server:
    admin:
      tls:
        enabled: true
        cert: /certs/tls.crt
        key: /certs/tls.key
  ```
- **Network policies** - restrict pod-to-pod traffic in Kubernetes
- **SSRF protection** - VoidLLM blocks connections to private/loopback IPs by default (configurable via `settings.mcp.allow_private_urls`)
- **MCP Origin validation** - every MCP endpoint validates the `Origin` header against DNS-rebinding attacks, per the MCP Streamable HTTP spec. Requests without an `Origin` header (CLIs, SDKs, service-to-service calls) are unaffected. This is explicit-allow: with no allowlist configured, only the built-in localhost origins (`localhost`, `127.0.0.1`, `[::1]`, any port, `http` or `https`) are accepted — an `Origin` is deliberately **not** compared against the request's own `Host` header, because that comparison cannot detect DNS rebinding at all. In that attack the attacker controls the DNS record itself (it first resolves to their own server, then rebinds to `127.0.0.1`), so both the `Origin` and `Host` headers the browser sends are derived from the same attacker-controlled URL and always agree — comparing one attacker-supplied value against another proves nothing. If you serve VoidLLM under a real domain and access the MCP endpoints from a browser, you must list that domain in `settings.mcp.allowed_origins`; VoidLLM logs a WARN at startup when the MCP gateway is active and the allowlist is empty, since that is very likely not what you want in production. VoidLLM has no host-restricted listen option (it always binds every interface), so this warning cannot be conditioned on whether the deployment is "really" loopback-only.
- **Playground tunnel on the admin port** - in dual-port mode the admin port also serves the embedded dashboard's Playground tunnel, which carries streaming LLM responses. Fiber v3 has no per-route timeout: read/write timeouts and body limit are per-app, so raising them for the tunnel raises them for every route on the admin app, including unauthenticated ones (`POST /api/v1/auth/login`, `POST /api/v1/invites/redeem`, `GET /api/v1/auth/providers`, `GET /api/v1/invites/peek`). This is a real, bounded weakening versus a pure control-plane app, not a neutral change: those endpoints can inherit up to a 120s read/write timeout instead of the 30s VoidLLM explicitly configures for the admin app (Fiber itself defaults to no read/write timeout at all), and up to an 8 MiB body limit instead of Fiber's 4 MiB default. The cap only bounds how far this can go - it does not eliminate the exposure. Don't expose the admin port to untrusted networks, and put a reverse proxy with its own connection timeouts in front of it.

## API Keys

- Keys are stored as HMAC-SHA256 hashes - the plaintext is shown once at creation and cannot be retrieved
- Upstream provider API keys are encrypted at rest with AES-256-GCM
- Key rotation with 24-hour grace period (old key works for 24h after rotation)
- Session keys expire after 24 hours

## Encryption Key

The `VOIDLLM_ENCRYPTION_KEY` is critical:
- Used to encrypt upstream API keys in the database
- Used to derive the HMAC secret for API key validation
- Changing it invalidates all stored upstream keys and API key hashes
- Keep it in a secrets manager, not in plain text config files

## Headers

- VoidLLM uses explicit allowlists for header forwarding - only known-safe headers are proxied to upstream providers
- `Authorization` headers are rewritten per-provider (Bearer for OpenAI, api-key for Azure)
- No redirect following - prevents SSRF via HTTP redirects

## Checklist

- [ ] Use a strong `VOIDLLM_ENCRYPTION_KEY` (min 32 characters or base64-encoded 32 bytes)
- [ ] Don't use `VOIDLLM_ADMIN_KEY` as a production API key (it's for bootstrap only)
- [ ] Separate admin port from proxy port in production
- [ ] Enable TLS on admin port or terminate TLS at the reverse proxy
- [ ] Set resource limits in Kubernetes
- [ ] Use network policies to restrict access
- [ ] Rotate API keys regularly
- [ ] Monitor `/metrics` for unusual patterns
- [ ] Report vulnerabilities to security@voidmind.io
