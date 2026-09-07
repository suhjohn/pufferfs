-- +goose Up
-- One API round trip; no process-local mutex or cached authorization. VOLATILE
-- obtains a fresh statement snapshot after waiting on the organization lock.
-- +goose StatementBegin
CREATE FUNCTION change_org_member(p_org TEXT, p_actor TEXT, p_key TEXT,
    p_target TEXT, p_role TEXT, p_mode TEXT)
RETURNS TABLE(user_id TEXT,email TEXT,name TEXT,avatar_url TEXT,role TEXT,joined_at TIMESTAMPTZ)
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    actor_role TEXT;
BEGIN
    IF p_mode NOT IN ('upsert','update','delete','join') OR
        (p_mode <> 'delete' AND p_role NOT IN ('owner','admin','editor','viewer')) THEN
        RAISE EXCEPTION 'invalid member change' USING ERRCODE='PF400';
    END IF;
    -- Compatible with foreign-key readers, but excludes other member changes.
    PERFORM id FROM organizations WHERE id=p_org FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'org not found' USING ERRCODE='PF404';
    END IF;
    IF p_actor <> '' THEN
        SELECT m.role INTO actor_role FROM org_members m
            WHERE m.org_id=p_org AND m.user_id=p_actor FOR SHARE;
        IF actor_role IS NULL OR actor_role NOT IN ('owner','admin') THEN
            RAISE EXCEPTION 'admin role required' USING ERRCODE='PF403';
        END IF;
        IF p_key <> '' THEN
            PERFORM id FROM api_keys WHERE id=p_key AND org_id=p_org AND api_keys.user_id=p_actor
                AND (expires_at IS NULL OR expires_at>clock_timestamp())
                AND (cardinality(scopes)=0 OR scopes && ARRAY['org:admin','admin','write','*']) FOR SHARE;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'org admin scope required' USING ERRCODE='PF403';
            END IF;
        END IF;
    END IF;
    SELECT m.role,m.joined_at,u.id,u.email,u.name,u.avatar_url
        INTO role,joined_at,user_id,email,name,avatar_url
        FROM org_members m JOIN users u ON u.id=m.user_id
        WHERE m.org_id=p_org AND m.user_id=p_target FOR UPDATE OF m;
    IF role IS NULL AND p_mode IN ('update','delete') THEN
        RAISE EXCEPTION 'member not found' USING ERRCODE='PF404';
    END IF;
    IF p_actor <> '' THEN
        IF p_mode='join' THEN
            RAISE EXCEPTION 'invalid member change' USING ERRCODE='PF400';
        END IF;
        IF p_actor=p_target AND (p_mode<>'upsert' OR role IS DISTINCT FROM p_role) THEN
            RAISE EXCEPTION 'cannot change your own membership' USING ERRCODE='PF403';
        END IF;
        IF actor_role='admin' AND (role IN ('owner','admin') OR
                (p_mode<>'delete' AND p_role IN ('owner','admin'))) THEN
            RAISE EXCEPTION 'cannot change that member role' USING ERRCODE='PF403';
        END IF;
        -- Only another currently locked owner may change an owner. Self-change
        -- is forbidden, so that actor preserves ownership without a count/query.
    END IF;
    -- Join preserves existing membership. Replayed upserts/updates do not write
    -- unchanged roles. Platform provisioning (empty actor) retains its authority
    -- to create or repair memberships, under the same organization lock.
    IF p_mode='delete' THEN
        DELETE FROM org_members m WHERE m.org_id=p_org AND m.user_id=p_target;
    ELSIF role IS NULL THEN
        SELECT u.id,u.email,u.name,u.avatar_url INTO user_id,email,name,avatar_url
            FROM users u WHERE u.id=p_target FOR KEY SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'user not found' USING ERRCODE='PF404';
        END IF;
        INSERT INTO org_members AS m(org_id,user_id,role) VALUES(p_org,p_target,p_role)
            RETURNING m.role,m.joined_at INTO role,joined_at;
    ELSIF p_mode<>'join' AND role<>p_role THEN
        UPDATE org_members m SET role=p_role WHERE m.org_id=p_org AND m.user_id=p_target
            RETURNING m.role,m.joined_at INTO role,joined_at;
    END IF;
    RETURN NEXT;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION change_org_member(TEXT,TEXT,TEXT,TEXT,TEXT,TEXT);
