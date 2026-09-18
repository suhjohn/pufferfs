-- +goose Up
-- Deletion requests derive from persistent root identities and published cutoffs.
ALTER TABLE file_catalog DROP COLUMN index_cleanup_ref, DROP COLUMN index_cleanup_record;
ALTER TABLE root_cleanup_targets DROP COLUMN mutation_ref, DROP COLUMN mutation_record;

-- +goose Down
ALTER TABLE file_catalog ADD COLUMN index_cleanup_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN index_cleanup_record INT NOT NULL DEFAULT 0;
ALTER TABLE root_cleanup_targets ADD COLUMN mutation_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN mutation_record INT NOT NULL DEFAULT 0;
