# Enterprise Guide

VoidLLM Enterprise adds SSO, audit logging, OpenTelemetry tracing, and advanced
cost reporting to the open-source core. All enterprise features are gated by a
license key — the same binary, just unlocked.

## License Activation

### Obtain a License

Purchase a Pro or Enterprise subscription at [voidllm.ai/pricing](https://voidllm.ai/pricing).
You'll receive a license key — a signed JWT that encodes your plan, features, and limits.

### Activate

**Option A: Environment variable**

```bash
export VOIDLLM_LICENSE="eyJhbGciOiJFZERTQSIs..."
```

**Option B: Config file**

```yaml
settings:
  license: ${VOIDLLM_LICENSE}
```

**Option C: License file**

```yaml
settings:
  license_file: /etc/voidllm/license.jwt
```

In Kubernetes, store the license in a Secret and mount it via the Helm chart:

```bash
helm install voidllm chart/voidllm/ \
  --set secrets.license="eyJhbGciOiJFZERTQSIs..."
```

### Verify

The license is verified locally at startup — no network call required.
VoidLLM also performs a daily background check against `license.voidllm.ai`
to validate subscription status (renewal, cancellation). If the check fails
(e.g., air-gapped deployment), VoidLLM continues operating with the last
known license state.

If the license is invalid or expired, VoidLLM falls back to Community mode —
the proxy keeps running, enterprise features are simply disabled.

You can also verify from the CLI:

```bash
voidllm license verify < license.jwt
```

Or check the UI: **System → License** shows your current plan, features, and expiry.

---

## SSO / OIDC

VoidLLM supports any OpenID Connect provider — Google Workspace, Azure AD, Okta,
Auth0, Keycloak, OneLogin, and more. No provider-specific code needed.

### Global Config (YAML)

For DevOps-managed deployments, configure SSO in `voidllm.yaml`:

```yaml
settings:
  sso:
    enabled: true
    issuer: "https://accounts.google.com"
    client_id: ${VOIDLLM_SSO_CLIENT_ID}
    client_secret: ${VOIDLLM_SSO_CLIENT_SECRET}
    redirect_url: "https://voidllm.company.com/api/v1/auth/oidc/callback"
    allowed_domains: ["company.com"]
    auto_provision: true
    default_role: "member"
    group_sync: true
    group_claim: "groups"
```

### Per-Org Config (UI)

Each organization can configure its own Identity Provider through the UI:

1. Navigate to **Organizations → [Org Name] → SSO** tab
2. Enter your IdP's issuer URL, client ID, and client secret
3. Click **Test Connection** to validate
4. Save

Per-org config overrides the global YAML config for that organization.

### Mixed Authentication

SSO and local accounts work side by side. When OIDC is enabled, the login page
shows both the email/password form and a "Sign in with SSO" button. Admins can
still create users manually or send invites — these users log in with a password.
SSO users log in via the Identity Provider and have no local password.

### User Onboarding with SSO

When SSO is active, there are three ways users can join:

1. **SSO auto-provisioning** — users are created automatically on first SSO login
   if their email domain is in `allowed_domains`
2. **Invite link + SSO** — admins send an invite, the recipient signs in via SSO
3. **Manual creation** — admins create local accounts (password-based) alongside SSO

See the [main documentation](deployment.md#user-onboarding) for the full
onboarding guide including non-SSO flows.

### Auto-Provisioning

When enabled, users from allowed email domains are automatically created on first
SSO login. They receive the configured default role and are added to the default org.

### Group Sync

When enabled, OIDC groups from the `groups` claim (configurable) are mapped to
VoidLLM teams. Teams are created automatically if they don't exist. Users are
added as members to matching teams on each login.

### Setup Guide (Google Workspace)

1. Go to [Google Cloud Console](https://console.cloud.google.com) → APIs & Services → Credentials
2. Create an OAuth 2.0 Client ID (Web application)
3. Set the redirect URI to: `https://your-voidllm.com/api/v1/auth/oidc/callback`
4. Copy the Client ID and Client Secret
5. Set `issuer: "https://accounts.google.com"` in your config

### Setup Guide (Azure AD)

1. Go to Azure Portal → Azure Active Directory → App Registrations
2. Register a new application
3. Set the redirect URI to: `https://your-voidllm.com/api/v1/auth/oidc/callback`
4. Create a client secret under Certificates & Secrets
5. Set `issuer: "https://login.microsoftonline.com/{tenant-id}/v2.0"` in your config

---

## Audit Logs

Every administrative action is automatically logged: user creation, team changes,
key rotation, model updates, SSO logins, and more.

### Enable

```yaml
settings:
  audit:
    enabled: true
```

### View

- **UI**: Navigate to **Security → Audit Log**
- **API**: `GET /api/v1/audit-logs?resource_type=key&action=create&limit=50`

### What's Logged

| Action | Resources |
|---|---|
| `create` | org, team, user, key, model, service_account, membership, invite |
| `update` | org, team, user, key, model, membership |
| `delete` | org, team, user, key, model, service_account, membership |
| `revoke` | key |
| `rotate` | key |
| `login` | session |
| `activate` / `deactivate` | model |

Each event records: who (actor), what (action + resource), when (timestamp),
the request body as JSON (what changed), client IP, and request ID for correlation.

**Privacy**: Audit logs contain only administrative metadata — never prompt or
response content.

---

## OpenTelemetry Tracing

Export distributed traces to Jaeger, Grafana Tempo, Datadog, or any OTLP-compatible backend.

### Enable

```yaml
settings:
  otel:
    enabled: true
    endpoint: "tempo:4317"
    insecure: true
    sample_rate: 1.0
```

### What's Traced

- `proxy.handle` — root span covering the entire proxy request lifecycle
- `proxy.upstream` — child span measuring time-to-first-byte from the LLM provider

Trace context (`traceparent` header) is propagated to upstream providers for
end-to-end distributed tracing.

### Log Correlation

When tracing is active, every log line automatically includes `trace_id` and
`span_id`, making it easy to find related logs for a specific trace in tools
like Grafana Loki or Elasticsearch.

---

## Cost Reports

Available on Pro and Enterprise plans. Provides cost visibility across models,
teams, and time periods.

### Features

- **Cost by Model** — breakdown with percentage of total spend
- **Daily Cost Trend** — day-over-day changes with trend indicators
- **Top Cost Drivers** — identify which models and teams drive cost
- **CSV Export** — download usage data for external analysis

Access via **Analytics → Cost Reports** in the UI.

---

## Pricing

| | Community | Pro | Enterprise |
|---|---|---|---|
| **Price** | Free | $299/mo | $799/mo |
| **Annual** | — | $2,990/yr | $7,990/yr |
| **License** | BSL 1.1 | Subscription | Subscription |
| **Use case** | Personal / evaluation | Teams & production | Organization-wide |
| Organizations | 1 | Unlimited | Unlimited |
| Teams per org | 3 | Unlimited | Unlimited |
| Users | Unlimited | Unlimited | Unlimited |
| Proxy + streaming + adapters | ✓ | ✓ | ✓ |
| Web UI + playground | ✓ | ✓ | ✓ |
| RBAC (4 built-in roles) | ✓ | ✓ | ✓ |
| Usage tracking + dashboard | ✓ | ✓ | ✓ |
| Rate limiting + token budgets | ✓ | ✓ | ✓ |
| Circuit breakers + per-model timeouts | ✓ | ✓ | ✓ |
| Prometheus metrics (14) | ✓ | ✓ | ✓ |
| Data retention | Unlimited | Unlimited | Unlimited |
| Usage export (CSV / JSON) | ✓ | ✓ | ✓ |
| Cost reports + budget alerts | | ✓ | ✓ |
| Multi-org management | | ✓ | ✓ |
| Cross-org analytics | | ✓ | ✓ |
| Priority email support (48h) | | ✓ | ✓ |
| SSO / OIDC (any provider) | | | ✓ |
| Per-org SSO configuration | | | ✓ |
| Audit logs (API + UI) | | | ✓ |
| OpenTelemetry tracing | | | ✓ |
| Multi-instance (Redis) | | | ✓ |
| Dedicated Slack support (24h) | | | ✓ |

Flat pricing — no per-user fees, no per-request charges, no per-token metering.
Self-hosted on your infrastructure. Annual billing saves 2 months.
