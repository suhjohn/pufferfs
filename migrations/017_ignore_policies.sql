-- +goose Up
CREATE TABLE IF NOT EXISTS org_ignore_policies (
    org_id             TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    patterns           TEXT NOT NULL DEFAULT '',
    updated_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_ignore_policies (
    org_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    patterns   TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, user_id)
);

-- +goose Down
DROP TABLE IF EXISTS user_ignore_policies;
DROP TABLE IF EXISTS org_ignore_policies;
