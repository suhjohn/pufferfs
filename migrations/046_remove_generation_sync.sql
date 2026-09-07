-- +goose Up
-- Current captures and deletion targets use file identities and S3 packs only.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION retain_root_cleanup_targets() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
        SELECT OLD.id,COALESCE(OLD.org_id,''),'namespace',namespace,OLD.vector_disabled
        FROM root_index_namespaces WHERE root_id=OLD.id
        ON CONFLICT DO NOTHING;
    INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
        SELECT OLD.id,OLD.org_id,'prefix',kind || '/' || OLD.org_id || '/' || OLD.id || '/',OLD.vector_disabled
        FROM unnest(ARRAY['sources','extractions','mutations']) AS kind
        ON CONFLICT DO NOTHING;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
DROP TABLE sync_job_shards;
DROP TABLE sync_generations;
DROP TABLE sync_jobs;
DROP TABLE root_states;
DROP TABLE embedding_cache;
DROP TABLE content_proofs;
ALTER TABLE roots DROP COLUMN visible_generation_id;
ALTER TABLE file_versions DROP COLUMN extents_indexed_at, DROP COLUMN extents_backfill_after;
ALTER TABLE source_objects ALTER COLUMN uploader_id SET NOT NULL;
ALTER TABLE source_objects ADD CONSTRAINT source_objects_uploader_identity CHECK (uploader_id <> '');

-- +goose Down
-- Removed inventories and jobs cannot be reconstructed.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Generation sync removal cannot be rolled back'; END $$;
-- +goose StatementEnd
