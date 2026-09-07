-- +goose Up
-- Replace per-vector locators with one bounded directory per immutable S3 pack.
-- Existing cache entries are not converted; cache misses are encoded normally.
DROP TABLE embedding_locations;
DROP TABLE embedding_packs;
CREATE TABLE embedding_packs (
    object_key TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    model_revision TEXT NOT NULL,
    dimensions INT NOT NULL CHECK (dimensions > 0),
    content_hashes TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(content_hashes) <= 512),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at TIMESTAMPTZ,
    cleanup_due_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX embedding_packs_unused_idx ON embedding_packs(last_used_at,object_key)
    WHERE retired_at IS NULL;
CREATE INDEX embedding_packs_cleanup_idx ON embedding_packs(cleanup_due_at,object_key)
    WHERE retired_at IS NOT NULL;
CREATE INDEX embedding_packs_hashes_idx ON embedding_packs USING gin(content_hashes)
    WHERE retired_at IS NULL;
CREATE INDEX embedding_packs_model_idx ON embedding_packs(org_id,model_revision)
    WHERE retired_at IS NULL;

-- +goose Down
DROP TABLE embedding_packs;
