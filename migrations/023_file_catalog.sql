-- +goose Up

-- Durable content is outside sync staging. Object bodies, text and vectors
-- live in S3; these tables contain ownership, references and progress only.
CREATE TABLE source_objects (
    object_key TEXT PRIMARY KEY,
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    root_id TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX source_objects_root_idx ON source_objects(root_id);

CREATE TABLE file_catalog (
    id TEXT PRIMARY KEY,
    root_id TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    captured_version_id TEXT,
    indexed_version_id TEXT,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (root_id, path)
);

CREATE TABLE file_versions (
    id TEXT PRIMARY KEY,
    file_id TEXT NOT NULL REFERENCES file_catalog(id) ON DELETE CASCADE,
    sequence BIGSERIAL UNIQUE NOT NULL,
    capture_id TEXT NOT NULL,
    previous_version_id TEXT,
    content_hash TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    source_manifest_ref TEXT NOT NULL,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (file_id, capture_id),
    UNIQUE (file_id, id),
    CHECK (NOT deleted OR (size_bytes = 0 AND source_manifest_ref = '')),
    CHECK (deleted OR source_manifest_ref <> '')
);
ALTER TABLE file_catalog ADD CONSTRAINT file_catalog_captured_version_fk
    FOREIGN KEY (id, captured_version_id) REFERENCES file_versions(file_id, id)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE file_catalog ADD CONSTRAINT file_catalog_indexed_version_fk
    FOREIGN KEY (id, indexed_version_id) REFERENCES file_versions(file_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE file_extractions (
    id TEXT PRIMARY KEY,
    version_id TEXT NOT NULL REFERENCES file_versions(id) ON DELETE CASCADE,
    revision TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'waiting_provider', 'complete', 'failed', 'superseded')),
    chunks_ref TEXT NOT NULL DEFAULT '',
    chunk_count BIGINT NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    error TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (version_id, revision)
);

-- Delivery/recovery ledger, NOT a database work queue. SQS consumers receive
-- work IDs; reconciliation republishes incomplete handoffs to SQS.
CREATE TABLE file_work (
    id TEXT PRIMARY KEY,
    extraction_id TEXT NOT NULL REFERENCES file_extractions(id) ON DELETE CASCADE,
    stage TEXT NOT NULL CHECK (stage IN ('transform', 'index')),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'waiting_provider', 'complete', 'failed', 'superseded')),
    attempt_token TEXT,
    attempt_count INT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    enqueued_at TIMESTAMPTZ,
    invocation_id TEXT,
    mutation_ref TEXT NOT NULL DEFAULT '',
    acknowledged_batches INT NOT NULL DEFAULT 0 CHECK (acknowledged_batches >= 0),
    error TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (extraction_id, stage)
);
CREATE INDEX file_work_reconciliation_idx ON file_work(updated_at)
    WHERE status IN ('pending', 'running');

CREATE TABLE provider_batches (
    id TEXT PRIMARY KEY,
    provider_job_id TEXT UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('preparing', 'submitted', 'complete', 'failed')),
    model TEXT NOT NULL,
    input_file_id TEXT,
    output_ref TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX provider_batches_pending_idx ON provider_batches(updated_at)
    WHERE status IN ('preparing', 'submitted');

CREATE TABLE provider_requests (
    request_key TEXT PRIMARY KEY,
    extraction_id TEXT NOT NULL REFERENCES file_extractions(id) ON DELETE CASCADE,
    batch_id TEXT REFERENCES provider_batches(id),
    ordinal INT NOT NULL CHECK (ordinal >= 0),
    location JSONB NOT NULL,
    input_file_id TEXT,
    result_ref TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'submitted', 'complete', 'failed')),
    error TEXT NOT NULL DEFAULT '',
    UNIQUE (extraction_id, ordinal)
);
CREATE INDEX provider_requests_batch_idx ON provider_requests(batch_id);

CREATE TABLE embedding_locations (
    org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    model_revision TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    object_key TEXT NOT NULL,
    byte_offset BIGINT NOT NULL CHECK (byte_offset >= 0),
    byte_length BIGINT NOT NULL CHECK (byte_length > 0),
    dimensions INT NOT NULL CHECK (dimensions > 0),
    PRIMARY KEY (org_id, model_revision, content_hash)
);

-- +goose Down
DROP TABLE embedding_locations;
DROP TABLE provider_requests;
DROP TABLE provider_batches;
DROP TABLE file_work;
DROP TABLE file_extractions;
ALTER TABLE file_catalog DROP CONSTRAINT file_catalog_indexed_version_fk;
ALTER TABLE file_catalog DROP CONSTRAINT file_catalog_captured_version_fk;
DROP TABLE file_versions;
DROP TABLE file_catalog;
DROP TABLE source_objects;
