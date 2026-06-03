-- +goose Up

-- Change embedding_cache primary key from (content_hash) to (org_id, content_hash)
-- for proper multi-tenant isolation of cached embeddings.
ALTER TABLE embedding_cache DROP CONSTRAINT embedding_cache_pkey;
ALTER TABLE embedding_cache ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE embedding_cache ADD PRIMARY KEY (org_id, content_hash);

-- +goose Down
ALTER TABLE embedding_cache DROP CONSTRAINT embedding_cache_pkey;
ALTER TABLE embedding_cache ALTER COLUMN org_id DROP NOT NULL;
ALTER TABLE embedding_cache ADD PRIMARY KEY (content_hash);
