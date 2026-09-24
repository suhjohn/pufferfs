-- +goose Up
CREATE TABLE search_leases (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slots INTEGER NOT NULL CHECK (slots > 0),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX search_leases_expiry_idx ON search_leases(expires_at);
CREATE INDEX search_leases_org_idx ON search_leases(org_id);

-- +goose Down
DROP TABLE search_leases;
