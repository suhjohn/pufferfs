-- +goose Up

-- Sync job tracking for indexing status
CREATE TABLE sync_jobs (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id      TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id),
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending, uploading, chunking, embedding, upserting, completed, failed
    total_files  INT NOT NULL DEFAULT 0,
    processed    INT NOT NULL DEFAULT 0,
    errors       JSONB NOT NULL DEFAULT '[]',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at  TIMESTAMPTZ
);

CREATE INDEX idx_sync_jobs_org_root ON sync_jobs(org_id, root_id);
CREATE INDEX idx_sync_jobs_status ON sync_jobs(status);

-- +goose Down
DROP TABLE IF EXISTS sync_jobs;
