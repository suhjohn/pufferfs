-- +goose Up
ALTER TABLE provider_requests ADD COLUMN attempt_count INT NOT NULL DEFAULT 1 CHECK(attempt_count >= 1);
ALTER TABLE provider_batches ADD COLUMN retry_of TEXT UNIQUE REFERENCES provider_batches(id);

-- +goose Down
ALTER TABLE provider_batches DROP COLUMN retry_of;
ALTER TABLE provider_requests DROP COLUMN attempt_count;
