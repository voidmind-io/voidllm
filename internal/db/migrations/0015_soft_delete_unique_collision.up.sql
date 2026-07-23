-- Migration: 0015_soft_delete_unique_collision.up.sql
-- Description: Soft-deleted rows held their UNIQUE column values hostage,
-- permanently blocking re-use of that name/email/slug (#172). Affected:
-- models.name, users.email, organizations.slug, teams(org_id, slug), and
-- model_deployments(model_id, name) -- all declared as inline UNIQUE
-- constraints in 0001/0003. SQLite cannot drop an inline UNIQUE constraint
-- without a full table rebuild, which is not possible inside this
-- transactional migration runner with foreign_keys=ON. Fix: mangle the
-- constrained value on every row that is already soft-deleted, freeing the
-- original value for reuse by new/active rows.
--
-- Each UPDATE carries a correlated NOT EXISTS guard rather than running
-- unconditionally. Reason: prior to this patch, values containing the
-- literal substring ':deleted:' were accepted on ACTIVE rows (nothing
-- rejected them). If some active row's value already happens to equal
-- <deleted-row's-value>:deleted:<deleted-row-id>, mangling the deleted row
-- would collide with that active row's existing value and abort the whole
-- migration under the UNIQUE constraint -- which would prevent the instance
-- from starting at all. The guard makes that one pathological row a no-op
-- instead: it keeps its original (still-blocked) value, and every other
-- deleted row is mangled normally. That original value stays permanently
-- blocked for reuse in this one case, which is preferred over a failed
-- startup. The odds of this are negligible -- it requires an existing
-- active row whose value exactly matches another specific row's full
-- tombstone string, including that row's own UUID.
--
-- idx_users_external is a *named* index rather than an inline constraint,
-- so unlike the others it can be dropped and recreated portably -- this
-- adds the missing deleted_at IS NULL predicate so a deleted OIDC identity
-- no longer blocks re-provisioning under the same provider + external_id.

UPDATE models
SET name = name || ':deleted:' || id
WHERE deleted_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM models t2
    WHERE t2.name = models.name || ':deleted:' || models.id
  );

UPDATE users
SET email = email || ':deleted:' || id
WHERE deleted_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM users t2
    WHERE t2.email = users.email || ':deleted:' || users.id
  );

UPDATE organizations
SET slug = slug || ':deleted:' || id
WHERE deleted_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM organizations t2
    WHERE t2.slug = organizations.slug || ':deleted:' || organizations.id
  );

UPDATE teams
SET slug = slug || ':deleted:' || id
WHERE deleted_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM teams t2
    WHERE t2.org_id = teams.org_id
      AND t2.slug = teams.slug || ':deleted:' || teams.id
  );

UPDATE model_deployments
SET name = name || ':deleted:' || id
WHERE deleted_at IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM model_deployments t2
    WHERE t2.model_id = model_deployments.model_id
      AND t2.name = model_deployments.name || ':deleted:' || model_deployments.id
  );

DROP INDEX idx_users_external;

CREATE UNIQUE INDEX idx_users_external
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL AND deleted_at IS NULL;
