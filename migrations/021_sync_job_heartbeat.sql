-- +goose Up
ALTER TABLE sync_jobs
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

UPDATE sync_jobs SET updated_at = COALESCE(finished_at, started_at);

CREATE INDEX IF NOT EXISTS idx_sync_jobs_active_updated
    ON sync_jobs(updated_at)
    WHERE status NOT IN ('completed', 'failed');

-- +goose Down
DROP INDEX IF EXISTS idx_sync_jobs_active_updated;
ALTER TABLE sync_jobs DROP COLUMN IF EXISTS updated_at;
