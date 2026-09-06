-- +goose Up
-- A root cascades into source_objects and file_versions through independent
-- paths. Check this edge at transaction end, after both cascades have finished.
-- Ordinary pack GC keeps the source_objects tombstone and never deletes edges.
ALTER TABLE file_version_extents
    ALTER CONSTRAINT file_version_extents_object_key_fkey DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
ALTER TABLE file_version_extents
    ALTER CONSTRAINT file_version_extents_object_key_fkey NOT DEFERRABLE INITIALLY IMMEDIATE;
