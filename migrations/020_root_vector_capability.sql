-- +goose Up

ALTER TABLE roots ADD COLUMN IF NOT EXISTS vector_disabled BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down

ALTER TABLE roots DROP COLUMN IF EXISTS vector_disabled;
