-- +goose Up
-- Never silently discard routing for previously sharded roots.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS(SELECT root_id FROM root_index_namespaces WHERE retired_at IS NULL
              GROUP BY root_id HAVING count(*)<>1 OR max(shard_count)<>1) THEN
        RAISE EXCEPTION 'Consolidate sharded root indexes before upgrading to one namespace per root';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE root_index_namespaces DROP COLUMN shard_index, DROP COLUMN shard_count;
CREATE UNIQUE INDEX root_index_namespaces_active_root ON root_index_namespaces(root_id)
    WHERE retired_at IS NULL;
CREATE INDEX root_index_namespaces_root ON root_index_namespaces(root_id);

-- +goose Down
ALTER TABLE root_index_namespaces ADD COLUMN shard_index INT NOT NULL DEFAULT 0,
    ADD COLUMN shard_count INT NOT NULL DEFAULT 1;
DROP INDEX root_index_namespaces_active_root;
DROP INDEX root_index_namespaces_root;
