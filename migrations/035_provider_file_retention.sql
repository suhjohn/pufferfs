-- +goose Up
-- Exact provider identities survive root/request deletion. No media bodies,
-- credentials, work queue or account-wide provider listing is stored here.
-- Only our uploads belong here, not Google's generated batch-result files.
CREATE TABLE provider_files (
    file_id TEXT PRIMARY KEY,
    extraction_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_check_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX provider_files_cleanup_idx ON provider_files(next_check_at) WHERE deleted_at IS NULL;
CREATE TABLE provider_batch_files (
    batch_id TEXT NOT NULL REFERENCES provider_batches(id),
    file_id TEXT NOT NULL REFERENCES provider_files(file_id),
    PRIMARY KEY (batch_id, file_id)
);
CREATE INDEX provider_batch_files_file_idx ON provider_batch_files(file_id);

INSERT INTO provider_files(file_id,extraction_id)
SELECT DISTINCT ON (input_file_id) input_file_id,extraction_id FROM provider_requests
WHERE input_file_id IS NOT NULL AND input_file_id<>'' ON CONFLICT DO NOTHING;
INSERT INTO provider_files(file_id)
SELECT input_file_id FROM provider_batches WHERE input_file_id IS NOT NULL AND input_file_id<>''
ON CONFLICT DO NOTHING;
INSERT INTO provider_batch_files(batch_id,file_id)
SELECT batch_id,input_file_id FROM provider_requests
WHERE batch_id IS NOT NULL AND input_file_id IS NOT NULL AND input_file_id<>''
ON CONFLICT DO NOTHING;
INSERT INTO provider_batch_files(batch_id,file_id)
SELECT id,input_file_id FROM provider_batches WHERE input_file_id IS NOT NULL AND input_file_id<>''
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE provider_batch_files;
DROP TABLE provider_files;
