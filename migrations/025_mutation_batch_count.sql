-- +goose Up
ALTER TABLE file_work ADD COLUMN mutation_batch_count INT
    CHECK (mutation_batch_count >= 0);
-- +goose Down
ALTER TABLE file_work DROP COLUMN mutation_batch_count;
