---
title: "License Activation"
description: "Activate, verify, and manage VoidLLM Enterprise licenses"
section: enterprise
order: 1
---
# License Activation

VoidLLM Enterprise features are gated by a license key - a signed JWT (Ed25519). The same binary serves all tiers. Enterprise features are simply unlocked when a valid license is present.

## Obtain a License

Purchase a Pro or Enterprise subscription at [voidllm.ai](https://voidllm.ai/#pricing). You'll receive a license key - a signed JWT that encodes your plan, features, and limits.

## Activate

Three ways to provide the license:

**Environment variable (recommended for Docker/K8s):**

```bash
export VOIDLLM_LICENSE="eyJhbGciOiJFZERTQSIs..."
```

**Config file:**

```yaml
settings:
  license: ${VOIDLLM_LICENSE}
```

**License file:**

```yaml
settings:
  license_file: /etc/voidllm/license.jwt
```

In Kubernetes, store the license in a Secret via the Helm chart:

```bash
helm install voidllm voidllm/voidllm \
  --set secrets.license="eyJhbGciOiJFZERTQSIs..."
```

## Verification

The license is verified locally at startup - no network call required. VoidLLM also performs a daily background heartbeat to `license.voidllm.ai` to check subscription status (renewal, cancellation, payment issues). If the heartbeat fails (e.g., air-gapped deployment), VoidLLM continues operating with the last known license state.

If the license is close to expiry (< 7 days), the heartbeat automatically requests a refreshed JWT and persists it to the database. This means container restarts don't require re-supplying the license after the initial activation.

If the license is invalid or expired, VoidLLM falls back to Community mode - the proxy keeps running, enterprise features are simply disabled. No downtime, no data loss.

**CLI verification:**

```bash
voidllm license verify < license.jwt
```

**UI:** System -> License shows your current plan, features, and expiry date.

## What Each Plan Unlocks

| Feature | Community | Pro | Enterprise |
|---|---|---|---|
| Unlimited orgs + teams | 1 org, 3 teams | Yes | Yes |
| Cost reports | | Yes | Yes |
| SSO / OIDC | | | Yes |
| Audit logs | | | Yes |
| OpenTelemetry | | | Yes |
| Multi-instance (Redis) | | | Yes |

## Founding Member

A one-time $999 payment for lifetime Enterprise access - all current and future features, no recurring fees. Includes Product Advisory Board membership and direct founder access. [Learn more](https://voidllm.ai/#pricing).

## Graceful Degradation

VoidLLM never stops working because of a license issue:

- **Expired license:** Falls back to Community. Proxy keeps routing. Enterprise UI features show an upgrade prompt.
- **Invalid license:** Same as expired - Community mode.
- **Network unreachable:** Continues with last known license state. Logs a warning.
- **License revoked:** On next heartbeat, falls back to Community. Active requests are not interrupted.
