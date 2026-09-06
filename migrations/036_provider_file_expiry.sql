-- +goose Up
-- A delete acknowledgement and passage of the provider's retention deadline
-- are different evidence. In particular, HTTP 403 proves neither deletion nor
-- expiry. Keep exact upload identities while recording which outcome occurred.
ALTER TABLE provider_files ADD COLUMN expires_at TIMESTAMPTZ;
ALTER TABLE provider_files ADD COLUMN expired_at TIMESTAMPTZ;
-- Legacy rows have no returned expirationTime. Registration occurs after the
-- upload, so 48 hours after registration is a conservative bound under the
-- uploaded Gemini Files retention policy (not the generated-result policy).
UPDATE provider_files SET expires_at=created_at+INTERVAL '48 hours';
ALTER TABLE provider_files ALTER COLUMN expires_at SET DEFAULT (NOW()+INTERVAL '48 hours');
ALTER TABLE provider_files ALTER COLUMN expires_at SET NOT NULL;
DROP INDEX provider_files_cleanup_idx;
CREATE INDEX provider_files_cleanup_idx ON provider_files(next_check_at)
    WHERE deleted_at IS NULL AND expired_at IS NULL;
CREATE INDEX provider_files_expiry_idx ON provider_files(expires_at)
    WHERE deleted_at IS NULL AND expired_at IS NULL;

-- +goose Down
DROP INDEX provider_files_cleanup_idx;
DROP INDEX provider_files_expiry_idx;
ALTER TABLE provider_files DROP COLUMN expired_at;
ALTER TABLE provider_files DROP COLUMN expires_at;
CREATE INDEX provider_files_cleanup_idx ON provider_files(next_check_at) WHERE deleted_at IS NULL;
