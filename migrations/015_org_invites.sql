-- +goose Up

CREATE TABLE IF NOT EXISTS org_invites (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email              TEXT NOT NULL,
    role               TEXT NOT NULL DEFAULT 'viewer',
    invited_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_org_invites_org_email
    ON org_invites(org_id, email);

CREATE INDEX IF NOT EXISTS idx_org_invites_email
    ON org_invites(email);

-- +goose Down

DROP TABLE IF EXISTS org_invites;
