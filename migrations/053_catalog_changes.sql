-- +goose Up
-- Capture transactions append events, without acquiring another root lock.
-- A reader assigns revisions only after those transactions have committed.
-- A sequence allocated inside a capture is NOT a safe catalog watermark.
CREATE TABLE catalog_change_outbox (
    id BIGSERIAL PRIMARY KEY,
    root_id TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    file_id TEXT NOT NULL REFERENCES file_catalog(id) ON DELETE CASCADE
);
CREATE INDEX catalog_change_outbox_root_idx ON catalog_change_outbox(root_id,id);
CREATE TABLE catalog_change_state (
    root_id TEXT PRIMARY KEY REFERENCES roots(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0)
);
CREATE TABLE catalog_file_changes (
    root_id TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    file_id TEXT PRIMARY KEY REFERENCES file_catalog(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL CHECK (revision > 0)
);
CREATE UNIQUE INDEX catalog_file_changes_cursor_idx ON catalog_file_changes(root_id,revision);

-- +goose StatementBegin
CREATE FUNCTION record_catalog_head_change() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.captured_version_id IS NULL THEN RETURN NULL; END IF;
    IF TG_OP='UPDATE' AND ROW(NEW.captured_version_id,NEW.indexed_version_id,NEW.indexed_extraction_id,NEW.deleted)
        IS NOT DISTINCT FROM ROW(OLD.captured_version_id,OLD.indexed_version_id,OLD.indexed_extraction_id,OLD.deleted)
    THEN RETURN NULL; END IF;
    INSERT INTO catalog_change_outbox(root_id,file_id) VALUES(NEW.root_id,NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER catalog_head_change AFTER INSERT OR UPDATE OF captured_version_id,indexed_version_id,indexed_extraction_id,deleted
    ON file_catalog FOR EACH ROW EXECUTE FUNCTION record_catalog_head_change();

-- A proof change affects proof_current for its user even without a head change.
-- Keep each event until projected: coalescing uncommitted writes with an older
-- event can lose a change if a reader consumes that event before the commit.
-- +goose StatementBegin
CREATE FUNCTION record_catalog_proof_change() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE changed_root TEXT; changed_path TEXT;
BEGIN
    IF TG_OP='DELETE' THEN changed_root:=OLD.root_id; changed_path:=OLD.path;
    ELSE
        IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NULL; END IF;
        changed_root:=NEW.root_id; changed_path:=NEW.path;
    END IF;
    INSERT INTO catalog_change_outbox(root_id,file_id)
        SELECT f.root_id,f.id FROM file_catalog f JOIN roots r ON r.id=f.root_id
        WHERE f.root_id=changed_root AND f.path=changed_path AND r.deleting_at IS NULL;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER catalog_proof_change AFTER INSERT OR UPDATE OR DELETE ON file_content_proofs
    FOR EACH ROW EXECUTE FUNCTION record_catalog_proof_change();
INSERT INTO catalog_change_outbox(root_id,file_id)
    SELECT root_id,id FROM file_catalog WHERE captured_version_id IS NOT NULL;

-- +goose Down
DROP TRIGGER catalog_proof_change ON file_content_proofs;
DROP FUNCTION record_catalog_proof_change();
DROP TRIGGER catalog_head_change ON file_catalog;
DROP FUNCTION record_catalog_head_change();
DROP TABLE catalog_file_changes;
DROP TABLE catalog_change_state;
DROP TABLE catalog_change_outbox;
