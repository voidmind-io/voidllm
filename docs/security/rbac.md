---
title: "RBAC"
description: "Role-based access control with org, team, user, and key hierarchy"
section: security
order: 1
---
# RBAC (Role-Based Access Control)

VoidLLM uses a hierarchical access control model: Organization -> Team -> User -> Key.

## Roles

| Role | Scope | Can do |
|---|---|---|
| `system_admin` | All orgs | Everything. Create orgs, manage all users, access all data. |
| `org_admin` | Their org | Manage teams, users, keys, models, settings within their org. |
| `team_admin` | Their teams | Manage team members, team keys. Can't create teams or manage org settings. |
| `member` | Their scope | Use keys, view own usage. No admin capabilities. |

Roles are hierarchical: `system_admin` > `org_admin` > `team_admin` > `member`. A higher role can do everything a lower role can.

## Key Types

| Prefix | Type | Scoped to | Created by |
|---|---|---|---|
| `vl_uk_` | User key | User's org | org_admin or self |
| `vl_tk_` | Team key | Specific team | team_admin |
| `vl_sa_` | Service account | Org or team | org_admin or team_admin |
| `vl_sk_` | Session key | Login session (24h TTL) | System (on login) |

## Limits Inheritance

Rate limits and token budgets can be set at org, team, and key level. The **most restrictive** limit wins:

- Org allows 10,000 tokens/day
- Team allows 5,000 tokens/day
- Key allows 1,000 tokens/day
- Result: the key can use 1,000 tokens/day

If a higher level has no limit set, the lower level's limit applies. If no limits are set anywhere, usage is unlimited.

## Model Access Control

Model access uses an allowlist model:

- **Org level:** which models the org can access (empty = all allowed)
- **Team level:** subset of org models (empty = inherit all from org)
- **Key level:** subset of team/org models (empty = inherit all)

Configure via UI (Organization -> Models tab, Team -> Models tab) or API.

## MCP Access Control

MCP server access for global servers is **closed by default** at the org level:

- **Org level:** must explicitly grant access to global MCP servers
- **Team level:** can restrict within org allowlist (empty = inherit all from org)

Org-scoped and team-scoped MCP servers are automatically accessible to their scope.

System admins bypass MCP access checks.

## User Onboarding

Three ways to add users:

1. **Invite Link** - admin creates invite, user sets password via link (expires in 7 days)
2. **Manual Creation** - system admin creates user directly with email + password
3. **SSO Auto-Provisioning** - users created automatically on first SSO login (Enterprise)

See [SSO documentation](../enterprise/sso.md) for SSO-based onboarding.
