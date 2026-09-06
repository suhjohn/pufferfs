-- +goose Up
CREATE INDEX file_extractions_version_order_idx ON file_extractions(version_id,sequence DESC);
-- +goose Down
DROP INDEX file_extractions_version_order_idx;
