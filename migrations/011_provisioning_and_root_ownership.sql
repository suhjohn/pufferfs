-- +goose Up

ALTER TABLE organizations ADD COLUMN IF NOT EXISTS external_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_organizations_external_id
    ON organizations(external_id)
    WHERE external_id IS NOT NULL;

ALTER TABLE users ADD COLUMN IF NOT EXISTS external_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_external_id
    ON users(external_id)
    WHERE external_id IS NOT NULL;

ALTER TABLE roots ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'org';
ALTER TABLE roots ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_roots_owner
    ON roots(owner_user_id)
    WHERE owner_user_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_roots_owner;
ALTER TABLE roots DROP COLUMN IF EXISTS owner_user_id;
ALTER TABLE roots DROP COLUMN IF EXISTS scope;

DROP INDEX IF EXISTS idx_users_external_id;
ALTER TABLE users DROP COLUMN IF EXISTS external_id;

DROP INDEX IF EXISTS idx_organizations_external_id;
ALTER TABLE organizations DROP COLUMN IF EXISTS external_id;
