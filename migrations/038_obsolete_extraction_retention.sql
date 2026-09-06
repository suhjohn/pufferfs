-- +goose Up
-- Retire artifacts only after their version is neither captured nor published
-- and all processing is terminal. Catalog/source history remains intact.
ALTER TABLE file_extractions ADD COLUMN artifacts_retired_at TIMESTAMPTZ;
ALTER TABLE file_extractions ADD COLUMN artifact_cleanup_due_at TIMESTAMPTZ;
ALTER TABLE file_extractions ADD COLUMN artifacts_deleted_at TIMESTAMPTZ;
CREATE INDEX file_extractions_retention_idx ON file_extractions(updated_at,id)
    WHERE artifacts_retired_at IS NULL AND status IN ('complete','superseded');
CREATE INDEX file_extractions_artifact_cleanup_idx ON file_extractions(artifact_cleanup_due_at,id)
    WHERE artifacts_retired_at IS NOT NULL;

-- +goose Down
DROP INDEX file_extractions_artifact_cleanup_idx;
DROP INDEX file_extractions_retention_idx;
ALTER TABLE file_extractions DROP COLUMN artifacts_deleted_at;
ALTER TABLE file_extractions DROP COLUMN artifact_cleanup_due_at;
ALTER TABLE file_extractions DROP COLUMN artifacts_retired_at;
