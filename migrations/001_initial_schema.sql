-- +goose Up
CREATE TABLE IF NOT EXISTS roots (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    source_path TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS root_states (
    root_id    TEXT PRIMARY KEY REFERENCES roots(id) ON DELETE CASCADE,
    state      JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS embedding_cache (
    content_hash TEXT PRIMARY KEY,
    embedding    BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS embedding_cache;
DROP TABLE IF EXISTS root_states;
DROP TABLE IF EXISTS roots;
