-- +goose Up
-- Per-user captured hashes preserve the existing path/hash proof semantics.
-- Deleted rows suppress fallback to an older root-wide proof during migration.
CREATE TABLE file_content_proofs (
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_id TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    content_hash TEXT NOT NULL,
    deleted BOOLEAN NOT NULL,
    PRIMARY KEY (org_id, user_id, root_id, path)
);

-- +goose Down
DROP TABLE file_content_proofs;
