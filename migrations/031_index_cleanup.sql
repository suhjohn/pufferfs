-- +goose Up
-- Scheduled maintenance checkpoints, not an execution queue. Mutations live
-- in packed S3 artifacts and remain reusable for repeated late-write sweeps.
ALTER TABLE file_catalog
    ADD COLUMN index_cleanup_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN index_cleanup_record INT NOT NULL DEFAULT 0 CHECK (index_cleanup_record >= 0),
    ADD COLUMN index_cleanup_due_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
CREATE INDEX file_catalog_cleanup_due_idx ON file_catalog(index_cleanup_due_at,id)
    WHERE indexed_version_id IS NOT NULL;

-- +goose Down
DROP INDEX file_catalog_cleanup_due_idx;
ALTER TABLE file_catalog DROP COLUMN index_cleanup_ref,
    DROP COLUMN index_cleanup_record, DROP COLUMN index_cleanup_due_at;
