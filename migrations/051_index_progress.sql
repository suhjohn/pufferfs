-- +goose Up
-- A cursor counts confirmed chunk ordinals, not configuration-dependent batches.
ALTER TABLE file_work ADD COLUMN index_cursor BIGINT NOT NULL DEFAULT 0 CHECK (index_cursor >= 0);
UPDATE file_work w SET index_cursor=e.chunk_count FROM file_extractions e
    WHERE w.extraction_id=e.id AND w.stage='index' AND w.status='complete';

-- +goose Down
ALTER TABLE file_work DROP COLUMN index_cursor;
