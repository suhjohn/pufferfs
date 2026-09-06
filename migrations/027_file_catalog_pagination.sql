-- +goose Up
CREATE INDEX file_catalog_root_id_idx ON file_catalog(root_id, id);

-- +goose Down
DROP INDEX file_catalog_root_id_idx;
