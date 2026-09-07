-- +goose Up
-- Both are exact non-unique duplicates of existing unique indexes. Keep the
-- uniqueness constraints and their indexes, without maintaining a second copy.
DROP INDEX idx_api_keys_hash;
DROP INDEX idx_root_index_namespaces_root;

-- +goose Down
CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);
CREATE INDEX idx_root_index_namespaces_root ON root_index_namespaces(root_id,shard_index);
