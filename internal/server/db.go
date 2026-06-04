package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	// pgx stdlib adapter for goose
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// DB wraps the Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
}

type SyncGeneration struct {
	ID                string
	RootID            string
	BaseGenerationID  string
	Seq               int64
	BaseGenerationSeq int64
}

var errSyncInProgress = errors.New("sync already in progress for root")

// NewDB creates a connection pool and runs migrations.
func NewDB(databaseURL string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	db := &DB{pool: pool}
	if err := db.runMigrations(databaseURL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return db, nil
}

// Close shuts down the database pool.
func (db *DB) Close() {
	db.pool.Close()
}

func (db *DB) runMigrations(databaseURL string) error {
	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "migrations"
	}

	// Check if migrations directory exists; fall back to inline if not
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return db.migrateFallback()
	}

	gooseDB, err := goose.OpenDBWithDriver("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer gooseDB.Close()

	if err := goose.Up(gooseDB, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// migrateFallback runs inline SQL migrations when goose files aren't available.
func (db *DB) migrateFallback() error {
	_, err := db.pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS organizations (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			slug       TEXT NOT NULL UNIQUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS users (
			id          TEXT PRIMARY KEY,
			email       TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL DEFAULT '',
			avatar_url  TEXT NOT NULL DEFAULT '',
			provider    TEXT NOT NULL DEFAULT 'google',
			provider_id TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS org_members (
			org_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role      TEXT NOT NULL DEFAULT 'viewer',
			joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, user_id)
		);

		CREATE TABLE IF NOT EXISTS api_keys (
			id         TEXT PRIMARY KEY,
			org_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			key_hash   TEXT NOT NULL UNIQUE,
			name       TEXT NOT NULL DEFAULT '',
			scopes     TEXT[] NOT NULL DEFAULT '{}',
			expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS roots (
			id          TEXT PRIMARY KEY,
			org_id      TEXT REFERENCES organizations(id) ON DELETE CASCADE,
			name        TEXT NOT NULL,
			source_path TEXT NOT NULL,
			simhash     TEXT NOT NULL DEFAULT '',
			visible_generation_id TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE roots ADD COLUMN IF NOT EXISTS visible_generation_id TEXT NOT NULL DEFAULT '';

		CREATE TABLE IF NOT EXISTS root_states (
			root_id    TEXT PRIMARY KEY REFERENCES roots(id) ON DELETE CASCADE,
			state      JSONB NOT NULL DEFAULT '{}',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS embedding_cache (
			org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			content_hash TEXT NOT NULL,
			embedding    BYTEA NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, content_hash)
		);

		CREATE TABLE IF NOT EXISTS root_acls (
			id          TEXT PRIMARY KEY,
			org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id     TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			path_prefix TEXT NOT NULL DEFAULT '/',
			grant_to    TEXT NOT NULL,
			permission  TEXT NOT NULL DEFAULT 'read',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS sync_jobs (
			id           TEXT PRIMARY KEY,
			org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id      TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			user_id      TEXT NOT NULL REFERENCES users(id),
			status       TEXT NOT NULL DEFAULT 'pending',
			total_files  INT NOT NULL DEFAULT 0,
			processed    INT NOT NULL DEFAULT 0,
			errors       JSONB NOT NULL DEFAULT '[]',
			started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			finished_at  TIMESTAMPTZ
		);

		CREATE TABLE IF NOT EXISTS sync_generations (
			id                 TEXT PRIMARY KEY,
			org_id             TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id            TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			sync_job_id        TEXT REFERENCES sync_jobs(id) ON DELETE SET NULL,
			base_generation_id TEXT NOT NULL DEFAULT '',
			seq                BIGSERIAL,
			base_generation_seq BIGINT NOT NULL DEFAULT 0,
			status             TEXT NOT NULL DEFAULT 'building',
			manifest_ref       TEXT NOT NULL DEFAULT '',
			created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			visible_at         TIMESTAMPTZ
		);
		ALTER TABLE sync_generations ADD COLUMN IF NOT EXISTS seq BIGSERIAL;
		ALTER TABLE sync_generations ADD COLUMN IF NOT EXISTS base_generation_seq BIGINT NOT NULL DEFAULT 0;
		CREATE UNIQUE INDEX IF NOT EXISTS sync_generations_seq_idx ON sync_generations(seq);

		CREATE TABLE IF NOT EXISTS content_proofs (
			org_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			root_id    TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			root_hash  TEXT NOT NULL DEFAULT '',
			proof      JSONB NOT NULL DEFAULT '{}',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, user_id, root_id)
		);
	`)
	return err
}

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

func (db *DB) CreateOrganization(ctx context.Context, name, slug string) (*models.Organization, error) {
	org := &models.Organization{
		ID:        uuid.New().String(),
		Name:      name,
		Slug:      slug,
		CreatedAt: time.Now(),
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, created_at) VALUES ($1, $2, $3, $4)`,
		org.ID, org.Name, org.Slug, org.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return org, nil
}

func (db *DB) GetOrganization(ctx context.Context, id string) (*models.Organization, error) {
	org := &models.Organization{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, created_at FROM organizations WHERE id = $1`, id,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedAt)
	if err != nil {
		return nil, err
	}
	return org, nil
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// UpsertUser creates or updates a user from OAuth info, returning the user ID.
// Also creates a personal org if the user is new.
func (db *DB) UpsertUser(ctx context.Context, info auth.UserInfo, provider string) (userID, orgID string, role auth.Role, err error) {
	// Check if user exists
	var existingID string
	err = db.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE email = $1`, info.Email,
	).Scan(&existingID)

	if err == nil {
		// User exists — update profile
		_, err = db.pool.Exec(ctx,
			`UPDATE users SET name = $1, avatar_url = $2, provider_id = $3 WHERE id = $4`,
			info.Name, info.Picture, info.ID, existingID,
		)
		if err != nil {
			return "", "", "", fmt.Errorf("updating user: %w", err)
		}

		// Find their org membership (pick first org)
		err = db.pool.QueryRow(ctx,
			`SELECT org_id, role FROM org_members WHERE user_id = $1 LIMIT 1`, existingID,
		).Scan(&orgID, &role)
		if err != nil {
			return "", "", "", fmt.Errorf("looking up org membership: %w", err)
		}
		return existingID, orgID, role, nil
	}

	// New user — create user + personal org
	userID = uuid.New().String()
	_, err = db.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, avatar_url, provider, provider_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, info.Email, info.Name, info.Picture, provider, info.ID,
	)
	if err != nil {
		return "", "", "", fmt.Errorf("creating user: %w", err)
	}

	// Create a personal org
	slug := strings.Split(info.Email, "@")[0]
	org, err := db.CreateOrganization(ctx, info.Name+"'s Workspace", slug)
	if err != nil {
		return "", "", "", fmt.Errorf("creating personal org: %w", err)
	}

	// Add user as owner
	_, err = db.pool.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		org.ID, userID,
	)
	if err != nil {
		return "", "", "", fmt.Errorf("adding org membership: %w", err)
	}

	return userID, org.ID, auth.RoleOwner, nil
}

// GetUser retrieves a user by ID.
func (db *DB) GetUser(ctx context.Context, id string) (*models.User, error) {
	u := &models.User{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, email, name, avatar_url, provider, created_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Provider, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// API Keys
// ---------------------------------------------------------------------------

// CreateAPIKey creates a new API key for a user in an org.
func (db *DB) CreateAPIKey(ctx context.Context, orgID, userID, name string, scopes []string) (rawKey string, err error) {
	id := uuid.New().String()
	rawKey = "pfs_" + uuid.New().String()
	keyHash := auth.HashAPIKey(rawKey)

	_, err = db.pool.Exec(ctx,
		`INSERT INTO api_keys (id, org_id, user_id, key_hash, name, scopes, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		id, orgID, userID, keyHash, name, scopes,
	)
	if err != nil {
		return "", err
	}
	return rawKey, nil
}

// ResolveAPIKey looks up an API key by its hash and returns the associated identity.
func (db *DB) ResolveAPIKey(ctx context.Context, keyHash string) (*auth.Identity, error) {
	var orgID, userID, role string
	err := db.pool.QueryRow(ctx,
		`SELECT ak.org_id, ak.user_id, om.role
		 FROM api_keys ak
		 JOIN org_members om ON om.org_id = ak.org_id AND om.user_id = ak.user_id
		 WHERE ak.key_hash = $1
		   AND (ak.expires_at IS NULL OR ak.expires_at > NOW())`,
		keyHash,
	).Scan(&orgID, &userID, &role)
	if err != nil {
		return nil, err
	}

	var email string
	_ = db.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)

	return &auth.Identity{
		UserID: userID,
		OrgID:  orgID,
		Role:   auth.Role(role),
		Email:  email,
	}, nil
}

// ListAPIKeys lists all API keys for a user in an org.
func (db *DB) ListAPIKeys(ctx context.Context, orgID, userID string) ([]models.APIKey, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, name, scopes, created_at, expires_at
		 FROM api_keys WHERE org_id = $1 AND user_id = $2 ORDER BY created_at DESC`,
		orgID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []models.APIKey
	for rows.Next() {
		var k models.APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Scopes, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// DeleteAPIKey deletes an API key by ID (scoped to org).
func (db *DB) DeleteAPIKey(ctx context.Context, orgID, keyID string) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM api_keys WHERE id = $1 AND org_id = $2`, keyID, orgID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Org Members
// ---------------------------------------------------------------------------

// AddOrgMember adds a user to an org with a role.
func (db *DB) AddOrgMember(ctx context.Context, orgID, userID string, role auth.Role) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = $3`,
		orgID, userID, string(role),
	)
	return err
}

// ListOrgMembers lists all members of an org.
func (db *DB) ListOrgMembers(ctx context.Context, orgID string) ([]models.OrgMember, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT u.id, u.email, u.name, u.avatar_url, om.role, om.joined_at
		 FROM org_members om JOIN users u ON u.id = om.user_id
		 WHERE om.org_id = $1 ORDER BY om.joined_at`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []models.OrgMember
	for rows.Next() {
		var m models.OrgMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.AvatarURL, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, nil
}

// RemoveOrgMember removes a user from an org.
func (db *DB) RemoveOrgMember(ctx context.Context, orgID, userID string) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Roots (org-scoped)
// ---------------------------------------------------------------------------

// CreateRoot creates a new root metadata entry scoped to an org.
func (db *DB) CreateRoot(ctx context.Context, orgID, name, sourcePath string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{
		ID:         uuid.New().String(),
		OrgID:      orgID,
		Name:       name,
		SourcePath: sourcePath,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO roots (id, org_id, name, source_path, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		root.ID, root.OrgID, root.Name, root.SourcePath, root.CreatedAt, root.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRoot retrieves a root by ID, scoped to an org.
func (db *DB) GetRoot(ctx context.Context, orgID, id string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, name, source_path, created_at, updated_at
		 FROM roots WHERE id = $1 AND org_id = $2`, id, orgID,
	).Scan(&root.ID, &root.OrgID, &root.Name, &root.SourcePath, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRootByName retrieves a root by name, scoped to an org.
func (db *DB) GetRootByName(ctx context.Context, orgID, name string) (*models.RootMetadata, error) {
	root := &models.RootMetadata{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, name, source_path, created_at, updated_at
		 FROM roots WHERE name = $1 AND org_id = $2`, name, orgID,
	).Scan(&root.ID, &root.OrgID, &root.Name, &root.SourcePath, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// ListRoots returns all roots for an org.
func (db *DB) ListRoots(ctx context.Context, orgID string) ([]models.RootMetadata, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, name, source_path, created_at, updated_at
		 FROM roots WHERE org_id = $1 ORDER BY created_at DESC`, orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roots []models.RootMetadata
	for rows.Next() {
		var r models.RootMetadata
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.SourcePath, &r.CreatedAt, &r.UpdatedAt); err != nil {
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
		return make(map[string]models.FileState), nil
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

func (db *DB) GetVisibleGeneration(ctx context.Context, rootID string) (string, error) {
	var generationID string
	err := db.pool.QueryRow(ctx,
		`SELECT visible_generation_id FROM roots WHERE id = $1`, rootID,
	).Scan(&generationID)
	return generationID, err
}

func (db *DB) GetGenerationSeq(ctx context.Context, generationID string) (int64, error) {
	if generationID == "" {
		return 0, nil
	}
	var seq int64
	err := db.pool.QueryRow(ctx,
		`SELECT seq FROM sync_generations WHERE id = $1`, generationID,
	).Scan(&seq)
	return seq, err
}

func (db *DB) GetVisibleGenerationSeq(ctx context.Context, rootID string) (int64, error) {
	visibleID, err := db.GetVisibleGeneration(ctx, rootID)
	if err != nil || visibleID == "" {
		return 0, err
	}
	return db.GetGenerationSeq(ctx, visibleID)
}

func (db *DB) CreateSyncGeneration(ctx context.Context, orgID, rootID, syncJobID, manifestRef string) (*SyncGeneration, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var baseGenerationID string
	err = tx.QueryRow(ctx,
		`SELECT visible_generation_id FROM roots WHERE id = $1 FOR UPDATE`, rootID,
	).Scan(&baseGenerationID)
	if err != nil {
		return nil, err
	}

	staleCutoff := time.Now().Add(-syncJobTimeout())
	if _, err := tx.Exec(ctx,
		`UPDATE sync_generations
		 SET status = 'failed'
		 WHERE root_id = $1 AND status = 'building' AND created_at < $2`,
		rootID, staleCutoff,
	); err != nil {
		return nil, err
	}

	var buildingID string
	err = tx.QueryRow(ctx,
		`SELECT id FROM sync_generations
		 WHERE root_id = $1 AND status = 'building'
		 LIMIT 1`,
		rootID,
	).Scan(&buildingID)
	if err == nil {
		return nil, errSyncInProgress
	}
	if err != pgx.ErrNoRows {
		return nil, err
	}

	var baseSeq int64
	if baseGenerationID != "" {
		err = tx.QueryRow(ctx,
			`SELECT seq FROM sync_generations WHERE id = $1`, baseGenerationID,
		).Scan(&baseSeq)
		if err != nil {
			return nil, err
		}
	}

	generationID := uuid.New().String()
	var seq int64
	err = tx.QueryRow(ctx,
		`INSERT INTO sync_generations (id, org_id, root_id, sync_job_id, base_generation_id, base_generation_seq, status, manifest_ref)
		 VALUES ($1, $2, $3, $4, $5, $6, 'building', $7)
		 RETURNING seq`,
		generationID, orgID, rootID, syncJobID, baseGenerationID, baseSeq, manifestRef,
	).Scan(&seq)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &SyncGeneration{
		ID:                generationID,
		RootID:            rootID,
		BaseGenerationID:  baseGenerationID,
		Seq:               seq,
		BaseGenerationSeq: baseSeq,
	}, nil
}

func (db *DB) CommitSyncGeneration(ctx context.Context, generation *SyncGeneration, state map[string]models.FileState) error {
	if generation == nil {
		return fmt.Errorf("sync generation is required")
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE roots
		 SET visible_generation_id = $1, updated_at = NOW()
		 WHERE id = $2 AND visible_generation_id = $3`,
		generation.ID, generation.RootID, generation.BaseGenerationID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("root visible generation changed while sync was running")
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO root_states (root_id, state, updated_at)
		 VALUES ($1, $2::jsonb, NOW())
		 ON CONFLICT (root_id) DO UPDATE SET state = $2::jsonb, updated_at = NOW()`,
		generation.RootID, string(stateJSON),
	); err != nil {
		return err
	}

	tag, err = tx.Exec(ctx,
		`UPDATE sync_generations
		 SET status = 'visible', visible_at = NOW()
		 WHERE id = $1 AND status = 'building'`,
		generation.ID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("sync generation %s is no longer building", generation.ID)
	}

	return tx.Commit(ctx)
}

func (db *DB) MarkSyncGenerationFailed(ctx context.Context, generationID string) error {
	if generationID == "" {
		return nil
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_generations SET status = 'failed' WHERE id = $1 AND status = 'building'`,
		generationID,
	)
	return err
}

func (db *DB) MarkSyncGenerationFailedForJob(ctx context.Context, jobID string) error {
	if jobID == "" {
		return nil
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_generations SET status = 'failed' WHERE sync_job_id = $1 AND status = 'building'`,
		jobID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Embedding Cache
// ---------------------------------------------------------------------------

// GetCachedEmbeddings looks up cached embeddings by content hash within an org.
func (db *DB) GetCachedEmbeddings(ctx context.Context, orgID string, hashes []string) (map[string][]float64, error) {
	result := make(map[string][]float64)
	if len(hashes) == 0 {
		return result, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT content_hash, embedding FROM embedding_cache WHERE org_id = $1 AND content_hash = ANY($2)`,
		orgID, hashes,
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

// SaveCachedEmbeddings stores embeddings in the cache via a batched multi-value INSERT.
func (db *DB) SaveCachedEmbeddings(ctx context.Context, orgID string, entries map[string][]float64) error {
	if len(entries) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO embedding_cache (org_id, content_hash, embedding, created_at) VALUES `)
	args := make([]any, 0, len(entries)*3)
	args = append(args, orgID) // $1 = org_id
	i := 0
	for hash, emb := range entries {
		if i > 0 {
			sb.WriteString(", ")
		}
		p := i*2 + 2 // starts at $2 since $1 is org_id
		fmt.Fprintf(&sb, "($1, $%d, $%d, NOW())", p, p+1)
		args = append(args, hash, encodeEmbedding(emb))
		i++
	}
	sb.WriteString(` ON CONFLICT (org_id, content_hash) DO NOTHING`)

	_, err := db.pool.Exec(ctx, sb.String(), args...)
	return err
}

// ---------------------------------------------------------------------------
// ACLs
// ---------------------------------------------------------------------------

// CreateACL creates a folder-level ACL entry.
func (db *DB) CreateACL(ctx context.Context, orgID, rootID, pathPrefix, grantTo, permission string) (*models.RootACL, error) {
	acl := &models.RootACL{
		ID:         uuid.New().String(),
		OrgID:      orgID,
		RootID:     rootID,
		PathPrefix: pathPrefix,
		GrantTo:    grantTo,
		Permission: permission,
		CreatedAt:  time.Now(),
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO root_acls (id, org_id, root_id, path_prefix, grant_to, permission, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		acl.ID, acl.OrgID, acl.RootID, acl.PathPrefix, acl.GrantTo, acl.Permission, acl.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return acl, nil
}

// GetACLsForUser returns all ACL entries that apply to a user for a root.
// Matches both direct user grants and role-based grants.
func (db *DB) GetACLsForUser(ctx context.Context, orgID, rootID, userID string, role auth.Role) ([]models.RootACL, error) {
	grantTargets := []string{
		userID,
		"role:" + string(role),
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, root_id, path_prefix, grant_to, permission, created_at
		 FROM root_acls
		 WHERE org_id = $1 AND root_id = $2 AND grant_to = ANY($3)
		 ORDER BY length(path_prefix) DESC`,
		orgID, rootID, grantTargets,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var acls []models.RootACL
	for rows.Next() {
		var a models.RootACL
		if err := rows.Scan(&a.ID, &a.OrgID, &a.RootID, &a.PathPrefix, &a.GrantTo, &a.Permission, &a.CreatedAt); err != nil {
			return nil, err
		}
		acls = append(acls, a)
	}
	return acls, nil
}

// ListACLs returns all ACLs for a root.
func (db *DB) ListACLs(ctx context.Context, orgID, rootID string) ([]models.RootACL, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, root_id, path_prefix, grant_to, permission, created_at
		 FROM root_acls WHERE org_id = $1 AND root_id = $2 ORDER BY path_prefix`,
		orgID, rootID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var acls []models.RootACL
	for rows.Next() {
		var a models.RootACL
		if err := rows.Scan(&a.ID, &a.OrgID, &a.RootID, &a.PathPrefix, &a.GrantTo, &a.Permission, &a.CreatedAt); err != nil {
			return nil, err
		}
		acls = append(acls, a)
	}
	return acls, nil
}

// DeleteACL removes an ACL entry.
func (db *DB) DeleteACL(ctx context.Context, orgID, aclID string) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM root_acls WHERE id = $1 AND org_id = $2`, aclID, orgID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Sync Jobs
// ---------------------------------------------------------------------------

// CreateSyncJob creates a new sync job record.
func (db *DB) CreateSyncJob(ctx context.Context, orgID, rootID, userID string, totalFiles int) (*models.SyncJob, error) {
	job := &models.SyncJob{
		ID:         uuid.New().String(),
		OrgID:      orgID,
		RootID:     rootID,
		UserID:     userID,
		Status:     "pending",
		TotalFiles: totalFiles,
		Processed:  0,
		StartedAt:  time.Now(),
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO sync_jobs (id, org_id, root_id, user_id, status, total_files, processed, started_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		job.ID, job.OrgID, job.RootID, job.UserID, job.Status, job.TotalFiles, job.Processed, job.StartedAt,
	)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// UpdateSyncJobStatus updates the status and progress of a sync job.
func (db *DB) UpdateSyncJobStatus(ctx context.Context, jobID, status string, processed int) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_jobs SET status = $1, processed = $2 WHERE id = $3`,
		status, processed, jobID,
	)
	return err
}

// CompleteSyncJob marks a sync job as completed or failed.
func (db *DB) CompleteSyncJob(ctx context.Context, jobID, status string, errors []map[string]string) error {
	if errors == nil {
		errors = []map[string]string{}
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_jobs SET status = $1, finished_at = NOW(), errors = $2 WHERE id = $3`,
		status, errors, jobID,
	)
	return err
}

// GetSyncJob retrieves a sync job by ID.
func (db *DB) GetSyncJob(ctx context.Context, orgID, jobID string) (*models.SyncJob, error) {
	job := &models.SyncJob{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, root_id, user_id, status, total_files, processed, errors, started_at, finished_at
		 FROM sync_jobs WHERE id = $1 AND org_id = $2`, jobID, orgID,
	).Scan(&job.ID, &job.OrgID, &job.RootID, &job.UserID, &job.Status, &job.TotalFiles,
		&job.Processed, &job.Errors, &job.StartedAt, &job.FinishedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListSyncJobs lists recent sync jobs for a root.
func (db *DB) ListSyncJobs(ctx context.Context, orgID, rootID string, limit int) ([]models.SyncJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, root_id, user_id, status, total_files, processed, errors, started_at, finished_at
		 FROM sync_jobs WHERE org_id = $1 AND root_id = $2
		 ORDER BY started_at DESC LIMIT $3`,
		orgID, rootID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.SyncJob
	for rows.Next() {
		var j models.SyncJob
		if err := rows.Scan(&j.ID, &j.OrgID, &j.RootID, &j.UserID, &j.Status, &j.TotalFiles,
			&j.Processed, &j.Errors, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// GetLatestSyncJob gets the most recent sync job for a root.
func (db *DB) GetLatestSyncJob(ctx context.Context, orgID, rootID string) (*models.SyncJob, error) {
	job := &models.SyncJob{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, root_id, user_id, status, total_files, processed, errors, started_at, finished_at
		 FROM sync_jobs WHERE org_id = $1 AND root_id = $2
		 ORDER BY started_at DESC LIMIT 1`,
		orgID, rootID,
	).Scan(&job.ID, &job.OrgID, &job.RootID, &job.UserID, &job.Status, &job.TotalFiles,
		&job.Processed, &job.Errors, &job.StartedAt, &job.FinishedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// Ping checks the database connection.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// ---------------------------------------------------------------------------
// SimHash / Index Reuse
// ---------------------------------------------------------------------------

// UpdateRootSimHash stores the SimHash for a root.
func (db *DB) UpdateRootSimHash(ctx context.Context, orgID, rootID, simhash string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE roots SET simhash = $1 WHERE id = $2 AND org_id = $3`,
		simhash, rootID, orgID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Content Proofs
// ---------------------------------------------------------------------------

// UpsertContentProof stores or updates a content proof for a user+root pair.
func (db *DB) UpsertContentProof(ctx context.Context, orgID, userID, rootID, rootHash string, proof []byte) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO content_proofs (org_id, user_id, root_id, root_hash, proof, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (org_id, user_id, root_id)
		 DO UPDATE SET root_hash = EXCLUDED.root_hash, proof = EXCLUDED.proof, updated_at = NOW()`,
		orgID, userID, rootID, rootHash, proof,
	)
	return err
}

// GetContentProof retrieves the content proof for a user+root pair.
func (db *DB) GetContentProof(ctx context.Context, orgID, userID, rootID string) ([]byte, string, error) {
	var proof []byte
	var rootHash string
	err := db.pool.QueryRow(ctx,
		`SELECT proof, root_hash FROM content_proofs
		 WHERE org_id = $1 AND user_id = $2 AND root_id = $3`,
		orgID, userID, rootID,
	).Scan(&proof, &rootHash)
	if err != nil {
		return nil, "", err
	}
	return proof, rootHash, nil
}

// ---------------------------------------------------------------------------
// Encoding helpers
// ---------------------------------------------------------------------------

func encodeEmbedding(emb []float64) []byte {
	buf := make([]byte, len(emb)*8)
	for i, v := range emb {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

func decodeEmbedding(buf []byte) ([]float64, error) {
	if len(buf)%8 != 0 {
		return nil, fmt.Errorf("invalid embedding bytes length: %d", len(buf))
	}
	emb := make([]float64, len(buf)/8)
	for i := range emb {
		emb[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
	}
	return emb, nil
}
