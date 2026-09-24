-- +goose Up
-- All API/worker replicas using one provider account share this model budget.
-- Reservations survive ambiguous responses and crashes for their full window.
CREATE TABLE provider_capacity (
    model TEXT PRIMARY KEY,
    requests_per_minute INTEGER NOT NULL CHECK(requests_per_minute>0),
    tokens_per_minute BIGINT NOT NULL CHECK(tokens_per_minute>0),
    blocked_until TIMESTAMPTZ NOT NULL DEFAULT '-infinity'
);
CREATE TABLE provider_reservations (
    id TEXT PRIMARY KEY,
    model TEXT NOT NULL REFERENCES provider_capacity(model) ON DELETE CASCADE,
    org_id TEXT NOT NULL,
    tokens BIGINT NOT NULL CHECK(tokens>=0),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX provider_reservations_window_idx ON provider_reservations(model,expires_at);
CREATE TABLE provider_waiters (
    model TEXT NOT NULL REFERENCES provider_capacity(model) ON DELETE CASCADE,
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    tokens BIGINT NOT NULL CHECK(tokens>0),
    waiting_until TIMESTAMPTZ NOT NULL,
    last_served TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    PRIMARY KEY(model,org_id)
);

-- Both languages call this function before EACH network attempt. The row lock
-- is held only for admission, never during provider IO. Return seconds to retry
-- (zero means admitted). Expired tenants cannot block live work indefinitely.
-- +goose StatementBegin
CREATE FUNCTION reserve_provider_capacity(p_model TEXT,p_org TEXT,p_id TEXT,p_tokens BIGINT,
    p_requests INTEGER,p_limit BIGINT,p_wait_seconds INTEGER)
    RETURNS TABLE(delay DOUBLE PRECISION, available_tokens BIGINT) LANGUAGE plpgsql AS $$
DECLARE capacity provider_capacity; used_requests BIGINT; used_tokens BIGINT;
    first_expiry TIMESTAMPTZ; selected TEXT; admission_time TIMESTAMPTZ;
BEGIN
    IF p_tokens<=0 OR p_requests<=0 OR p_limit<=0 OR p_tokens>p_limit OR p_wait_seconds NOT BETWEEN 1 AND 90 THEN
        RAISE EXCEPTION 'provider request exceeds configured capacity';
    END IF;
    INSERT INTO provider_capacity(model,requests_per_minute,tokens_per_minute)
        VALUES(p_model,p_requests,p_limit) ON CONFLICT DO NOTHING;
    SELECT * INTO capacity FROM provider_capacity WHERE model=p_model FOR UPDATE;
    IF capacity.requests_per_minute<>p_requests OR capacity.tokens_per_minute<>p_limit THEN
        RAISE EXCEPTION 'provider budget configuration differs between replicas';
    END IF;
    admission_time:=clock_timestamp();
    DELETE FROM provider_reservations WHERE model=p_model AND expires_at<=admission_time;
    INSERT INTO provider_waiters(model,org_id,tokens,waiting_until)
        VALUES(p_model,p_org,p_tokens,admission_time+make_interval(secs=>p_wait_seconds))
        ON CONFLICT(model,org_id) DO UPDATE SET tokens=EXCLUDED.tokens,
            waiting_until=GREATEST(provider_waiters.waiting_until,EXCLUDED.waiting_until);
    IF capacity.blocked_until>admission_time THEN
        RETURN QUERY SELECT EXTRACT(EPOCH FROM capacity.blocked_until-admission_time)::DOUBLE PRECISION,0::BIGINT;
        RETURN;
    END IF;
    SELECT count(*),COALESCE(sum(tokens),0),min(expires_at)
        INTO used_requests,used_tokens,first_expiry FROM provider_reservations WHERE model=p_model;
    IF used_requests>=p_requests THEN
        RETURN QUERY SELECT GREATEST(0.1,EXTRACT(EPOCH FROM first_expiry-admission_time))::DOUBLE PRECISION,0::BIGINT;
        RETURN;
    END IF;
    IF used_tokens+p_tokens>p_limit THEN
        -- Successful in-flight calls may settle below their reservation well
        -- before the window expires. Recheck without idling a whole minute.
        RETURN QUERY SELECT LEAST(5,GREATEST(0.1,EXTRACT(EPOCH FROM first_expiry-admission_time)))::DOUBLE PRECISION,
            GREATEST(0,p_limit-used_tokens);
        RETURN;
    END IF;
    SELECT org_id INTO selected FROM provider_waiters
        WHERE model=p_model AND waiting_until>admission_time AND tokens<=p_limit-used_tokens
        ORDER BY last_served,org_id LIMIT 1;
    IF selected<>p_org THEN
        RETURN QUERY SELECT 1::DOUBLE PRECISION,0::BIGINT;
        RETURN;
    END IF;
    INSERT INTO provider_reservations(id,model,org_id,tokens,expires_at)
        VALUES(p_id,p_model,p_org,p_tokens,admission_time+INTERVAL '60 seconds');
    UPDATE provider_waiters SET last_served=admission_time,waiting_until=admission_time
        WHERE model=p_model AND org_id=p_org;
    RETURN QUERY SELECT 0::DOUBLE PRECISION,0::BIGINT;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION reserve_provider_capacity(TEXT,TEXT,TEXT,BIGINT,INTEGER,BIGINT,INTEGER);
DROP TABLE provider_waiters;
DROP TABLE provider_reservations;
DROP TABLE provider_capacity;
