-- +goose Up

CREATE TABLE IF NOT EXISTS user_identities (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,
    provider_id    TEXT NOT NULL DEFAULT '',
    email          TEXT NOT NULL,
    email_verified BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_identities_provider_id
    ON user_identities(provider, provider_id)
    WHERE provider_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_identities_provider_email
    ON user_identities(provider, email)
    WHERE provider_id = '';

CREATE INDEX IF NOT EXISTS idx_user_identities_user
    ON user_identities(user_id);

INSERT INTO user_identities (id, user_id, provider, provider_id, email, email_verified, created_at, last_seen_at)
SELECT
    'uid_' || substr(md5(id || ':' || provider || ':' || COALESCE(provider_id, '') || ':' || email), 1, 24),
    id,
    provider,
    COALESCE(provider_id, ''),
    email,
    TRUE,
    created_at,
    NOW()
FROM users
WHERE provider <> '' OR COALESCE(provider_id, '') <> ''
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS email_login_challenges (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL,
    code_hash       TEXT NOT NULL,
    flow            TEXT NOT NULL DEFAULT 'web',
    cli_redirect_uri TEXT NOT NULL DEFAULT '',
    attempts        INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 5,
    request_ip_hash TEXT NOT NULL DEFAULT '',
    user_agent_hash TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_email_login_challenges_email_created
    ON email_login_challenges(email, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_email_login_challenges_expires
    ON email_login_challenges(expires_at);

-- +goose Down

DROP TABLE IF EXISTS email_login_challenges;
DROP TABLE IF EXISTS user_identities;
