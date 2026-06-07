-- +goose Up

CREATE TABLE sync_job_shards (
    job_id          TEXT NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
    stage           TEXT NOT NULL,
    shard_index     INT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'completed',
    files_processed INT NOT NULL DEFAULT 0,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_id, stage, shard_index)
);

CREATE INDEX idx_sync_job_shards_job_stage_status
    ON sync_job_shards(job_id, stage, status);

-- +goose Down

DROP TABLE IF EXISTS sync_job_shards;
