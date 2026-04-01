---
title: "SSO / OIDC"
description: "Configure Google, Azure AD, Okta, or any OIDC provider"
section: enterprise
order: 2
---
# SSO / OIDC

VoidLLM supports any OpenID Connect provider - Google Workspace, Azure AD, Okta, Auth0, Keycloak, OneLogin, and more. No provider-specific code needed. Requires an Enterprise license.

## Global Configuration (YAML)

For DevOps-managed deployments, configure SSO in `voidllm.yaml`:

```yaml
settings:
  sso:
    enabled: true
    issuer: "https://accounts.google.com"
    client_id: ${VOIDLLM_SSO_CLIENT_ID}
    client_secret: ${VOIDLLM_SSO_CLIENT_SECRET}
    redirect_url: "https://voidllm.company.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "email", "profile"]
    allowed_domains: ["company.com"]
    auto_provision: true
    default_role: "member"
    group_sync: true
    group_claim: "groups"
```

## Per-Org Configuration (UI)

Each organization can configure its own Identity Provider through the UI:

1. Navigate to **Organizations -> [Org Name] -> SSO** tab
2. Enter your IdP's issuer URL, client ID, and client secret
3. Click **Test Connection** to validate
4. Save

Per-org config overrides the global YAML config for that organization. This allows multi-tenant deployments where each customer uses their own IdP.

## Mixed Authentication

SSO and local accounts work side by side. When OIDC is enabled, the login page shows both the email/password form and a "Sign in with SSO" button.

- Admins can still create local users or send invites
- Local users log in with email + password
- SSO users log in via the Identity Provider (no local password)
- Both types coexist in the same organization

## Auto-Provisioning

When enabled, users from allowed email domains are automatically created on first SSO login:

- They receive the configured `default_role` (typically "member")
- They are added to the default organization
- No admin action required - just share the VoidLLM URL

## Group Sync

When enabled, OIDC groups from the `groups` claim are mapped to VoidLLM teams:

- Teams are created automatically if they don't exist
- Users are added as members to matching teams on each login
- Group membership is updated on every login (additive - users are never removed from teams via sync)

Configure the claim name if your IdP uses a different field:

```yaml
settings:
  sso:
    group_claim: "groups"   # default
```

## Setup Guide: Google Workspace

1. Go to [Google Cloud Console](https://console.cloud.google.com) -> APIs & Services -> Credentials
2. Create an OAuth 2.0 Client ID (Web application)
3. Set the redirect URI to: `https://your-voidllm.com/api/v1/auth/oidc/callback`
4. Copy the Client ID and Client Secret
5. Configure VoidLLM:

```yaml
settings:
  sso:
    enabled: true
    issuer: "https://accounts.google.com"
    client_id: ${VOIDLLM_SSO_CLIENT_ID}
    client_secret: ${VOIDLLM_SSO_CLIENT_SECRET}
    redirect_url: "https://your-voidllm.com/api/v1/auth/oidc/callback"
    allowed_domains: ["yourcompany.com"]
```

## Setup Guide: Azure AD

1. Go to Azure Portal -> Azure Active Directory -> App Registrations
2. Register a new application
3. Set the redirect URI to: `https://your-voidllm.com/api/v1/auth/oidc/callback`
4. Create a client secret under Certificates & Secrets
5. Configure VoidLLM:

```yaml
settings:
  sso:
    enabled: true
    issuer: "https://login.microsoftonline.com/{tenant-id}/v2.0"
    client_id: ${VOIDLLM_SSO_CLIENT_ID}
    client_secret: ${VOIDLLM_SSO_CLIENT_SECRET}
    redirect_url: "https://your-voidllm.com/api/v1/auth/oidc/callback"
```

Replace `{tenant-id}` with your Azure AD tenant ID.

## Troubleshooting

**"Invalid redirect URI"** - the redirect URL in VoidLLM config must exactly match what you registered with the IdP, including the protocol (https) and path.

**"User not provisioned"** - check that `auto_provision: true` is set and the user's email domain is in `allowed_domains`.

**"Groups not syncing"** - verify the IdP includes the `groups` claim in the token. Some providers require explicit configuration to include group membership in OIDC tokens.
