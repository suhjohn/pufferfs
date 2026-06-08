-- +goose Up

ALTER TABLE sync_jobs ALTER COLUMN root_id DROP NOT NULL;
ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS sync_jobs_root_id_fkey;
ALTER TABLE sync_jobs
    ADD CONSTRAINT sync_jobs_root_id_fkey
    FOREIGN KEY (root_id) REFERENCES roots(id) ON DELETE SET NULL;

-- +goose Down

DELETE FROM sync_jobs WHERE root_id IS NULL;
ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS sync_jobs_root_id_fkey;
ALTER TABLE sync_jobs
    ADD CONSTRAINT sync_jobs_root_id_fkey
    FOREIGN KEY (root_id) REFERENCES roots(id) ON DELETE CASCADE;
ALTER TABLE sync_jobs ALTER COLUMN root_id SET NOT NULL;
