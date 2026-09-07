-- +goose Up
-- Shared parent locks prevent deletion, without serializing unrelated groups.
-- Unique constraints arbitrate concurrent inserts; VOLATILE reads the committed
-- winner on the next loop iteration. No-op retries never UPDATE a data row.
-- +goose StatementBegin
CREATE FUNCTION upsert_org_group(p_org TEXT,p_id TEXT,p_name TEXT,p_external TEXT)
RETURNS SETOF groups LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    result groups;
BEGIN
    PERFORM id FROM organizations WHERE id=p_org FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'org not found' USING ERRCODE='PF404';
    END IF;
    LOOP
        -- Preserve external-ID precedence over the caller's optional ID. Name
        -- matches only detect a conflicting identity; names are not upsert keys.
        SELECT g.* INTO result FROM groups g
            WHERE g.id=p_id OR (g.org_id=p_org AND
                ((p_external<>'' AND g.external_id=p_external) OR g.name=p_name))
            ORDER BY CASE WHEN g.org_id=p_org AND p_external<>'' AND g.external_id=p_external THEN 0
                WHEN g.id=p_id THEN 1 ELSE 2 END
            LIMIT 1 FOR UPDATE;
        IF FOUND THEN
            IF result.org_id<>p_org THEN
                RAISE EXCEPTION 'group ID belongs to another org' USING ERRCODE='PF409';
            END IF;
            IF result.id<>p_id AND (p_external='' OR result.external_id<>p_external) THEN
                RAISE EXCEPTION 'group name already exists' USING ERRCODE='PF409';
            END IF;
            IF (result.name,result.external_id) IS DISTINCT FROM (p_name,p_external) THEN
                UPDATE groups SET name=p_name,external_id=p_external,updated_at=NOW()
                    WHERE id=result.id RETURNING * INTO result;
            END IF;
            RETURN NEXT result;
            RETURN;
        END IF;
        INSERT INTO groups(id,org_id,name,external_id) VALUES(p_id,p_org,p_name,p_external)
            ON CONFLICT DO NOTHING RETURNING * INTO result;
        IF FOUND THEN
            RETURN NEXT result;
            RETURN;
        END IF;
    END LOOP;
EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'group name or external ID already exists' USING ERRCODE='PF409';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION add_org_group_member(p_org TEXT,p_group TEXT,p_user TEXT)
RETURNS SETOF group_members LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    result group_members;
BEGIN
    -- Parent-first locking also coordinates with organization deletion cascades.
    PERFORM g.id FROM (SELECT id FROM organizations WHERE id=p_org FOR KEY SHARE) o
        JOIN groups g ON g.org_id=o.id WHERE g.id=p_group FOR KEY SHARE OF g;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'group not found' USING ERRCODE='PF404';
    END IF;
    PERFORM user_id FROM org_members WHERE org_id=p_org AND user_id=p_user FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member not found' USING ERRCODE='PF404';
    END IF;
    LOOP
        SELECT * INTO result FROM group_members
            WHERE org_id=p_org AND group_id=p_group AND user_id=p_user FOR KEY SHARE;
        IF FOUND THEN
            RETURN NEXT result;
            RETURN;
        END IF;
        INSERT INTO group_members(org_id,group_id,user_id) VALUES(p_org,p_group,p_user)
            ON CONFLICT DO NOTHING RETURNING * INTO result;
        IF FOUND THEN
            RETURN NEXT result;
            RETURN;
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION add_org_group_member(TEXT,TEXT,TEXT);
DROP FUNCTION upsert_org_group(TEXT,TEXT,TEXT,TEXT);
