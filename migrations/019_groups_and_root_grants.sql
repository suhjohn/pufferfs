-- +goose Up

CREATE TABLE IF NOT EXISTS groups (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(org_id, name)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_org_external
    ON groups(org_id, external_id)
    WHERE external_id <> '';

CREATE TABLE IF NOT EXISTS group_members (
    org_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    group_id  TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, group_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_group_members_user
    ON group_members(org_id, user_id);

CREATE TABLE IF NOT EXISTS root_grants (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id        TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    principal_type TEXT NOT NULL,
    principal_id   TEXT NOT NULL,
    permissions    TEXT[] NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(root_id, principal_type, principal_id)
);
CREATE INDEX IF NOT EXISTS idx_root_grants_org_principal
    ON root_grants(org_id, principal_type, principal_id);
CREATE INDEX IF NOT EXISTS idx_root_grants_root
    ON root_grants(root_id);

-- +goose Down

DROP TABLE IF EXISTS root_grants;
DROP TABLE IF EXISTS group_members;
DROP INDEX IF EXISTS idx_groups_org_external;
DROP TABLE IF EXISTS groups;
