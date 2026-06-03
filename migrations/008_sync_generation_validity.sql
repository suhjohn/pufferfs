-- +goose Up
ALTER TABLE sync_generations ADD COLUMN IF NOT EXISTS seq BIGSERIAL;
ALTER TABLE sync_generations ADD COLUMN IF NOT EXISTS base_generation_seq BIGINT NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX IF NOT EXISTS sync_generations_seq_idx ON sync_generations(seq);

-- +goose Down
DROP INDEX IF EXISTS sync_generations_seq_idx;
ALTER TABLE sync_generations DROP COLUMN IF EXISTS base_generation_seq;
ALTER TABLE sync_generations DROP COLUMN IF EXISTS seq;
