-- +goose Up
ALTER TABLE roots ADD COLUMN IF NOT EXISTS visible_generation_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sync_generations (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id            TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    sync_job_id        TEXT REFERENCES sync_jobs(id) ON DELETE SET NULL,
    base_generation_id TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'building',
    manifest_ref       TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    visible_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS sync_generations_root_status_idx
    ON sync_generations(root_id, status, created_at);

-- +goose Down
DROP INDEX IF EXISTS sync_generations_root_status_idx;
DROP TABLE IF EXISTS sync_generations;
ALTER TABLE roots DROP COLUMN IF EXISTS visible_generation_id;
