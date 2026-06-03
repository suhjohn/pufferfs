-- +goose Up

-- Organizations
CREATE TABLE organizations (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Users (email/OAuth2 only, no passwords)
CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    provider   TEXT NOT NULL DEFAULT 'google',  -- 'google', 'github', etc.
    provider_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Organization membership with role
CREATE TABLE org_members (
    org_id  TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    TEXT NOT NULL DEFAULT 'viewer',  -- 'owner', 'admin', 'editor', 'viewer'
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, user_id)
);

-- API keys (hashed, scoped to org + user)
CREATE TABLE api_keys (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_hash   TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    scopes     TEXT[] NOT NULL DEFAULT '{}',
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);

-- Add org_id to roots (existing table)
ALTER TABLE roots ADD COLUMN org_id TEXT REFERENCES organizations(id) ON DELETE CASCADE;
CREATE INDEX idx_roots_org ON roots(org_id);

-- Add org_id to embedding_cache for tenant isolation
ALTER TABLE embedding_cache ADD COLUMN org_id TEXT REFERENCES organizations(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE embedding_cache DROP COLUMN IF EXISTS org_id;
ALTER TABLE roots DROP COLUMN IF EXISTS org_id;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;
