-- Migration: 0015_soft_delete_unique_collision.down.sql
-- Description: Reverses 0015_soft_delete_unique_collision.up.sql.
--
-- Restores idx_users_external to its original 0001 definition (no
-- deleted_at predicate), then strips the ':deleted:' || id tombstone suffix
-- appended by the up migration. Each UPDATE's WHERE clause only matches
-- rows whose current value still ends in that row's own ':deleted:' || id
-- suffix, so it is a safe no-op for any row that was changed independently
-- since the up migration ran.
--
-- Each strip also carries a correlated NOT EXISTS guard: the whole point of
-- the up migration was to free a deleted row's original value for reuse, so
-- by the time this down migration runs, some other row (active, or another
-- deleted row) may legitimately hold that original value already. Stripping
-- the suffix unconditionally would then collide with it under the UNIQUE
-- constraint and abort the migration. The guard skips the strip for that
-- one row instead -- it keeps its tombstoned value rather than aborting.
-- There is no way to recover the original value for such a row without
-- manual intervention, since it is now legitimately in use elsewhere.
--
-- idx_users_external cannot be given an equivalent guard: CREATE UNIQUE
-- INDEX either succeeds or fails outright, there is no per-row skip. If a
-- deleted and an active user now share (auth_provider, external_id) -- which
-- the up migration explicitly allowed -- recreating the unfiltered index
-- will fail by definition, and this migration will abort at that
-- statement. Rolling back after such an identity has been reused requires
-- manually resolving (re-mangling or removing) the conflicting row(s)
-- first. In practice this is not a concern: no VoidLLM code path executes
-- down migrations automatically; they exist for manual/local use only.

DROP INDEX idx_users_external;

CREATE UNIQUE INDEX idx_users_external
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL;

UPDATE model_deployments
SET name = substr(name, 1, length(name) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(name) > length(':deleted:' || id)
  AND substr(name, length(name) - length(':deleted:' || id) + 1) = ':deleted:' || id
  AND NOT EXISTS (
    SELECT 1 FROM model_deployments t2
    WHERE t2.model_id = model_deployments.model_id
      AND t2.name = substr(model_deployments.name, 1, length(model_deployments.name) - length(':deleted:' || model_deployments.id))
  );

UPDATE teams
SET slug = substr(slug, 1, length(slug) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(slug) > length(':deleted:' || id)
  AND substr(slug, length(slug) - length(':deleted:' || id) + 1) = ':deleted:' || id
  AND NOT EXISTS (
    SELECT 1 FROM teams t2
    WHERE t2.org_id = teams.org_id
      AND t2.slug = substr(teams.slug, 1, length(teams.slug) - length(':deleted:' || teams.id))
  );

UPDATE organizations
SET slug = substr(slug, 1, length(slug) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(slug) > length(':deleted:' || id)
  AND substr(slug, length(slug) - length(':deleted:' || id) + 1) = ':deleted:' || id
  AND NOT EXISTS (
    SELECT 1 FROM organizations t2
    WHERE t2.slug = substr(organizations.slug, 1, length(organizations.slug) - length(':deleted:' || organizations.id))
  );

UPDATE users
SET email = substr(email, 1, length(email) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(email) > length(':deleted:' || id)
  AND substr(email, length(email) - length(':deleted:' || id) + 1) = ':deleted:' || id
  AND NOT EXISTS (
    SELECT 1 FROM users t2
    WHERE t2.email = substr(users.email, 1, length(users.email) - length(':deleted:' || users.id))
  );

UPDATE models
SET name = substr(name, 1, length(name) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(name) > length(':deleted:' || id)
  AND substr(name, length(name) - length(':deleted:' || id) + 1) = ':deleted:' || id
  AND NOT EXISTS (
    SELECT 1 FROM models t2
    WHERE t2.name = substr(models.name, 1, length(models.name) - length(':deleted:' || models.id))
  );
