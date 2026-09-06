-- +goose Up
ALTER TABLE source_objects
    ADD COLUMN uploader_id TEXT,
    ADD COLUMN capture_id TEXT,
    ADD COLUMN authorized_until TIMESTAMPTZ NOT NULL DEFAULT (NOW()+INTERVAL '15 minutes'),
    ADD COLUMN retired_at TIMESTAMPTZ,
    ADD COLUMN cleanup_due_at TIMESTAMPTZ,
    ADD COLUMN deleted_at TIMESTAMPTZ;
ALTER TABLE file_versions
    ADD COLUMN extents_indexed_at TIMESTAMPTZ,
    ADD COLUMN extents_backfill_after TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN source_retired_at TIMESTAMPTZ;
UPDATE file_versions SET extents_indexed_at=NOW() WHERE deleted;

-- Metadata edges only, not source bytes or another copy of the manifest.
-- Keep edges after retirement for provenance and idempotent capture receipts.
CREATE TABLE file_version_extents (
    version_id TEXT NOT NULL REFERENCES file_versions(id) ON DELETE CASCADE,
    ordinal INT NOT NULL CHECK(ordinal>=0),
    object_key TEXT NOT NULL REFERENCES source_objects(object_key),
    byte_offset BIGINT NOT NULL CHECK(byte_offset>=0),
    byte_length BIGINT NOT NULL CHECK(byte_length>0),
    PRIMARY KEY(version_id,ordinal)
);
CREATE INDEX file_version_extents_object_idx ON file_version_extents(object_key,version_id);
CREATE INDEX source_objects_retention_idx ON source_objects(root_id,authorized_until,created_at) WHERE retired_at IS NULL;
CREATE INDEX source_objects_cleanup_idx ON source_objects(cleanup_due_at) WHERE retired_at IS NOT NULL;
CREATE INDEX file_versions_backfill_idx ON file_versions(extents_backfill_after,id) WHERE extents_indexed_at IS NULL;
CREATE INDEX file_versions_source_retention_idx ON file_versions(created_at,id) WHERE source_retired_at IS NULL;

-- +goose Down
DROP TABLE file_version_extents;
DROP INDEX file_versions_backfill_idx;
DROP INDEX file_versions_source_retention_idx;
DROP INDEX source_objects_cleanup_idx;
DROP INDEX source_objects_retention_idx;
ALTER TABLE file_versions DROP COLUMN extents_indexed_at, DROP COLUMN extents_backfill_after, DROP COLUMN source_retired_at;
ALTER TABLE source_objects DROP COLUMN uploader_id, DROP COLUMN capture_id,
    DROP COLUMN authorized_until, DROP COLUMN retired_at, DROP COLUMN cleanup_due_at, DROP COLUMN deleted_at;
