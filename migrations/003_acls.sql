-- +goose Up

-- Folder-level access control lists
CREATE TABLE root_acls (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id     TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    path_prefix TEXT NOT NULL DEFAULT '/',
    grant_to    TEXT NOT NULL,  -- user_id or 'role:viewer', 'role:editor', etc.
    permission  TEXT NOT NULL DEFAULT 'read',  -- 'read', 'write', 'none'
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_root_acls_org_root ON root_acls(org_id, root_id);
CREATE INDEX idx_root_acls_grant ON root_acls(grant_to);

-- +goose Down
DROP TABLE IF EXISTS root_acls;
