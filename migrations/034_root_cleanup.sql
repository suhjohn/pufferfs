-- +goose Up

-- Permanent deletion tombstones, not an execution queue. No cascading FKs:
-- recovery must outlive deletion of the root, organization and work ledger.
CREATE TABLE root_cleanup_targets (
    root_id TEXT NOT NULL CHECK (root_id <> ''),
    org_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('namespace', 'prefix')),
    target TEXT NOT NULL CHECK (target <> ''),
    vector_disabled BOOLEAN NOT NULL,
    mutation_ref TEXT NOT NULL DEFAULT '',
    mutation_record INT NOT NULL DEFAULT 0 CHECK (mutation_record >= 0),
    due_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_checked_at TIMESTAMPTZ,
    PRIMARY KEY (root_id, kind, target)
);
CREATE INDEX root_cleanup_due_idx ON root_cleanup_targets(due_at, root_id, kind, target);

-- Capture targets before cascades can discard namespace/generation identities.
-- These hooks also cover organization deletion and direct DB root deletion.
-- +goose StatementBegin
CREATE FUNCTION retain_root_cleanup_targets() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
        SELECT OLD.id,COALESCE(OLD.org_id,''),'namespace',namespace,OLD.vector_disabled
        FROM root_index_namespaces WHERE root_id=OLD.id
        ON CONFLICT DO NOTHING;
    IF COALESCE(OLD.org_id,'') <> '' THEN
        INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
            VALUES(OLD.id,OLD.org_id,'namespace','org-' || OLD.org_id || '-root-' || OLD.id,OLD.vector_disabled)
            ON CONFLICT DO NOTHING;
        INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
            SELECT OLD.id,OLD.org_id,'prefix',kind || '/' || OLD.org_id || '/' || OLD.id || '/',OLD.vector_disabled
            FROM unnest(ARRAY['sources','extractions','mutations']) AS kind
            ON CONFLICT DO NOTHING;
    END IF;
    INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
        SELECT OLD.id,COALESCE(OLD.org_id,''),'prefix',kind || '/' || OLD.id || '/',OLD.vector_disabled
        FROM unnest(ARRAY['files','bundles','states','chunks']) AS kind
        ON CONFLICT DO NOTHING;
    INSERT INTO root_cleanup_targets(root_id,org_id,kind,target,vector_disabled)
        SELECT OLD.id,COALESCE(OLD.org_id,''),'prefix','syncs/' || id || '/',OLD.vector_disabled
        FROM sync_generations WHERE root_id=OLD.id
        ON CONFLICT DO NOTHING;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER root_cleanup_before_delete BEFORE DELETE ON roots
    FOR EACH ROW EXECUTE FUNCTION retain_root_cleanup_targets();
CREATE TRIGGER root_cleanup_on_mark AFTER UPDATE OF deleting_at ON roots
    FOR EACH ROW WHEN (NEW.deleting_at IS NOT NULL) EXECUTE FUNCTION retain_root_cleanup_targets();

-- Organization FKs can cascade in different orders. Mark roots while all
-- child identities still exist, before *any* of the organization's cascades.
-- +goose StatementBegin
CREATE FUNCTION mark_roots_before_org_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE roots SET deleting_at=COALESCE(deleting_at,NOW()) WHERE org_id=OLD.id;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER root_cleanup_before_org_delete BEFORE DELETE ON organizations
    FOR EACH ROW EXECUTE FUNCTION mark_roots_before_org_delete();

-- IDs are permanent identities, not reusable names. Otherwise a later sweep
-- could erase a newly created root's objects. API creation already uses UUIDs.
-- AFTER INSERT checks only after the roots PK has resolved concurrent deletes.
-- +goose StatementBegin
CREATE FUNCTION reject_deleted_root_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id IS DISTINCT FROM OLD.id OR NEW.org_id IS DISTINCT FROM OLD.org_id THEN
            RAISE EXCEPTION 'root identity and organization are immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM root_cleanup_targets WHERE root_id=NEW.id) THEN
        RAISE EXCEPTION 'deleted root identity cannot be reused';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER root_cleanup_reject_reuse AFTER INSERT ON roots
    FOR EACH ROW EXECUTE FUNCTION reject_deleted_root_identity();
CREATE TRIGGER root_cleanup_immutable_identity BEFORE UPDATE OF id,org_id ON roots
    FOR EACH ROW EXECUTE FUNCTION reject_deleted_root_identity();

-- Resume roots already marked for deletion when this migration is applied.
UPDATE roots SET deleting_at=deleting_at WHERE deleting_at IS NOT NULL;

-- +goose Down
DROP TRIGGER root_cleanup_immutable_identity ON roots;
DROP TRIGGER root_cleanup_reject_reuse ON roots;
DROP TRIGGER root_cleanup_before_org_delete ON organizations;
DROP TRIGGER root_cleanup_on_mark ON roots;
DROP TRIGGER root_cleanup_before_delete ON roots;
DROP FUNCTION reject_deleted_root_identity();
DROP FUNCTION mark_roots_before_org_delete();
DROP FUNCTION retain_root_cleanup_targets();
DROP TABLE root_cleanup_targets;
