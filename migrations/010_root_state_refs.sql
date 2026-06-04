-- +goose Up
ALTER TABLE root_states ADD COLUMN IF NOT EXISTS state_ref TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE root_states DROP COLUMN IF EXISTS state_ref;
