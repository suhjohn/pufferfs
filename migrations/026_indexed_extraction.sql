-- +goose Up
ALTER TABLE file_extractions ADD CONSTRAINT file_extractions_version_id_unique UNIQUE(version_id,id);
ALTER TABLE file_catalog ADD COLUMN indexed_extraction_id TEXT;
ALTER TABLE file_catalog ADD CONSTRAINT file_catalog_indexed_extraction_fk
    FOREIGN KEY(indexed_version_id,indexed_extraction_id) REFERENCES file_extractions(version_id,id)
    DEFERRABLE INITIALLY DEFERRED;
-- +goose Down
ALTER TABLE file_catalog DROP CONSTRAINT file_catalog_indexed_extraction_fk;
ALTER TABLE file_catalog DROP COLUMN indexed_extraction_id;
ALTER TABLE file_extractions DROP CONSTRAINT file_extractions_version_id_unique;
