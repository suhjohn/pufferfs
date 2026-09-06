-- +goose Up
-- An extraction's immutable registration order, not its completion timestamp
-- or lexicographic revision name. Idempotent registration keeps the same value.
ALTER TABLE file_extractions ADD COLUMN sequence BIGSERIAL UNIQUE NOT NULL
    CHECK (sequence > 0);

-- Historical extraction order was not recorded. Conservatively preserve each
-- published extraction over every pre-migration alternative for that version.
-- Register a new revision to deliberately replace it after migration.
UPDATE file_extractions e SET sequence=nextval('file_extractions_sequence_seq')
    FROM file_catalog f WHERE f.indexed_extraction_id=e.id;

-- Old cleanup packs only encode version cutoffs; regenerate with revision
-- ordering before applying any same-version cleanup.
UPDATE file_catalog SET index_cleanup_ref='',index_cleanup_record=0,
    index_cleanup_due_at=NOW() WHERE indexed_version_id IS NOT NULL;

-- +goose Down
ALTER TABLE file_extractions DROP COLUMN sequence;
UPDATE file_catalog SET index_cleanup_ref='',index_cleanup_record=0,
    index_cleanup_due_at=NOW() WHERE indexed_version_id IS NOT NULL;
