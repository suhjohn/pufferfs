-- +goose Up
ALTER TABLE file_work ADD COLUMN org_id TEXT REFERENCES organizations(id) ON DELETE CASCADE;
UPDATE file_work w SET org_id=r.org_id FROM file_extractions e
    JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
    JOIN roots r ON r.id=f.root_id WHERE e.id=w.extraction_id;
ALTER TABLE file_work ALTER COLUMN org_id SET NOT NULL;
CREATE INDEX file_work_tenant_due_idx ON file_work(stage,org_id,next_attempt_at,id)
    WHERE status IN ('pending','running');

CREATE TABLE file_work_tenants (
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    stage TEXT NOT NULL CHECK(stage IN ('transform','index')),
    last_claimed TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    PRIMARY KEY(org_id,stage)
);
CREATE INDEX file_work_tenants_fair_idx ON file_work_tenants(stage,last_claimed,org_id);
INSERT INTO file_work_tenants(org_id,stage)
    SELECT id,stage FROM organizations CROSS JOIN (VALUES('transform'),('index')) s(stage);

-- Create tenant scheduling rows before any file work exists. No transition
-- holds a work row while trying to acquire its tenant's scheduling lock.
-- +goose StatementBegin
CREATE FUNCTION create_file_work_tenant() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO file_work_tenants(org_id,stage) VALUES(NEW.id,'transform'),(NEW.id,'index');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER file_work_tenant_created AFTER INSERT ON organizations
    FOR EACH ROW EXECUTE FUNCTION create_file_work_tenant();

-- Existing capture/provider/reindex writers keep one enqueue contract. The
-- scheduling tenant comes only from the immutable extraction ownership chain.
-- +goose StatementBegin
CREATE FUNCTION assign_file_work_tenant() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    SELECT r.org_id INTO NEW.org_id FROM file_extractions e
        JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
        JOIN roots r ON r.id=f.root_id WHERE e.id=NEW.extraction_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER file_work_tenant_assigned BEFORE INSERT OR UPDATE OF extraction_id ON file_work
    FOR EACH ROW EXECUTE FUNCTION assign_file_work_tenant();

-- +goose Down
DROP TRIGGER file_work_tenant_assigned ON file_work;
DROP FUNCTION assign_file_work_tenant();
DROP TRIGGER file_work_tenant_created ON organizations;
DROP FUNCTION create_file_work_tenant();
DROP TABLE file_work_tenants;
DROP INDEX file_work_tenant_due_idx;
ALTER TABLE file_work DROP COLUMN org_id;
