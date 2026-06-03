package server

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// DB wraps the Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// NewDB creates a connection pool and runs migrations.
func NewDB(databaseURL string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	db := &DB{pool: pool}
	if err := db.migrate(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return db, nil
}

// Close shuts down the database pool.
func (db *DB) Close() {
	db.pool.Close()
}

func (db *DB) migrate() error {
	_, err := db.pool.Exec(context.Background(), `
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
	`)
	return err
}

// CreateRoot creates a new root metadata entry.
func (db *DB) CreateRoot(ctx context.Context, name, sourcePath string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{
		ID:         uuid.New().String(),
		Name:       name,
		SourcePath: sourcePath,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO roots (id, name, source_path, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		root.ID, root.Name, root.SourcePath, root.CreatedAt, root.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRoot retrieves a root by ID.
func (db *DB) GetRoot(ctx context.Context, id string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, source_path, created_at, updated_at FROM roots WHERE id = $1`, id,
	).Scan(&root.ID, &root.Name, &root.SourcePath, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRootByName retrieves a root by name.
func (db *DB) GetRootByName(ctx context.Context, name string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, source_path, created_at, updated_at FROM roots WHERE name = $1`, name,
	).Scan(&root.ID, &root.Name, &root.SourcePath, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// ListRoots returns all roots.
func (db *DB) ListRoots(ctx context.Context) ([]models.RootMetadata, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, name, source_path, created_at, updated_at FROM roots ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roots []models.RootMetadata
	for rows.Next() {
		var r models.RootMetadata
		if err := rows.Scan(&r.ID, &r.Name, &r.SourcePath, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	return roots, nil
}

// SaveState persists the filesystem state for a root.
func (db *DB) SaveState(ctx context.Context, rootID string, state map[string]models.FileState) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO root_states (root_id, state, updated_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (root_id) DO UPDATE SET state = $2, updated_at = NOW()`,
		rootID, state,
	)
	return err
}

// LoadState retrieves the filesystem state for a root.
func (db *DB) LoadState(ctx context.Context, rootID string) (map[string]models.FileState, error) {
	var state map[string]models.FileState
	err := db.pool.QueryRow(ctx,
		`SELECT state FROM root_states WHERE root_id = $1`, rootID,
	).Scan(&state)
	if err != nil {
		return make(map[string]models.FileState), nil // Return empty on not found
	}
	return state, nil
}

// UpdateRootTimestamp updates the updated_at on a root.
func (db *DB) UpdateRootTimestamp(ctx context.Context, rootID string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE roots SET updated_at = NOW() WHERE id = $1`, rootID,
	)
	return err
}

// GetCachedEmbeddings looks up cached embeddings by content hash.
// Returns a map from content_hash -> embedding (as float64 slice).
func (db *DB) GetCachedEmbeddings(ctx context.Context, hashes []string) (map[string][]float64, error) {
	result := make(map[string][]float64)
	if len(hashes) == 0 {
		return result, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT content_hash, embedding FROM embedding_cache WHERE content_hash = ANY($1)`,
		hashes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hash string
		var embBytes []byte
		if err := rows.Scan(&hash, &embBytes); err != nil {
			return nil, err
		}
		emb, err := decodeEmbedding(embBytes)
		if err != nil {
			return nil, fmt.Errorf("decoding embedding for %s: %w", hash, err)
		}
		result[hash] = emb
	}
	return result, nil
}

// SaveCachedEmbeddings stores embeddings in the cache.
func (db *DB) SaveCachedEmbeddings(ctx context.Context, entries map[string][]float64) error {
	if len(entries) == 0 {
		return nil
	}

	for hash, emb := range entries {
		embBytes := encodeEmbedding(emb)
		_, err := db.pool.Exec(ctx,
			`INSERT INTO embedding_cache (content_hash, embedding, created_at)
			 VALUES ($1, $2, NOW())
			 ON CONFLICT (content_hash) DO NOTHING`,
			hash, embBytes,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// encodeEmbedding converts a float64 slice to bytes (little-endian float64s).
func encodeEmbedding(emb []float64) []byte {
	buf := make([]byte, len(emb)*8)
	for i, v := range emb {
		bits := math.Float64bits(v)
		buf[i*8] = byte(bits)
		buf[i*8+1] = byte(bits >> 8)
		buf[i*8+2] = byte(bits >> 16)
		buf[i*8+3] = byte(bits >> 24)
		buf[i*8+4] = byte(bits >> 32)
		buf[i*8+5] = byte(bits >> 40)
		buf[i*8+6] = byte(bits >> 48)
		buf[i*8+7] = byte(bits >> 56)
	}
	return buf
}

// decodeEmbedding converts bytes back to a float64 slice.
func decodeEmbedding(buf []byte) ([]float64, error) {
	if len(buf)%8 != 0 {
		return nil, fmt.Errorf("invalid embedding bytes length: %d", len(buf))
	}
	emb := make([]float64, len(buf)/8)
	for i := range emb {
		bits := uint64(buf[i*8]) |
			uint64(buf[i*8+1])<<8 |
			uint64(buf[i*8+2])<<16 |
			uint64(buf[i*8+3])<<24 |
			uint64(buf[i*8+4])<<32 |
			uint64(buf[i*8+5])<<40 |
			uint64(buf[i*8+6])<<48 |
			uint64(buf[i*8+7])<<56
		emb[i] = math.Float64frombits(bits)
	}
	return emb, nil
}
