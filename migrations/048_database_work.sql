-- +goose Up
-- Stop old consumers and Modal workers before applying this migration.
-- Keep one stable work identity per extraction; the phase advances in place.
ALTER TABLE file_work DROP CONSTRAINT file_work_extraction_id_stage_key;
CREATE TEMP TABLE retired_index_work ON COMMIT DROP AS
    SELECT i.id FROM file_work i JOIN file_work t ON t.extraction_id=i.extraction_id
    WHERE t.stage='transform' AND i.stage='index';
UPDATE file_work t SET stage='index',status=i.status,attempt_token=NULL,
    attempt_count=i.attempt_count,lease_until=NULL,error=i.error,updated_at=i.updated_at
FROM file_work i WHERE t.extraction_id=i.extraction_id
    AND t.stage='transform' AND i.stage='index';
DELETE FROM file_work WHERE id IN (SELECT id FROM retired_index_work);
ALTER TABLE file_work ADD CONSTRAINT file_work_extraction_key UNIQUE(extraction_id);
ALTER TABLE file_work ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE file_extractions ADD COLUMN row_format INT NOT NULL DEFAULT 1 CHECK(row_format=1);
UPDATE file_work SET status='pending',attempt_token=NULL,lease_until=NULL WHERE status='running';
ALTER TABLE file_work DROP COLUMN enqueued_at, DROP COLUMN invocation_id,
    DROP COLUMN mutation_ref, DROP COLUMN mutation_batch_count, DROP COLUMN acknowledged_batches;
DROP INDEX file_work_reconciliation_idx;
CREATE INDEX file_work_due_idx ON file_work(stage,next_attempt_at,id)
    WHERE status IN ('pending','running');

-- +goose Down
-- The old split-work schema cannot be restored while new workers are active.
-- Restore the pre-cutover database backup and old application together.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'database-work migration requires coordinated backup restore'; END $$;
-- +goose StatementEnd
