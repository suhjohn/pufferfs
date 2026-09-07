-- +goose Up
-- Replace the per-input ledger. No old jobs or upload records are converted.
DROP TABLE provider_batch_files;
DROP TABLE provider_files;
DROP TABLE provider_requests;
DROP TABLE provider_batches;

-- No catalog FK: submission and upload cleanup must survive root deletion.
-- Every row covers one contiguous source range, including all its retries.
CREATE TABLE provider_batches (
    id TEXT PRIMARY KEY,
    extraction_id TEXT NOT NULL,
    org_id TEXT NOT NULL,
    root_id TEXT NOT NULL,
    ordinal_start BIGINT NOT NULL CHECK (ordinal_start >= 0 AND ordinal_start % 64 = 0),
    request_count INT NOT NULL CHECK (request_count BETWEEN 1 AND 64),
    model TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('preparing','submitted','retry','complete','failed')),
    attempt_count INT NOT NULL DEFAULT 1 CHECK (attempt_count BETWEEN 1 AND 3),
    provider_job_id TEXT UNIQUE,
    submission_started_at TIMESTAMPTZ,
    reconciliation_cursor TEXT NOT NULL DEFAULT '' CHECK (length(reconciliation_cursor)<=8192),
    input_ref TEXT NOT NULL,
    output_ref TEXT NOT NULL DEFAULT '',
    cleanup_ref TEXT NOT NULL DEFAULT '',
    cleanup_complete BOOLEAN NOT NULL DEFAULT FALSE,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    next_check_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (extraction_id,ordinal_start)
);
CREATE INDEX provider_batches_pending_idx ON provider_batches(next_check_at,id)
    WHERE status IN ('preparing','submitted','retry');
CREATE INDEX provider_batches_cleanup_idx ON provider_batches(cleanup_after,id)
    WHERE NOT cleanup_complete AND status IN ('complete','failed');
CREATE INDEX file_work_provider_assembly_idx ON file_work(updated_at,id)
    WHERE stage='transform' AND status='waiting_provider';

-- +goose Down
-- No reconstruction of the removed per-input ledger is supported.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Provider manifest schema cannot be rolled back'; END $$;
-- +goose StatementEnd
