-- +goose Up
-- Stop old embedding workers and remove their cache objects before deploying.
DROP TABLE embedding_packs;

-- +goose Down
-- Removed cache directories cannot be reconstructed.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Embedding cache removal cannot be rolled back'; END $$;
-- +goose StatementEnd
