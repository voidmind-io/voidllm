# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in VoidLLM, **please do not open a public issue.**

Instead, report it privately:

- **Email:** security@voidmind.io
- **Subject:** `[VoidLLM Security] <brief description>`

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

## Response Timeline

- **Acknowledgment:** within 48 hours
- **Initial assessment:** within 5 business days
- **Fix timeline:** depends on severity, typically within 14 days for critical issues

## Scope

The following are in scope:
- Authentication bypass (API key validation, RBAC enforcement)
- Authorization issues (cross-org/cross-team data access)
- SQL injection
- SSRF via upstream URL manipulation
- Header injection or leakage
- Encryption key handling (AES-256-GCM, HMAC-SHA256)
- Information disclosure (secrets in logs, error messages, responses)
- Rate limit bypass

The following are out of scope:
- Vulnerabilities in upstream LLM providers
- Denial of service via legitimate high traffic
- Social engineering

## Security Design Principles

VoidLLM follows these security principles by design:

- **Zero-knowledge proxy** — no prompt or response content is ever stored or logged
- **Defense in depth** — RBAC enforced at both route middleware and handler level
- **Allowlist, not blocklist** — header forwarding uses explicit allowlists
- **Constant-time comparison** — API key verification uses `hmac.Equal`
- **No redirect following** — prevents SSRF via HTTP redirects
- **Encryption at rest** — upstream API keys encrypted with AES-256-GCM
