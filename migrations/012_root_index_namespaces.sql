-- +goose Up

CREATE TABLE IF NOT EXISTS root_index_namespaces (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id     TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    namespace   TEXT NOT NULL UNIQUE,
    shard_index INT NOT NULL,
    shard_count INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at  TIMESTAMPTZ,
    UNIQUE(root_id, shard_index)
);

CREATE INDEX IF NOT EXISTS idx_root_index_namespaces_root
    ON root_index_namespaces(root_id, shard_index);

CREATE INDEX IF NOT EXISTS idx_root_index_namespaces_org
    ON root_index_namespaces(org_id);

INSERT INTO root_index_namespaces (id, org_id, root_id, namespace, shard_index, shard_count)
SELECT
    'rin_' || substr(md5(r.org_id || ':' || r.id || ':0'), 1, 24),
    r.org_id,
    r.id,
    'org-' || r.org_id || '-root-' || r.id,
    0,
    1
FROM roots r
WHERE r.org_id IS NOT NULL
ON CONFLICT (root_id, shard_index) DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS root_index_namespaces;
