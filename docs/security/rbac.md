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

Rate limits (requests per minute / requests per day) and token budgets (daily /
monthly) can be set at four levels: **org -> team -> user -> key**. Every
level is checked independently on each request, so the **most restrictive**
limit anywhere in the hierarchy wins:

- Org allows 10,000 tokens/day
- Team allows 5,000 tokens/day
- User (the caller's org membership) allows 2,000 tokens/day
- Key allows 1,000 tokens/day
- Result: the key can use 1,000 tokens/day

If a level has no limit set (0), it is treated as unlimited and the next
level's limit applies. If no limits are set anywhere, usage is unlimited.

The **user level** is set on the org membership, not on an individual key. It
caps a single user's aggregate usage across *every* API key they own in that
org — including keys created after the limit was set — so a user cannot
bypass a per-user budget by minting additional keys. Only org admins and
system admins may set user-level limits, via `PATCH
/api/v1/orgs/{org_id}/members/{membership_id}`.

**Key-level limits** may only be set by org admins and system admins —
`team_admin` can never set or raise limits on any key, including keys scoped
to their own team, since a team-scoped key (team key or team-bound service
account key) has no individual owner and a team admin could otherwise use it
to grant themselves an unlimited budget. A `member` can never set or raise
the limits on their own key, whether at creation
(`POST /api/v1/orgs/{org_id}/keys`) or update
(`PATCH /api/v1/orgs/{org_id}/keys/{key_id}`).

A machine caller (a service account key with no team scope, which resolves
to `org_admin`) may set limits on other keys in the org, but never on its own
key or on another key belonging to the same service account — this prevents
a compromised or self-service org-level service account from raising its own
budget.

**Service-account keys inherit their role from the service account, not from
the key itself:** a service-account key that belongs to a team-bound service
account resolves to `team_admin` scope; one that belongs to an org-level
service account (no team) resolves to `org_admin` scope. This binding lives
on the service account, not on the key row, so rotating or re-issuing a
service-account key never changes its scope.

**Deleting a service account immediately invalidates its keys.** Because a
service-account key's role and scope are entirely derived from the owning
service account, a `vl_sa_` key whose service account has been soft-deleted
(or no longer exists) is never loaded into the in-memory key cache — at
startup, on the periodic cache refresh, and immediately after key creation or
rotation. On top of that, deleting a service account evicts its keys from the
handling instance's cache immediately, and from every other instance's cache
immediately when Redis is configured, or otherwise on that instance's next
periodic cache refresh. Such a key stops authenticating the moment its
service account is deleted, even though the key row itself is untouched;
rotating a key still attached to a deleted service account is also rejected
with `400 service account not found`.

**Deleting a user, or removing them from an org, immediately revokes their
keys — unless they are a system admin.** Every `user_key` and session key
requires a surviving (non soft-deleted) owning user to be loaded into the
in-memory key cache, and, for ordinary users, also a surviving membership in
the key's own org — at startup, on the periodic cache refresh, and
immediately after key creation, rotation, or update. System admins are the
one exception: their privilege comes from the global `users.is_system_admin`
flag, not from an org membership, so a system admin's key remains cacheable
without a membership row (legacy deployments may have system admins with no
membership at all). Soft-deleting a user (`DELETE /api/v1/users/{user_id}`)
always revokes their keys immediately, system admin or not. Removing an org
membership (`DELETE /api/v1/orgs/{org_id}/members/{membership_id}`) evicts
the affected keys immediately unless the user is a system admin, in which
case their key keeps authenticating on its global privilege alone. Both
operations evict from the handling instance's cache immediately, and from
every other instance's cache immediately when Redis is configured, or
otherwise on that instance's next periodic cache refresh. Conversely, adding
a user back to an org (`POST /api/v1/orgs/{org_id}/members`) reloads the key
cache immediately so a key that could not previously be cached for lack of a
membership row becomes usable again without waiting for the next reload. A
user's keys in other organizations are unaffected by removing their
membership in one org.

**Team keys are shared team credentials.** A `team_key` is not tied to an
individual user and is not subject to per-user limits — only key-level,
team-level, and org-level limits apply to it. Handing a team key to someone
gives them the full team budget; there is no way to restrict a team key to a
single member's allowance.

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
