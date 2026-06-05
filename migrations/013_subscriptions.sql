-- +goose Up

CREATE TABLE IF NOT EXISTS subscriptions (
    org_id                 TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    stripe_customer_id     TEXT NOT NULL DEFAULT '',
    stripe_subscription_id TEXT NOT NULL DEFAULT '',
    plan                   TEXT NOT NULL DEFAULT 'free',
    status                 TEXT NOT NULL DEFAULT 'none',
    current_period_end     TIMESTAMPTZ,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_subscriptions_customer
    ON subscriptions(stripe_customer_id)
    WHERE stripe_customer_id <> '';

-- +goose Down

DROP TABLE IF EXISTS subscriptions;
