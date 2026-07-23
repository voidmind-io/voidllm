-- Migration: 0015_soft_delete_unique_collision.down.sql
-- Description: Reverses 0015_soft_delete_unique_collision.up.sql.
--
-- Restores idx_users_external to its original 0001 definition (no
-- deleted_at predicate), then strips the ':deleted:' || id tombstone suffix
-- appended by the up migration. Each UPDATE's WHERE clause only matches
-- rows whose current value still ends in that row's own ':deleted:' || id
-- suffix, so it is a safe no-op for any row that was changed independently
-- since the up migration ran.

DROP INDEX idx_users_external;

CREATE UNIQUE INDEX idx_users_external
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL;

UPDATE model_deployments
SET name = substr(name, 1, length(name) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(name) > length(':deleted:' || id)
  AND substr(name, length(name) - length(':deleted:' || id) + 1) = ':deleted:' || id;

UPDATE teams
SET slug = substr(slug, 1, length(slug) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(slug) > length(':deleted:' || id)
  AND substr(slug, length(slug) - length(':deleted:' || id) + 1) = ':deleted:' || id;

UPDATE organizations
SET slug = substr(slug, 1, length(slug) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(slug) > length(':deleted:' || id)
  AND substr(slug, length(slug) - length(':deleted:' || id) + 1) = ':deleted:' || id;

UPDATE users
SET email = substr(email, 1, length(email) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(email) > length(':deleted:' || id)
  AND substr(email, length(email) - length(':deleted:' || id) + 1) = ':deleted:' || id;

UPDATE models
SET name = substr(name, 1, length(name) - length(':deleted:' || id))
WHERE deleted_at IS NOT NULL
  AND length(name) > length(':deleted:' || id)
  AND substr(name, length(name) - length(':deleted:' || id) + 1) = ':deleted:' || id;
