-- +goose Up
-- Cache packs, not published index mutations. Keep retirement tombstones even
-- after tenant deletion so a late S3 PUT can be removed by a subsequent pass.
CREATE TABLE embedding_packs (
    object_key TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at TIMESTAMPTZ,
    cleanup_due_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);
INSERT INTO embedding_packs(object_key,org_id)
    SELECT DISTINCT object_key,org_id FROM embedding_locations;
ALTER TABLE embedding_locations ADD CONSTRAINT embedding_locations_pack_fk
    FOREIGN KEY (object_key) REFERENCES embedding_packs(object_key);
CREATE INDEX embedding_packs_unused_idx ON embedding_packs(last_used_at,object_key)
    WHERE retired_at IS NULL;
CREATE INDEX embedding_packs_cleanup_idx ON embedding_packs(cleanup_due_at,object_key)
    WHERE retired_at IS NOT NULL;
CREATE INDEX embedding_locations_pack_idx ON embedding_locations(object_key);

-- +goose Down
DROP INDEX embedding_locations_pack_idx;
ALTER TABLE embedding_locations DROP CONSTRAINT embedding_locations_pack_fk;
DROP TABLE embedding_packs;
