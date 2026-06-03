-- +goose Up

-- SimHash column on roots for index reuse across org members.
-- Stores the 256-bit SimHash as a hex string for similarity matching.
ALTER TABLE roots ADD COLUMN IF NOT EXISTS simhash TEXT NOT NULL DEFAULT '';

-- Content proofs: per-user Merkle tree hashes for search result filtering.
-- Stores the serialized content proof (file hashes + dir hashes) for each user+root pair.
CREATE TABLE IF NOT EXISTS content_proofs (
    org_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_id   TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    root_hash TEXT NOT NULL DEFAULT '',
    proof     JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, user_id, root_id)
);

CREATE INDEX IF NOT EXISTS idx_roots_simhash ON roots (org_id, simhash) WHERE simhash != '';

-- +goose Down
DROP TABLE IF EXISTS content_proofs;
ALTER TABLE roots DROP COLUMN IF EXISTS simhash;
