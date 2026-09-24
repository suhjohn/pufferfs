-- +goose Up
ALTER TABLE file_extractions DROP CONSTRAINT file_extractions_row_format_check;
ALTER TABLE file_extractions ADD CONSTRAINT file_extractions_row_format_check CHECK(row_format IN (1,2));
ALTER TABLE file_extractions ADD COLUMN source_verified BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE file_extractions SET source_verified=TRUE WHERE status='complete';
ALTER TABLE file_extractions ADD COLUMN append_checkpoint_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE file_extractions ADD COLUMN append_chunk_count BIGINT NOT NULL DEFAULT 0 CHECK(append_chunk_count>=0);
ALTER TABLE file_extractions ADD COLUMN provider_assembly_cursor BIGINT NOT NULL DEFAULT 0 CHECK(provider_assembly_cursor>=0);
ALTER TABLE file_extractions ADD COLUMN provider_assembly_chunk_count BIGINT NOT NULL DEFAULT 0 CHECK(provider_assembly_chunk_count>=0);
ALTER TABLE file_work ADD COLUMN transform_checkpoint_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE file_work ADD COLUMN transform_cursor BIGINT NOT NULL DEFAULT 0 CHECK(transform_cursor>=0);
CREATE INDEX provider_batches_failed_extraction_idx ON provider_batches(extraction_id) WHERE status='failed';

-- A segment's row/artifact identity never changes when an append reuses it.
-- Membership, rather than the segment's original version, controls visibility.
CREATE TABLE file_segments (
    id TEXT PRIMARY KEY,
    file_id TEXT NOT NULL REFERENCES file_catalog(id) ON DELETE CASCADE,
    owner_extraction_id TEXT NOT NULL REFERENCES file_extractions(id) ON DELETE CASCADE,
    ordinal_start BIGINT NOT NULL CHECK(ordinal_start>=0),
    chunk_count INTEGER NOT NULL CHECK(chunk_count BETWEEN 1 AND 64),
    chunks_ref TEXT NOT NULL,
    line_start BIGINT,
    line_end BIGINT,
    page_start BIGINT,
    page_end BIGINT,
    indexed_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    index_cleanup_due_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(owner_extraction_id,ordinal_start),
    CHECK((line_start IS NULL AND line_end IS NULL) OR (line_start IS NOT NULL AND line_end IS NOT NULL AND line_start>0 AND line_end>=line_start)),
    CHECK((page_start IS NULL AND page_end IS NULL) OR (page_start IS NOT NULL AND page_end IS NOT NULL AND page_start>=0 AND page_end>=page_start))
);
CREATE INDEX file_segments_owner_idx ON file_segments(owner_extraction_id,retired_at);
CREATE INDEX file_segments_lines_idx ON file_segments(file_id,line_end,ordinal_start)
    WHERE retired_at IS NULL AND line_end IS NOT NULL;
CREATE INDEX file_segments_pages_idx ON file_segments(file_id,page_end,ordinal_start)
    WHERE retired_at IS NULL AND page_end IS NOT NULL;
CREATE INDEX file_segments_cleanup_idx ON file_segments(index_cleanup_due_at,id) WHERE retired_at IS NOT NULL;
CREATE TABLE extraction_segments (
    extraction_id TEXT NOT NULL REFERENCES file_extractions(id) ON DELETE CASCADE,
    ordinal_start BIGINT NOT NULL CHECK(ordinal_start>=0),
    segment_id TEXT NOT NULL REFERENCES file_segments(id) ON DELETE CASCADE,
    PRIMARY KEY(extraction_id,ordinal_start),
    UNIQUE(extraction_id,segment_id)
);
CREATE INDEX extraction_segments_references_idx ON extraction_segments(segment_id,extraction_id);

-- +goose Down
-- Reused row identities cannot be interpreted by old publication/read code.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'segment migration requires coordinated backup restore'; END $$;
-- +goose StatementEnd
