-- +goose Up
ALTER TABLE file_extractions ADD COLUMN prepared_request_count INT
    CHECK (prepared_request_count >= 0);
ALTER TABLE provider_batches ADD COLUMN submission_started_at TIMESTAMPTZ;
ALTER TABLE provider_requests ADD COLUMN mime_type TEXT;
ALTER TABLE provider_requests ADD COLUMN input_uri TEXT;

-- +goose Down
ALTER TABLE provider_requests DROP COLUMN input_uri;
ALTER TABLE provider_requests DROP COLUMN mime_type;
ALTER TABLE provider_batches DROP COLUMN submission_started_at;
ALTER TABLE file_extractions DROP COLUMN prepared_request_count;
