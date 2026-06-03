package server

import (
	"context"
	"fmt"
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
