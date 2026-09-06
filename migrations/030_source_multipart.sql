-- +goose Up
CREATE TABLE source_multipart_uploads (
    object_key TEXT PRIMARY KEY REFERENCES source_objects(object_key) ON DELETE CASCADE,
    upload_id TEXT NOT NULL DEFAULT '',
    part_size BIGINT NOT NULL CHECK(part_size >= 5242880),
    completion_parts JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE source_multipart_uploads;
