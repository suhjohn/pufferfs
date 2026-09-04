-- +goose Up

ALTER TABLE roots
    ADD COLUMN IF NOT EXISTS deleting_at TIMESTAMPTZ;

-- +goose Down

ALTER TABLE roots
    DROP COLUMN IF EXISTS deleting_at;
