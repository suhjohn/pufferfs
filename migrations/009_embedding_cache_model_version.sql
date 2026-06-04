-- +goose Up

-- Add model_version to the embedding cache key. Vectors produced by different
-- embedding models are not interchangeable, so caching by (org_id, content_hash)
-- alone would silently reuse stale vectors after a model upgrade. Existing rows
-- predate versioning and are tagged with the empty string.
ALTER TABLE embedding_cache ADD COLUMN IF NOT EXISTS model_version TEXT NOT NULL DEFAULT '';
ALTER TABLE embedding_cache DROP CONSTRAINT embedding_cache_pkey;
ALTER TABLE embedding_cache ADD PRIMARY KEY (org_id, model_version, content_hash);

-- +goose Down
ALTER TABLE embedding_cache DROP CONSTRAINT embedding_cache_pkey;
ALTER TABLE embedding_cache ADD PRIMARY KEY (org_id, content_hash);
ALTER TABLE embedding_cache DROP COLUMN IF EXISTS model_version;
