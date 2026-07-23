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
-- These UPDATEs run unconditionally, with no "already mangled" guard,
-- because this migration executes exactly once and no released version has
-- ever produced a ':deleted:'-suffixed value.
--
-- idx_users_external is a *named* index rather than an inline constraint,
-- so unlike the others it can be dropped and recreated portably -- this
-- adds the missing deleted_at IS NULL predicate so a deleted OIDC identity
-- no longer blocks re-provisioning under the same provider + external_id.

UPDATE models SET name = name || ':deleted:' || id WHERE deleted_at IS NOT NULL;

UPDATE users SET email = email || ':deleted:' || id WHERE deleted_at IS NOT NULL;

UPDATE organizations SET slug = slug || ':deleted:' || id WHERE deleted_at IS NOT NULL;

UPDATE teams SET slug = slug || ':deleted:' || id WHERE deleted_at IS NOT NULL;

UPDATE model_deployments SET name = name || ':deleted:' || id WHERE deleted_at IS NOT NULL;

DROP INDEX idx_users_external;

CREATE UNIQUE INDEX idx_users_external
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL AND deleted_at IS NULL;
