---
title: "License Activation"
description: "Activate, verify, and manage VoidLLM Enterprise licenses"
section: enterprise
order: 1
---
# License Activation

VoidLLM Enterprise features are gated by a license key - a signed JWT (Ed25519). The same binary serves all tiers. Enterprise features are simply unlocked when a valid license is present.

## Obtain a License

Purchase a Pro or Enterprise subscription at [voidllm.ai](https://voidllm.ai/#pricing). You'll receive a license key - a signed JWT that encodes your plan, features, and limits. Prices: Pro €99/month, Enterprise €399/month, Enterprise Plus from about €1,000/month billed annually (contact us). To evaluate first, see [30-day trial](#30-day-trial).

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

## 30-day trial

Try the paid features for 30 days, free and without payment data.

**How to request:** as a system admin open System -> License in the UI and fill in the trial form, or go to [voidllm.ai/trial](https://voidllm.ai/trial). The key is emailed to you. Paste it into System -> License to activate it; no restart is needed.

**What is sent:** nothing leaves your instance until you submit the form. The request contains your email, an optional company name, your consent choices, the instance ID, and the product version. No prompts, responses, or usage data are included, and no payment data is collected.

**What happens after 30 days:** the key stops working. An hourly check switches the running instance back to Community mode, and the License page shows that the license has expired. If audit logs, SSO, or OpenTelemetry were enabled at startup, they stay active until you restart VoidLLM, and the page tells you so. The proxy keeps running throughout.

**Heartbeat:** activating a key in the UI also starts the daily license heartbeat, so you do not need to restart for it.

## Past offers

Founding Member licenses (a one-time payment for lifetime Enterprise access) are no longer offered. Existing Founding Member licenses keep working.

## Graceful Degradation

VoidLLM never stops working because of a license issue:

- **Expired license:** Falls back to Community. Proxy keeps routing. Enterprise UI features show an upgrade prompt.
- **Invalid license:** Same as expired - Community mode.
- **Network unreachable:** Continues with last known license state. Logs a warning.
- **License revoked:** On next heartbeat, falls back to Community. Active requests are not interrupted.
