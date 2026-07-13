package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
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
	OrgID             string
	RootID            string
	SyncJobID         string
	BaseGenerationID  string
	Seq               int64
	BaseGenerationSeq int64
}

type RootStateRecord struct {
	State map[string]models.FileState
	Ref   string
}

type EmailLoginChallenge struct {
	ID             string
	Email          string
	CodeHash       string
	Flow           string
	CLIRedirectURI string
	Attempts       int
	MaxAttempts    int
	RequestIPHash  string
	UserAgentHash  string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
}

var errSyncInProgress = errors.New("sync already in progress for root")
var errStaleSyncBase = errors.New("sync base generation is stale")

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
			external_id TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE organizations ADD COLUMN IF NOT EXISTS external_id TEXT;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_organizations_external_id ON organizations(external_id) WHERE external_id IS NOT NULL;

		CREATE TABLE IF NOT EXISTS users (
			id          TEXT PRIMARY KEY,
			email       TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL DEFAULT '',
			avatar_url  TEXT NOT NULL DEFAULT '',
			provider    TEXT NOT NULL DEFAULT 'google',
			provider_id TEXT NOT NULL DEFAULT '',
			external_id TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE users ADD COLUMN IF NOT EXISTS external_id TEXT;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_users_external_id ON users(external_id) WHERE external_id IS NOT NULL;

		CREATE TABLE IF NOT EXISTS user_identities (
			id             TEXT PRIMARY KEY,
			user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider       TEXT NOT NULL,
			provider_id    TEXT NOT NULL DEFAULT '',
			email          TEXT NOT NULL,
			email_verified BOOLEAN NOT NULL DEFAULT TRUE,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_user_identities_provider_id
			ON user_identities(provider, provider_id)
			WHERE provider_id <> '';
		CREATE UNIQUE INDEX IF NOT EXISTS idx_user_identities_provider_email
			ON user_identities(provider, email)
			WHERE provider_id = '';
		CREATE INDEX IF NOT EXISTS idx_user_identities_user ON user_identities(user_id);

		CREATE TABLE IF NOT EXISTS email_login_challenges (
			id               TEXT PRIMARY KEY,
			email            TEXT NOT NULL,
			code_hash        TEXT NOT NULL,
			flow             TEXT NOT NULL DEFAULT 'web',
			cli_redirect_uri TEXT NOT NULL DEFAULT '',
			attempts         INT NOT NULL DEFAULT 0,
			max_attempts     INT NOT NULL DEFAULT 5,
			request_ip_hash  TEXT NOT NULL DEFAULT '',
			user_agent_hash  TEXT NOT NULL DEFAULT '',
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			expires_at       TIMESTAMPTZ NOT NULL,
			consumed_at      TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS idx_email_login_challenges_email_created
			ON email_login_challenges(email, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_email_login_challenges_expires
			ON email_login_challenges(expires_at);

		CREATE TABLE IF NOT EXISTS org_members (
			org_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role      TEXT NOT NULL DEFAULT 'viewer',
			joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, user_id)
		);

		CREATE TABLE IF NOT EXISTS groups (
			id          TEXT PRIMARY KEY,
			org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			name        TEXT NOT NULL,
			external_id TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(org_id, name)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_org_external
			ON groups(org_id, external_id)
			WHERE external_id <> '';

		CREATE TABLE IF NOT EXISTS group_members (
			org_id    TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			group_id  TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, group_id, user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_group_members_user
			ON group_members(org_id, user_id);

		CREATE TABLE IF NOT EXISTS org_invites (
			id                 TEXT PRIMARY KEY,
			org_id             TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			email              TEXT NOT NULL,
			role               TEXT NOT NULL DEFAULT 'viewer',
			invited_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_org_invites_org_email ON org_invites(org_id, email);
		CREATE INDEX IF NOT EXISTS idx_org_invites_email ON org_invites(email);

		CREATE TABLE IF NOT EXISTS org_ignore_policies (
			org_id             TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
			patterns           TEXT NOT NULL DEFAULT '',
			updated_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS user_ignore_policies (
			org_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			patterns   TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
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
			vector_disabled BOOLEAN NOT NULL DEFAULT FALSE,
			scope       TEXT NOT NULL DEFAULT 'org',
			owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
			simhash     TEXT NOT NULL DEFAULT '',
			visible_generation_id TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE roots ADD COLUMN IF NOT EXISTS vector_disabled BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE roots ADD COLUMN IF NOT EXISTS visible_generation_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE roots ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'org';
			ALTER TABLE roots ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
			CREATE INDEX IF NOT EXISTS idx_roots_owner ON roots(owner_user_id) WHERE owner_user_id IS NOT NULL;

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
			CREATE INDEX IF NOT EXISTS idx_root_index_namespaces_root ON root_index_namespaces(root_id, shard_index);
			CREATE INDEX IF NOT EXISTS idx_root_index_namespaces_org ON root_index_namespaces(org_id);
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

			CREATE TABLE IF NOT EXISTS root_states (
			root_id    TEXT PRIMARY KEY REFERENCES roots(id) ON DELETE CASCADE,
			state      JSONB NOT NULL DEFAULT '{}',
			state_ref  TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE root_states ADD COLUMN IF NOT EXISTS state_ref TEXT NOT NULL DEFAULT '';

		CREATE TABLE IF NOT EXISTS embedding_cache (
			org_id        TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			model_version TEXT NOT NULL DEFAULT '',
			content_hash  TEXT NOT NULL,
			embedding     BYTEA NOT NULL,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (org_id, model_version, content_hash)
		);
		ALTER TABLE embedding_cache ADD COLUMN IF NOT EXISTS model_version TEXT NOT NULL DEFAULT '';
		DO $$
		BEGIN
			IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'embedding_cache_pkey')
				AND NOT EXISTS (
					SELECT 1 FROM pg_constraint c
					JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
					WHERE c.conname = 'embedding_cache_pkey' AND a.attname = 'model_version'
				) THEN
				ALTER TABLE embedding_cache DROP CONSTRAINT embedding_cache_pkey;
				ALTER TABLE embedding_cache ADD PRIMARY KEY (org_id, model_version, content_hash);
			END IF;
		END $$;

		CREATE TABLE IF NOT EXISTS root_acls (
			id          TEXT PRIMARY KEY,
			org_id      TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id     TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			path_prefix TEXT NOT NULL DEFAULT '/',
			grant_to    TEXT NOT NULL,
			permission  TEXT NOT NULL DEFAULT 'read',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS root_grants (
			id             TEXT PRIMARY KEY,
			org_id         TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id        TEXT NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
			principal_type TEXT NOT NULL,
			principal_id   TEXT NOT NULL,
			permissions    TEXT[] NOT NULL DEFAULT '{}',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(root_id, principal_type, principal_id)
		);
		CREATE INDEX IF NOT EXISTS idx_root_grants_org_principal
			ON root_grants(org_id, principal_type, principal_id);
		CREATE INDEX IF NOT EXISTS idx_root_grants_root
			ON root_grants(root_id);

		CREATE TABLE IF NOT EXISTS sync_jobs (
			id           TEXT PRIMARY KEY,
			org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			root_id      TEXT REFERENCES roots(id) ON DELETE SET NULL,
			user_id      TEXT NOT NULL REFERENCES users(id),
			status       TEXT NOT NULL DEFAULT 'pending',
			total_files  INT NOT NULL DEFAULT 0,
			processed    INT NOT NULL DEFAULT 0,
			errors       JSONB NOT NULL DEFAULT '[]',
			started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			finished_at  TIMESTAMPTZ
		);
		ALTER TABLE sync_jobs ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
		ALTER TABLE sync_jobs ALTER COLUMN root_id DROP NOT NULL;
		DO $$
		BEGIN
			IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sync_jobs_root_id_fkey') THEN
				ALTER TABLE sync_jobs DROP CONSTRAINT sync_jobs_root_id_fkey;
			END IF;
			ALTER TABLE sync_jobs
				ADD CONSTRAINT sync_jobs_root_id_fkey
				FOREIGN KEY (root_id) REFERENCES roots(id) ON DELETE SET NULL;
		END $$;
		CREATE TABLE IF NOT EXISTS sync_job_shards (
			job_id          TEXT NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
			stage           TEXT NOT NULL,
			shard_index     INT NOT NULL,
			status          TEXT NOT NULL DEFAULT 'completed',
			files_processed INT NOT NULL DEFAULT 0,
			started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			finished_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (job_id, stage, shard_index)
		);
		CREATE INDEX IF NOT EXISTS idx_sync_job_shards_job_stage_status ON sync_job_shards(job_id, stage, status);

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

		CREATE TABLE IF NOT EXISTS subscriptions (
			org_id                 TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
			stripe_customer_id     TEXT NOT NULL DEFAULT '',
			stripe_subscription_id TEXT NOT NULL DEFAULT '',
			plan                   TEXT NOT NULL DEFAULT 'free',
			status                 TEXT NOT NULL DEFAULT 'none',
			current_period_end     TIMESTAMPTZ,
			updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
		`SELECT id, name, slug, COALESCE(external_id, ''), created_at FROM organizations WHERE id = $1`, id,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.ExternalID, &org.CreatedAt)
	if err != nil {
		return nil, err
	}
	return org, nil
}

func (db *DB) ProvisionOrganization(ctx context.Context, id, name, slug, externalID string) (*models.Organization, error) {
	if id == "" {
		id = uuid.New().String()
	}

	var existingID string
	switch {
	case externalID != "":
		err := db.pool.QueryRow(ctx, `SELECT id FROM organizations WHERE external_id = $1`, externalID).Scan(&existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	case slug != "":
		err := db.pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	if existingID != "" {
		_, err := db.pool.Exec(ctx,
			`UPDATE organizations
			 SET name = $1,
			     slug = $2,
			     external_id = COALESCE(NULLIF($3, ''), external_id)
			 WHERE id = $4`,
			name, slug, externalID, existingID,
		)
		if err != nil {
			return nil, err
		}
		return db.GetOrganization(ctx, existingID)
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, external_id, created_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NOW())`,
		id, name, slug, externalID,
	)
	if err != nil {
		return nil, err
	}
	return db.GetOrganization(ctx, id)
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// UpsertUser is retained for older OAuth call sites. New providers should call
// CompleteLogin so identity, invite, and org resolution remain provider-neutral.
func (db *DB) UpsertUser(ctx context.Context, info auth.UserInfo, provider string) (userID, orgID string, role auth.Role, err error) {
	login, err := db.CompleteLogin(ctx, auth.VerifiedIdentity{
		Provider:      provider,
		ProviderID:    info.ID,
		Email:         info.Email,
		Name:          info.Name,
		AvatarURL:     info.Picture,
		EmailVerified: true,
	})
	if err != nil {
		return "", "", "", err
	}
	return login.UserID, login.OrgID, login.Role, nil
}

// CompleteLogin resolves a verified provider identity into a PufferFS user,
// accepts pending email invites, and returns the effective org membership.
func (db *DB) CompleteLogin(ctx context.Context, identity auth.VerifiedIdentity) (auth.LoginResult, error) {
	identity.Email = normalizeEmail(identity.Email)
	identity.Provider = strings.TrimSpace(identity.Provider)
	identity.ProviderID = strings.TrimSpace(identity.ProviderID)
	identity.Name = strings.TrimSpace(identity.Name)
	identity.AvatarURL = strings.TrimSpace(identity.AvatarURL)
	if identity.Email == "" {
		return auth.LoginResult{}, fmt.Errorf("email is required")
	}
	if identity.Provider == "" {
		return auth.LoginResult{}, fmt.Errorf("provider is required")
	}

	userID, err := db.findOrCreateLoginUser(ctx, identity)
	if err != nil {
		return auth.LoginResult{}, err
	}
	if err := db.upsertUserIdentity(ctx, userID, identity); err != nil {
		return auth.LoginResult{}, err
	}

	invitedOrgID, invitedRole, accepted, err := db.acceptPendingOrgInvite(ctx, userID, identity.Email)
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("accepting org invite: %w", err)
	}
	if accepted {
		return auth.LoginResult{UserID: userID, OrgID: invitedOrgID, Role: invitedRole, Email: identity.Email}, nil
	}

	var orgID string
	var rawRole string
	err = db.pool.QueryRow(ctx,
		`SELECT org_id, role FROM org_members WHERE user_id = $1 ORDER BY joined_at LIMIT 1`,
		userID,
	).Scan(&orgID, &rawRole)
	if err == nil {
		return auth.LoginResult{UserID: userID, OrgID: orgID, Role: auth.Role(rawRole), Email: identity.Email}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return auth.LoginResult{}, fmt.Errorf("looking up org membership: %w", err)
	}

	org, err := db.CreateOrganization(ctx, workspaceName(identity), personalOrgSlug(identity.Email))
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("creating personal org: %w", err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		org.ID, userID,
	); err != nil {
		return auth.LoginResult{}, fmt.Errorf("adding org membership: %w", err)
	}
	return auth.LoginResult{UserID: userID, OrgID: org.ID, Role: auth.RoleOwner, Email: identity.Email}, nil
}

func (db *DB) findOrCreateLoginUser(ctx context.Context, identity auth.VerifiedIdentity) (string, error) {
	var existingID string
	var existingProvider string
	err := db.pool.QueryRow(ctx,
		`SELECT id, provider FROM users WHERE email = $1`,
		identity.Email,
	).Scan(&existingID, &existingProvider)
	if err == nil {
		nameExpr := `name`
		avatarExpr := `avatar_url`
		args := []any{existingID}
		if identity.Name != "" {
			nameExpr = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, identity.Name)
		}
		if identity.AvatarURL != "" {
			avatarExpr = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, identity.AvatarURL)
		}
		if existingProvider == identity.Provider && identity.ProviderID != "" {
			args = append(args, identity.ProviderID)
			if _, err := db.pool.Exec(ctx,
				fmt.Sprintf(`UPDATE users SET name = %s, avatar_url = %s, provider_id = $%d WHERE id = $1`, nameExpr, avatarExpr, len(args)),
				args...,
			); err != nil {
				return "", fmt.Errorf("updating user: %w", err)
			}
		} else if _, err := db.pool.Exec(ctx,
			fmt.Sprintf(`UPDATE users SET name = %s, avatar_url = %s WHERE id = $1`, nameExpr, avatarExpr),
			args...,
		); err != nil {
			return "", fmt.Errorf("updating user: %w", err)
		}
		return existingID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("looking up user: %w", err)
	}

	userID := uuid.New().String()
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, avatar_url, provider, provider_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, identity.Email, identity.Name, identity.AvatarURL, identity.Provider, identity.ProviderID,
	); err != nil {
		return "", fmt.Errorf("creating user: %w", err)
	}
	return userID, nil
}

func (db *DB) upsertUserIdentity(ctx context.Context, userID string, identity auth.VerifiedIdentity) error {
	var identityID string
	var err error
	if identity.ProviderID != "" {
		err = db.pool.QueryRow(ctx,
			`SELECT id FROM user_identities WHERE provider = $1 AND provider_id = $2`,
			identity.Provider, identity.ProviderID,
		).Scan(&identityID)
	} else {
		err = db.pool.QueryRow(ctx,
			`SELECT id FROM user_identities WHERE provider = $1 AND provider_id = '' AND email = $2`,
			identity.Provider, identity.Email,
		).Scan(&identityID)
	}
	if err == nil {
		_, err = db.pool.Exec(ctx,
			`UPDATE user_identities
			 SET user_id = $1, email = $2, email_verified = $3, last_seen_at = NOW()
			 WHERE id = $4`,
			userID, identity.Email, identity.EmailVerified, identityID,
		)
		if err != nil {
			return fmt.Errorf("updating identity: %w", err)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("looking up identity: %w", err)
	}

	if _, err := db.pool.Exec(ctx,
		`INSERT INTO user_identities (id, user_id, provider, provider_id, email, email_verified, created_at, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())`,
		"uid_"+uuid.New().String(), userID, identity.Provider, identity.ProviderID, identity.Email, identity.EmailVerified,
	); err != nil {
		return fmt.Errorf("creating identity: %w", err)
	}
	return nil
}

func workspaceName(identity auth.VerifiedIdentity) string {
	if identity.Name != "" {
		return identity.Name + "'s Workspace"
	}
	local := strings.Split(identity.Email, "@")[0]
	if local == "" {
		local = "PufferFS"
	}
	return local + "'s Workspace"
}

func personalOrgSlug(email string) string {
	local := strings.Split(normalizeEmail(email), "@")[0]
	local = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, local)
	local = strings.Trim(local, "-")
	if local == "" {
		local = "workspace"
	}
	return local + "-" + uuid.New().String()[:8]
}

func (db *DB) acceptPendingOrgInvite(ctx context.Context, userID, email string) (orgID string, role auth.Role, accepted bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback(ctx)

	var inviteID string
	var rawRole string
	err = tx.QueryRow(ctx,
		`SELECT id, org_id, role
		 FROM org_invites
		 WHERE email = $1
		 ORDER BY created_at
		 LIMIT 1`,
		normalizeEmail(email),
	).Scan(&inviteID, &orgID, &rawRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		orgID, userID, rawRole,
	); err != nil {
		return "", "", false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM org_invites WHERE id = $1`, inviteID); err != nil {
		return "", "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", false, err
	}
	return orgID, auth.Role(rawRole), true, nil
}

func (db *DB) CreateEmailLoginChallenge(ctx context.Context, challenge EmailLoginChallenge) error {
	if challenge.ID == "" {
		challenge.ID = "elc_" + uuid.New().String()
	}
	challenge.Email = normalizeEmail(challenge.Email)
	if challenge.Flow == "" {
		challenge.Flow = "web"
	}
	if challenge.MaxAttempts <= 0 {
		challenge.MaxAttempts = 5
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO email_login_challenges
		 (id, email, code_hash, flow, cli_redirect_uri, max_attempts, request_ip_hash, user_agent_hash, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9)`,
		challenge.ID,
		challenge.Email,
		challenge.CodeHash,
		challenge.Flow,
		challenge.CLIRedirectURI,
		challenge.MaxAttempts,
		challenge.RequestIPHash,
		challenge.UserAgentHash,
		challenge.ExpiresAt,
	)
	return err
}

func (db *DB) GetEmailLoginChallenge(ctx context.Context, id string) (*EmailLoginChallenge, error) {
	var challenge EmailLoginChallenge
	err := db.pool.QueryRow(ctx,
		`SELECT id, email, code_hash, flow, cli_redirect_uri, attempts, max_attempts,
		        request_ip_hash, user_agent_hash, created_at, expires_at, consumed_at
		   FROM email_login_challenges
		  WHERE id = $1`,
		id,
	).Scan(
		&challenge.ID,
		&challenge.Email,
		&challenge.CodeHash,
		&challenge.Flow,
		&challenge.CLIRedirectURI,
		&challenge.Attempts,
		&challenge.MaxAttempts,
		&challenge.RequestIPHash,
		&challenge.UserAgentHash,
		&challenge.CreatedAt,
		&challenge.ExpiresAt,
		&challenge.ConsumedAt,
	)
	if err != nil {
		return nil, err
	}
	return &challenge, nil
}

func (db *DB) IncrementEmailLoginChallengeAttempts(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE email_login_challenges SET attempts = attempts + 1 WHERE id = $1`,
		id,
	)
	return err
}

func (db *DB) ConsumeEmailLoginChallenge(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE email_login_challenges
		    SET consumed_at = NOW()
		  WHERE id = $1 AND consumed_at IS NULL`,
		id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (db *DB) CountRecentEmailLoginChallenges(ctx context.Context, email, ipHash string, since time.Time) (int, error) {
	email = normalizeEmail(email)
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		   FROM email_login_challenges
		  WHERE created_at >= $1
		    AND (email = $2 OR ($3 <> '' AND request_ip_hash = $3))`,
		since, email, ipHash,
	).Scan(&count)
	return count, err
}

func (db *DB) DeleteExpiredEmailLoginChallenges(ctx context.Context) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM email_login_challenges
		  WHERE expires_at < NOW() - INTERVAL '1 day'
		     OR consumed_at < NOW() - INTERVAL '1 day'`,
	)
	return err
}

// GetUser retrieves a user by ID.
func (db *DB) GetUser(ctx context.Context, id string) (*models.User, error) {
	u := &models.User{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, email, name, avatar_url, provider, COALESCE(external_id, ''), created_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Provider, &u.ExternalID, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (db *DB) ProvisionUser(ctx context.Context, id, email, name, avatarURL, provider, providerID, externalID string) (*models.User, error) {
	if id == "" {
		id = uuid.New().String()
	}
	if provider == "" {
		provider = "admin"
	}

	var existingID string
	switch {
	case externalID != "":
		err := db.pool.QueryRow(ctx, `SELECT id FROM users WHERE external_id = $1`, externalID).Scan(&existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	case email != "":
		err := db.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&existingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	if existingID != "" {
		_, err := db.pool.Exec(ctx,
			`UPDATE users
			 SET email = $1,
			     name = $2,
			     avatar_url = $3,
			     provider = $4,
			     provider_id = $5,
			     external_id = COALESCE(NULLIF($6, ''), external_id)
			 WHERE id = $7`,
			email, name, avatarURL, provider, providerID, externalID, existingID,
		)
		if err != nil {
			return nil, err
		}
		return db.GetUser(ctx, existingID)
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, avatar_url, provider, provider_id, external_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NOW())`,
		id, email, name, avatarURL, provider, providerID, externalID,
	)
	if err != nil {
		return nil, err
	}
	return db.GetUser(ctx, id)
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
	var scopes []string
	err := db.pool.QueryRow(ctx,
		`SELECT ak.org_id, ak.user_id, om.role, ak.scopes
		 FROM api_keys ak
		 JOIN org_members om ON om.org_id = ak.org_id AND om.user_id = ak.user_id
		 WHERE ak.key_hash = $1
		   AND (ak.expires_at IS NULL OR ak.expires_at > NOW())`,
		keyHash,
	).Scan(&orgID, &userID, &role, &scopes)
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
		Scopes: scopes,
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
// Ignore Policies
// ---------------------------------------------------------------------------

func (db *DB) GetEffectiveIgnorePolicy(ctx context.Context, orgID, userID string) (*models.EffectiveIgnorePolicy, error) {
	orgPolicy, err := db.GetOrgIgnorePolicy(ctx, orgID)
	if err != nil {
		return nil, err
	}
	userPolicy, err := db.GetUserIgnorePolicy(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	return &models.EffectiveIgnorePolicy{
		OrgPatterns:  orgPolicy.Patterns,
		UserPatterns: userPolicy.Patterns,
	}, nil
}

func (db *DB) GetOrgIgnorePolicy(ctx context.Context, orgID string) (*models.IgnorePolicy, error) {
	policy := &models.IgnorePolicy{OrgID: orgID}
	err := db.pool.QueryRow(ctx,
		`SELECT org_id, patterns, COALESCE(updated_by_user_id, ''), updated_at
		 FROM org_ignore_policies WHERE org_id = $1`,
		orgID,
	).Scan(&policy.OrgID, &policy.Patterns, &policy.UpdatedByUserID, &policy.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return policy, nil
		}
		return nil, err
	}
	return policy, nil
}

func (db *DB) SetOrgIgnorePolicy(ctx context.Context, orgID, updatedByUserID, patterns string) (*models.IgnorePolicy, error) {
	policy := &models.IgnorePolicy{}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO org_ignore_policies (org_id, patterns, updated_by_user_id, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (org_id) DO UPDATE SET
			patterns = EXCLUDED.patterns,
			updated_by_user_id = EXCLUDED.updated_by_user_id,
			updated_at = NOW()
		 RETURNING org_id, patterns, COALESCE(updated_by_user_id, ''), updated_at`,
		orgID, patterns, updatedByUserID,
	).Scan(&policy.OrgID, &policy.Patterns, &policy.UpdatedByUserID, &policy.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return policy, nil
}

func (db *DB) GetUserIgnorePolicy(ctx context.Context, orgID, userID string) (*models.IgnorePolicy, error) {
	policy := &models.IgnorePolicy{OrgID: orgID, UserID: userID}
	err := db.pool.QueryRow(ctx,
		`SELECT org_id, user_id, patterns, updated_at
		 FROM user_ignore_policies WHERE org_id = $1 AND user_id = $2`,
		orgID, userID,
	).Scan(&policy.OrgID, &policy.UserID, &policy.Patterns, &policy.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return policy, nil
		}
		return nil, err
	}
	return policy, nil
}

func (db *DB) SetUserIgnorePolicy(ctx context.Context, orgID, userID, patterns string) (*models.IgnorePolicy, error) {
	policy := &models.IgnorePolicy{}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO user_ignore_policies (org_id, user_id, patterns, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (org_id, user_id) DO UPDATE SET
			patterns = EXCLUDED.patterns,
			updated_at = NOW()
		 RETURNING org_id, user_id, patterns, updated_at`,
		orgID, userID, patterns,
	).Scan(&policy.OrgID, &policy.UserID, &policy.Patterns, &policy.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return policy, nil
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

func (db *DB) InviteOrgMember(ctx context.Context, orgID, email string, role auth.Role, invitedByUserID string) (*models.OrgInvite, error) {
	invite := &models.OrgInvite{
		ID:              uuid.New().String(),
		Email:           normalizeEmail(email),
		Role:            string(role),
		InvitedByUserID: invitedByUserID,
		CreatedAt:       time.Now(),
	}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO org_invites (id, org_id, email, role, invited_by_user_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (org_id, email)
		 DO UPDATE SET role = EXCLUDED.role,
		               invited_by_user_id = EXCLUDED.invited_by_user_id,
		               created_at = EXCLUDED.created_at
		 RETURNING id, email, role, invited_by_user_id, created_at`,
		invite.ID, orgID, invite.Email, invite.Role, invitedByUserID, invite.CreatedAt,
	).Scan(&invite.ID, &invite.Email, &invite.Role, &invite.InvitedByUserID, &invite.CreatedAt)
	if err != nil {
		return nil, err
	}
	return invite, nil
}

func (db *DB) ListOrgInvites(ctx context.Context, orgID string) ([]models.OrgInvite, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, email, role, invited_by_user_id, created_at
		 FROM org_invites
		 WHERE org_id = $1
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invites []models.OrgInvite
	for rows.Next() {
		var invite models.OrgInvite
		if err := rows.Scan(&invite.ID, &invite.Email, &invite.Role, &invite.InvitedByUserID, &invite.CreatedAt); err != nil {
			return nil, err
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

func (db *DB) GetOrgInvite(ctx context.Context, orgID, inviteID string) (*models.OrgInvite, error) {
	var invite models.OrgInvite
	err := db.pool.QueryRow(ctx,
		`SELECT id, email, role, invited_by_user_id, created_at
		 FROM org_invites
		 WHERE org_id = $1 AND id = $2`,
		orgID, inviteID,
	).Scan(&invite.ID, &invite.Email, &invite.Role, &invite.InvitedByUserID, &invite.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &invite, nil
}

func (db *DB) DeleteOrgInvite(ctx context.Context, orgID, inviteID string) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM org_invites WHERE org_id = $1 AND id = $2`, orgID, inviteID)
	return err
}

func (db *DB) UpdateOrgMemberRole(ctx context.Context, orgID, userID string, role auth.Role) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2`,
		orgID, userID, string(role),
	)
	return err
}

func (db *DB) GetOrgMember(ctx context.Context, orgID, userID string) (*models.OrgMember, error) {
	var m models.OrgMember
	err := db.pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.name, u.avatar_url, om.role, om.joined_at
		 FROM org_members om JOIN users u ON u.id = om.user_id
		 WHERE om.org_id = $1 AND om.user_id = $2`,
		orgID, userID,
	).Scan(&m.UserID, &m.Email, &m.Name, &m.AvatarURL, &m.Role, &m.JoinedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
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

func (db *DB) CountOrgMembersByRole(ctx context.Context, orgID string, role auth.Role) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM org_members WHERE org_id = $1 AND role = $2`,
		orgID, string(role),
	).Scan(&count)
	return count, err
}

// RemoveOrgMember removes a user from an org.
func (db *DB) RemoveOrgMember(ctx context.Context, orgID, userID string) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID,
	)
	return err
}

// CreateGroup creates or updates an organization group used for root grants.
func (db *DB) CreateGroup(ctx context.Context, orgID, id, name, externalID string) (*models.Group, error) {
	name = strings.TrimSpace(name)
	externalID = strings.TrimSpace(externalID)
	if id == "" {
		id = uuid.New().String()
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if externalID != "" {
		var existingID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM groups WHERE org_id = $1 AND external_id = $2`,
			orgID, externalID,
		).Scan(&existingID)
		if err == nil {
			id = existingID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	group := &models.Group{}
	err = tx.QueryRow(ctx,
		`INSERT INTO groups (id, org_id, name, external_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 ON CONFLICT (id) DO UPDATE SET
		   name = EXCLUDED.name,
		   external_id = EXCLUDED.external_id,
		   updated_at = NOW()
		 RETURNING id, org_id, name, external_id, created_at, updated_at`,
		id, orgID, name, externalID,
	).Scan(&group.ID, &group.OrgID, &group.Name, &group.ExternalID, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	tx = nil
	return group, nil
}

func (db *DB) GetGroup(ctx context.Context, orgID, groupID string) (*models.Group, error) {
	group := &models.Group{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, name, external_id, created_at, updated_at
		 FROM groups WHERE org_id = $1 AND id = $2`,
		orgID, groupID,
	).Scan(&group.ID, &group.OrgID, &group.Name, &group.ExternalID, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return group, nil
}

func (db *DB) ListGroups(ctx context.Context, orgID string) ([]models.Group, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, name, external_id, created_at, updated_at
		 FROM groups WHERE org_id = $1 ORDER BY name`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []models.Group
	for rows.Next() {
		var group models.Group
		if err := rows.Scan(&group.ID, &group.OrgID, &group.Name, &group.ExternalID, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (db *DB) AddGroupMember(ctx context.Context, orgID, groupID, userID string) (*models.GroupMember, error) {
	member := &models.GroupMember{}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO group_members (org_id, group_id, user_id, joined_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (org_id, group_id, user_id) DO UPDATE SET joined_at = group_members.joined_at
		 RETURNING group_id, user_id, joined_at`,
		orgID, groupID, userID,
	).Scan(&member.GroupID, &member.UserID, &member.JoinedAt)
	if err != nil {
		return nil, err
	}
	return member, nil
}

func (db *DB) DeleteGroupMember(ctx context.Context, orgID, groupID, userID string) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM group_members WHERE org_id = $1 AND group_id = $2 AND user_id = $3`,
		orgID, groupID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (db *DB) ListGroupMembers(ctx context.Context, orgID, groupID string) ([]models.GroupMember, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT gm.group_id, gm.user_id, u.email, u.name, gm.joined_at
		 FROM group_members gm
		 JOIN users u ON u.id = gm.user_id
		 WHERE gm.org_id = $1 AND gm.group_id = $2
		 ORDER BY u.email`,
		orgID, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []models.GroupMember
	for rows.Next() {
		var member models.GroupMember
		if err := rows.Scan(&member.GroupID, &member.UserID, &member.Email, &member.Name, &member.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// ---------------------------------------------------------------------------
// Roots
// ---------------------------------------------------------------------------

// CreateRoot creates a new root metadata entry scoped to an org.
func (db *DB) CreateRoot(ctx context.Context, orgID, name, sourcePath string) (*models.RootMetadata, error) {
	return db.CreateRootWithScope(ctx, orgID, name, sourcePath, models.RootScopeOrg, "")
}

func (db *DB) CreateRootWithScope(ctx context.Context, orgID, name, sourcePath, scope, ownerUserID string) (*models.RootMetadata, error) {
	return db.CreateRootWithScopeAndFeatures(ctx, orgID, name, sourcePath, scope, ownerUserID, true)
}

func (db *DB) CreateRootWithScopeAndFeatures(ctx context.Context, orgID, name, sourcePath, scope, ownerUserID string, vectorDisabled bool) (*models.RootMetadata, error) {
	if scope == "" {
		scope = models.RootScopeOrg
	}
	if scope != models.RootScopeUser {
		ownerUserID = ""
	}
	root := &models.RootMetadata{
		ID:                   uuid.New().String(),
		OrgID:                orgID,
		Name:                 name,
		SourcePath:           sourcePath,
		Scope:                scope,
		OwnerUserID:          ownerUserID,
		VectorDisabled:       vectorDisabled,
		VisibleGenerationID:  "",
		VisibleGenerationSeq: 0,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	_, err = tx.Exec(ctx,
		`INSERT INTO roots (id, org_id, name, source_path, scope, owner_user_id, vector_disabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9)`,
		root.ID, root.OrgID, root.Name, root.SourcePath, root.Scope, root.OwnerUserID, root.VectorDisabled, root.CreatedAt, root.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := db.insertRootIndexNamespacesTx(ctx, tx, root, rootIndexNamespaceShardCount()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	tx = nil
	return root, nil
}

const rootSelectColumns = `r.id, r.org_id, r.name, r.source_path, r.scope, COALESCE(r.owner_user_id, ''), r.vector_disabled, r.visible_generation_id, COALESCE(g.seq, 0), r.created_at, r.updated_at`

func scanRoot(row pgx.Row) (*models.RootMetadata, error) {
	root := &models.RootMetadata{}
	err := row.Scan(&root.ID, &root.OrgID, &root.Name, &root.SourcePath, &root.Scope, &root.OwnerUserID, &root.VectorDisabled, &root.VisibleGenerationID, &root.VisibleGenerationSeq, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRoot retrieves a root by ID, scoped to an org.
func (db *DB) GetRoot(ctx context.Context, orgID, id string) (*models.RootMetadata, error) {
	return scanRoot(db.pool.QueryRow(ctx,
		`SELECT `+rootSelectColumns+`
		 FROM roots r
		 LEFT JOIN sync_generations g ON g.id = r.visible_generation_id
		 WHERE r.id = $1 AND r.org_id = $2`, id, orgID,
	))
}

func (db *DB) GetRootAnyOrg(ctx context.Context, id string) (*models.RootMetadata, error) {
	return scanRoot(db.pool.QueryRow(ctx,
		`SELECT `+rootSelectColumns+`
		 FROM roots r
		 LEFT JOIN sync_generations g ON g.id = r.visible_generation_id
		 WHERE r.id = $1`, id,
	))
}

// GetRootByName retrieves a root by name, scoped to an org.
func (db *DB) GetRootByName(ctx context.Context, orgID, name string) (*models.RootMetadata, error) {
	return scanRoot(db.pool.QueryRow(ctx,
		`SELECT `+rootSelectColumns+`
		 FROM roots r
		 LEFT JOIN sync_generations g ON g.id = r.visible_generation_id
		 WHERE r.name = $1 AND r.org_id = $2`, name, orgID,
	))
}

// ListRoots returns all roots for an org.
func (db *DB) ListRoots(ctx context.Context, orgID string) ([]models.RootMetadata, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+rootSelectColumns+`
		 FROM roots r
		 LEFT JOIN sync_generations g ON g.id = r.visible_generation_id
		 WHERE r.org_id = $1 ORDER BY r.created_at DESC`, orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roots []models.RootMetadata
	for rows.Next() {
		var r models.RootMetadata
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.SourcePath, &r.Scope, &r.OwnerUserID, &r.VectorDisabled, &r.VisibleGenerationID, &r.VisibleGenerationSeq, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	return roots, nil
}

func (db *DB) ListAccessibleRoots(ctx context.Context, orgID, userID string, role auth.Role) ([]models.RootMetadata, error) {
	allRoots, err := db.ListRoots(ctx, orgID)
	if err != nil {
		return nil, err
	}
	roots := make([]models.RootMetadata, 0, len(allRoots))
	for _, root := range allRoots {
		perms, source, err := db.RootPermissions(ctx, &root, userID, role)
		if err != nil {
			return nil, err
		}
		if rootPermissionAllowed(perms, models.RootPermissionRead) {
			root.Access = perms
			root.AccessSource = source
			roots = append(roots, root)
		}
	}
	return roots, nil
}

func (db *DB) ListRootsOwnedByUser(ctx context.Context, userID string) ([]models.RootMetadata, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+rootSelectColumns+`
		 FROM roots r
		 LEFT JOIN sync_generations g ON g.id = r.visible_generation_id
		 WHERE r.owner_user_id = $1
		 ORDER BY r.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roots []models.RootMetadata
	for rows.Next() {
		var r models.RootMetadata
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.SourcePath, &r.Scope, &r.OwnerUserID, &r.VectorDisabled, &r.VisibleGenerationID, &r.VisibleGenerationSeq, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	return roots, rows.Err()
}

func (db *DB) DeleteRoot(ctx context.Context, orgID, rootID string) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM roots WHERE id = $1 AND org_id = $2`, rootID, orgID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (db *DB) CreateRootGrant(ctx context.Context, orgID, rootID, principalType, principalID string, permissions []string) (*models.RootGrant, error) {
	grant := &models.RootGrant{}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO root_grants (id, org_id, root_id, principal_type, principal_id, permissions, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		 ON CONFLICT (root_id, principal_type, principal_id) DO UPDATE SET
		   permissions = EXCLUDED.permissions,
		   updated_at = NOW()
		 RETURNING id, org_id, root_id, principal_type, principal_id, permissions, created_at, updated_at`,
		uuid.New().String(), orgID, rootID, principalType, principalID, permissions,
	).Scan(&grant.ID, &grant.OrgID, &grant.RootID, &grant.PrincipalType, &grant.PrincipalID, &grant.Permissions, &grant.CreatedAt, &grant.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return grant, nil
}

func (db *DB) ListRootGrants(ctx context.Context, orgID, rootID string) ([]models.RootGrant, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, root_id, principal_type, principal_id, permissions, created_at, updated_at
		 FROM root_grants WHERE org_id = $1 AND root_id = $2
		 ORDER BY principal_type, principal_id`,
		orgID, rootID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []models.RootGrant
	for rows.Next() {
		var grant models.RootGrant
		if err := rows.Scan(&grant.ID, &grant.OrgID, &grant.RootID, &grant.PrincipalType, &grant.PrincipalID, &grant.Permissions, &grant.CreatedAt, &grant.UpdatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (db *DB) DeleteRootGrant(ctx context.Context, orgID, rootID, grantID string) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM root_grants WHERE id = $1 AND org_id = $2 AND root_id = $3`,
		grantID, orgID, rootID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (db *DB) RootPermissions(ctx context.Context, root *models.RootMetadata, userID string, role auth.Role) ([]string, string, error) {
	if root == nil {
		return nil, "", nil
	}
	permissions := map[string]bool{}
	source := ""
	add := func(nextSource string, perms ...string) {
		for _, perm := range perms {
			addRootPermission(permissions, perm)
		}
		if source == "" && len(perms) > 0 {
			source = nextSource
		}
	}

	switch root.Scope {
	case "", models.RootScopeOrg:
		add("org", models.RootPermissionRead)
		if auth.HasMinRole(role, auth.RoleEditor) {
			add("role", models.RootPermissionSync)
		}
		if auth.HasMinRole(role, auth.RoleAdmin) {
			add("role", models.RootPermissionDelete, models.RootPermissionAdmin)
		}
	case models.RootScopeUser:
		if root.OwnerUserID == userID {
			add("owner", models.RootPermissionRead, models.RootPermissionSync, models.RootPermissionDelete, models.RootPermissionAdmin)
		} else if auth.HasMinRole(role, auth.RoleAdmin) {
			add("role", models.RootPermissionRead, models.RootPermissionSync, models.RootPermissionDelete, models.RootPermissionAdmin)
		}
	case models.RootScopeRestricted:
		if auth.HasMinRole(role, auth.RoleAdmin) {
			add("role", models.RootPermissionRead, models.RootPermissionSync, models.RootPermissionDelete, models.RootPermissionAdmin)
		}
	}

	rows, err := db.pool.Query(ctx,
		`SELECT principal_type, permissions
		 FROM root_grants rg
		 WHERE rg.org_id = $1 AND rg.root_id = $2
		   AND (
		     (rg.principal_type = 'org' AND rg.principal_id = $1)
		     OR (rg.principal_type = 'user' AND rg.principal_id = $3)
		     OR (rg.principal_type = 'group' AND EXISTS (
		       SELECT 1 FROM group_members gm
		       WHERE gm.org_id = rg.org_id
		         AND gm.group_id = rg.principal_id
		         AND gm.user_id = $3
		     ))
		   )`,
		root.OrgID, root.ID, userID,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	for rows.Next() {
		var principalType string
		var grantPerms []string
		if err := rows.Scan(&principalType, &grantPerms); err != nil {
			return nil, "", err
		}
		add(principalType, grantPerms...)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return sortedRootPermissions(permissions), source, nil
}

func addRootPermission(permissions map[string]bool, permission string) {
	switch strings.TrimSpace(permission) {
	case models.RootPermissionAdmin:
		permissions[models.RootPermissionRead] = true
		permissions[models.RootPermissionSync] = true
		permissions[models.RootPermissionDelete] = true
		permissions[models.RootPermissionAdmin] = true
	case models.RootPermissionDelete:
		permissions[models.RootPermissionRead] = true
		permissions[models.RootPermissionDelete] = true
	case models.RootPermissionSync:
		permissions[models.RootPermissionRead] = true
		permissions[models.RootPermissionSync] = true
	case models.RootPermissionRead:
		permissions[models.RootPermissionRead] = true
	}
}

func sortedRootPermissions(permissions map[string]bool) []string {
	order := []string{models.RootPermissionRead, models.RootPermissionSync, models.RootPermissionDelete, models.RootPermissionAdmin}
	out := make([]string, 0, len(order))
	for _, permission := range order {
		if permissions[permission] {
			out = append(out, permission)
		}
	}
	return out
}

func rootPermissionAllowed(permissions []string, action string) bool {
	for _, permission := range permissions {
		if permission == action || permission == models.RootPermissionAdmin {
			return true
		}
		if action == models.RootPermissionRead && (permission == models.RootPermissionSync || permission == models.RootPermissionDelete) {
			return true
		}
	}
	return false
}

func (db *DB) insertRootIndexNamespacesTx(ctx context.Context, tx pgx.Tx, root *models.RootMetadata, shardCount int) error {
	if shardCount < 1 {
		shardCount = defaultRootIndexNamespaceShards
	}
	if shardCount > maxRootIndexNamespaceShards {
		shardCount = maxRootIndexNamespaceShards
	}
	for shardIndex := 0; shardIndex < shardCount; shardIndex++ {
		_, err := tx.Exec(ctx,
			`INSERT INTO root_index_namespaces (id, org_id, root_id, namespace, shard_index, shard_count, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.NewString(), root.OrgID, root.ID, rootIndexNamespaceName(root.OrgID, root.ID, shardIndex), shardIndex, shardCount, root.CreatedAt,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

const rootIndexNamespaceSelectColumns = `id, org_id, root_id, namespace, shard_index, shard_count, created_at, retired_at`

func scanRootIndexNamespaces(rows pgx.Rows) ([]models.RootIndexNamespace, error) {
	defer rows.Close()
	var namespaces []models.RootIndexNamespace
	for rows.Next() {
		var ns models.RootIndexNamespace
		if err := rows.Scan(&ns.ID, &ns.OrgID, &ns.RootID, &ns.Namespace, &ns.ShardIndex, &ns.ShardCount, &ns.CreatedAt, &ns.RetiredAt); err != nil {
			return nil, err
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, rows.Err()
}

func (db *DB) listRootIndexNamespaces(ctx context.Context, orgID, rootID string) ([]models.RootIndexNamespace, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+rootIndexNamespaceSelectColumns+`
		 FROM root_index_namespaces
		 WHERE org_id = $1 AND root_id = $2 AND retired_at IS NULL
		 ORDER BY shard_index`,
		orgID, rootID,
	)
	if err != nil {
		return nil, err
	}
	return scanRootIndexNamespaces(rows)
}

func (db *DB) ListRootIndexNamespaces(ctx context.Context, orgID, rootID string) ([]models.RootIndexNamespace, error) {
	namespaces, err := db.listRootIndexNamespaces(ctx, orgID, rootID)
	if err != nil {
		return nil, err
	}
	if len(namespaces) > 0 {
		return namespaces, nil
	}
	if err := db.EnsureRootIndexNamespaces(ctx, orgID, rootID); err != nil {
		return nil, err
	}
	return db.listRootIndexNamespaces(ctx, orgID, rootID)
}

func (db *DB) EnsureRootIndexNamespaces(ctx context.Context, orgID, rootID string) error {
	namespaces, err := db.listRootIndexNamespaces(ctx, orgID, rootID)
	if err != nil {
		return err
	}
	if len(namespaces) > 0 {
		return nil
	}

	root, err := db.GetRoot(ctx, orgID, rootID)
	if err != nil {
		return err
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	var existing int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM root_index_namespaces WHERE org_id = $1 AND root_id = $2 AND retired_at IS NULL`,
		orgID, rootID,
	).Scan(&existing); err != nil {
		return err
	}
	if existing == 0 {
		if err := db.insertRootIndexNamespacesTx(ctx, tx, root, rootIndexNamespaceShardCount()); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	tx = nil
	return nil
}

func (db *DB) DeleteOrganization(ctx context.Context, orgID string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (db *DB) RootHasActiveSync(ctx context.Context, orgID, rootID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_generations
			 WHERE org_id = $1 AND root_id = $2 AND status = 'building'
			UNION ALL
			SELECT 1 FROM sync_jobs
			 WHERE org_id = $1 AND root_id = $2 AND status NOT IN ('completed', 'failed')
		)`,
		orgID, rootID,
	).Scan(&exists)
	return exists, err
}

func (db *DB) OrgHasActiveSync(ctx context.Context, orgID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_generations
			 WHERE org_id = $1 AND status = 'building'
			UNION ALL
			SELECT 1 FROM sync_jobs
			 WHERE org_id = $1 AND status NOT IN ('completed', 'failed')
		)`,
		orgID,
	).Scan(&exists)
	return exists, err
}

func (db *DB) UserHasActiveSync(ctx context.Context, userID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_jobs
			 WHERE user_id = $1 AND status NOT IN ('completed', 'failed')
		)`,
		userID,
	).Scan(&exists)
	return exists, err
}

func (db *DB) DeleteUser(ctx context.Context, userID string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM content_proofs WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM org_members WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sync_jobs WHERE user_id = $1`, userID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

func (db *DB) ListSyncGenerationIDs(ctx context.Context, orgID, rootID string) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id FROM sync_generations WHERE org_id = $1 AND root_id = $2`,
		orgID, rootID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SaveState persists the filesystem state for a root.
func (db *DB) SaveState(ctx context.Context, rootID string, state map[string]models.FileState) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO root_states (root_id, state, state_ref, updated_at)
		 VALUES ($1, $2, '', NOW())
		 ON CONFLICT (root_id) DO UPDATE SET state = $2, state_ref = '', updated_at = NOW()`,
		rootID, state,
	)
	return err
}

func (db *DB) SaveStateRef(ctx context.Context, rootID, stateRef string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO root_states (root_id, state, state_ref, updated_at)
		 VALUES ($1, '{}'::jsonb, $2, NOW())
		 ON CONFLICT (root_id) DO UPDATE SET state = '{}'::jsonb, state_ref = $2, updated_at = NOW()`,
		rootID, stateRef,
	)
	return err
}

// LoadState retrieves the filesystem state for a root.
func (db *DB) LoadState(ctx context.Context, rootID string) (map[string]models.FileState, error) {
	record, err := db.LoadStateRecord(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if record.State == nil {
		return make(map[string]models.FileState), nil
	}
	return record.State, nil
}

func (db *DB) LoadStateRecord(ctx context.Context, rootID string) (*RootStateRecord, error) {
	var state map[string]models.FileState
	var stateRef string
	err := db.pool.QueryRow(ctx,
		`SELECT state, COALESCE(state_ref, '') FROM root_states WHERE root_id = $1`, rootID,
	).Scan(&state, &stateRef)
	if err != nil {
		return &RootStateRecord{State: make(map[string]models.FileState)}, nil
	}
	return &RootStateRecord{State: state, Ref: stateRef}, nil
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

func (db *DB) CreateSyncGeneration(ctx context.Context, orgID, rootID, syncJobID, manifestRef, clientBaseGenerationID string, clientBaseGenerationSeq int64) (*SyncGeneration, error) {
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
	if err := validateSyncBase(clientBaseGenerationID, clientBaseGenerationSeq, baseGenerationID, baseSeq); err != nil {
		return nil, err
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
		OrgID:             orgID,
		RootID:            rootID,
		SyncJobID:         syncJobID,
		BaseGenerationID:  baseGenerationID,
		Seq:               seq,
		BaseGenerationSeq: baseSeq,
	}, nil
}

func (db *DB) GetSyncGeneration(ctx context.Context, orgID, rootID, generationID string) (*SyncGeneration, error) {
	var generation SyncGeneration
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, root_id, COALESCE(sync_job_id, ''), COALESCE(base_generation_id, ''), seq, base_generation_seq
		 FROM sync_generations
		 WHERE id = $1 AND org_id = $2 AND root_id = $3 AND status = 'building'`,
		generationID, orgID, rootID,
	).Scan(&generation.ID, &generation.OrgID, &generation.RootID, &generation.SyncJobID, &generation.BaseGenerationID, &generation.Seq, &generation.BaseGenerationSeq)
	if err != nil {
		return nil, err
	}
	return &generation, nil
}

func (db *DB) GetSyncGenerationForJob(ctx context.Context, orgID, rootID, jobID string) (*SyncGeneration, error) {
	var generation SyncGeneration
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, root_id, COALESCE(sync_job_id, ''), COALESCE(base_generation_id, ''), seq, base_generation_seq
		 FROM sync_generations
		 WHERE org_id = $1 AND root_id = $2 AND sync_job_id = $3
		 ORDER BY seq DESC
		 LIMIT 1`,
		orgID, rootID, jobID,
	).Scan(&generation.ID, &generation.OrgID, &generation.RootID, &generation.SyncJobID, &generation.BaseGenerationID, &generation.Seq, &generation.BaseGenerationSeq)
	if err != nil {
		return nil, err
	}
	return &generation, nil
}

func validateSyncBase(clientBaseGenerationID string, clientBaseGenerationSeq int64, visibleGenerationID string, visibleGenerationSeq int64) error {
	if clientBaseGenerationID != visibleGenerationID {
		return fmt.Errorf("%w: client base generation %q does not match visible generation %q", errStaleSyncBase, clientBaseGenerationID, visibleGenerationID)
	}
	if clientBaseGenerationSeq != 0 && clientBaseGenerationSeq != visibleGenerationSeq {
		return fmt.Errorf("%w: client base generation seq %d does not match visible generation seq %d", errStaleSyncBase, clientBaseGenerationSeq, visibleGenerationSeq)
	}
	return nil
}

func (db *DB) CommitSyncGeneration(ctx context.Context, generation *SyncGeneration, state map[string]models.FileState, stateRef string) error {
	if generation == nil {
		return fmt.Errorf("sync generation is required")
	}
	var stateJSON []byte
	var err error
	if stateRef == "" {
		stateJSON, err = json.Marshal(state)
		if err != nil {
			return err
		}
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

	if stateRef != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO root_states (root_id, state, state_ref, updated_at)
			 VALUES ($1, '{}'::jsonb, $2, NOW())
			 ON CONFLICT (root_id) DO UPDATE SET state = '{}'::jsonb, state_ref = $2, updated_at = NOW()`,
			generation.RootID, stateRef,
		); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx,
			`INSERT INTO root_states (root_id, state, state_ref, updated_at)
			 VALUES ($1, $2::jsonb, '', NOW())
			 ON CONFLICT (root_id) DO UPDATE SET state = $2::jsonb, state_ref = '', updated_at = NOW()`,
			generation.RootID, string(stateJSON),
		); err != nil {
			return err
		}
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

func (db *DB) GetSyncGenerationStatus(ctx context.Context, generationID string) (string, error) {
	var status string
	err := db.pool.QueryRow(ctx,
		`SELECT status FROM sync_generations WHERE id = $1`,
		generationID,
	).Scan(&status)
	return status, err
}

func (db *DB) ListFailedSyncGenerations(ctx context.Context, orgID, rootID string, limit int) ([]SyncGeneration, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, root_id, base_generation_id, seq, base_generation_seq
		 FROM sync_generations
		 WHERE org_id = $1 AND root_id = $2 AND status = 'failed'
		 ORDER BY created_at ASC
		 LIMIT $3`,
		orgID, rootID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var generations []SyncGeneration
	for rows.Next() {
		var generation SyncGeneration
		if err := rows.Scan(&generation.ID, &generation.OrgID, &generation.RootID, &generation.BaseGenerationID, &generation.Seq, &generation.BaseGenerationSeq); err != nil {
			return nil, err
		}
		generations = append(generations, generation)
	}
	return generations, rows.Err()
}

// ---------------------------------------------------------------------------
// Embedding Cache
// ---------------------------------------------------------------------------

// GetCachedEmbeddings looks up cached embeddings by content hash within an org,
// scoped to a specific embedding model version so vectors are never reused
// across model versions.
func (db *DB) GetCachedEmbeddings(ctx context.Context, orgID, modelVersion string, hashes []string) (map[string][]float64, error) {
	result := make(map[string][]float64)
	if len(hashes) == 0 {
		return result, nil
	}
	hashes = uniqueStrings(hashes)
	if len(hashes) == 0 {
		return result, nil
	}
	batchSize := embeddingCacheQueryBatchSize()
	if len(hashes) <= batchSize {
		return db.getCachedEmbeddingsBatch(ctx, orgID, modelVersion, hashes)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	batches := make(chan []string)
	workers := embeddingCacheQueryConcurrency()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batches {
				cached, err := db.getCachedEmbeddingsBatch(ctx, orgID, modelVersion, batch)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
				for hash, embedding := range cached {
					result[hash] = embedding
				}
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}

sendBatches:
	for start := 0; start < len(hashes); start += batchSize {
		end := start + batchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		select {
		case batches <- hashes[start:end]:
		case <-ctx.Done():
			break sendBatches
		}
	}
	close(batches)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

func (db *DB) getCachedEmbeddingsBatch(ctx context.Context, orgID, modelVersion string, hashes []string) (map[string][]float64, error) {
	result := make(map[string][]float64)
	rows, err := db.pool.Query(ctx,
		`SELECT content_hash, embedding FROM embedding_cache WHERE org_id = $1 AND model_version = $2 AND content_hash = ANY($3)`,
		orgID, modelVersion, hashes,
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
	return result, rows.Err()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func embeddingCacheQueryBatchSize() int {
	const defaultBatchSize = 500
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_EMBEDDING_CACHE_QUERY_BATCH_SIZE"))
	if raw == "" {
		return defaultBatchSize
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 {
		return defaultBatchSize
	}
	if size > 5000 {
		return 5000
	}
	return size
}

func embeddingCacheQueryConcurrency() int {
	const defaultConcurrency = 4
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_EMBEDDING_CACHE_QUERY_CONCURRENCY"))
	if raw == "" {
		return defaultConcurrency
	}
	concurrency, err := strconv.Atoi(raw)
	if err != nil || concurrency < 1 {
		return defaultConcurrency
	}
	if concurrency > 16 {
		return 16
	}
	return concurrency
}

// SaveCachedEmbeddings stores embeddings in the cache via a batched multi-value
// INSERT, keyed by (org_id, model_version, content_hash).
func (db *DB) SaveCachedEmbeddings(ctx context.Context, orgID, modelVersion string, entries map[string][]float64) error {
	if len(entries) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO embedding_cache (org_id, model_version, content_hash, embedding, created_at) VALUES `)
	args := make([]any, 0, len(entries)*2+2)
	args = append(args, orgID, modelVersion) // $1 = org_id, $2 = model_version
	i := 0
	for hash, emb := range entries {
		if i > 0 {
			sb.WriteString(", ")
		}
		p := i*2 + 3 // value placeholders start at $3 ($1=org_id, $2=model_version)
		fmt.Fprintf(&sb, "($1, $2, $%d, $%d, NOW())", p, p+1)
		args = append(args, hash, encodeEmbedding(emb))
		i++
	}
	sb.WriteString(` ON CONFLICT (org_id, model_version, content_hash) DO NOTHING`)

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
	job.UpdatedAt = job.StartedAt
	_, err := db.pool.Exec(ctx,
		`INSERT INTO sync_jobs (id, org_id, root_id, user_id, status, total_files, processed, started_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		job.ID, job.OrgID, job.RootID, job.UserID, job.Status, job.TotalFiles, job.Processed, job.StartedAt,
	)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// UpdateSyncJobStatus updates only the phase/status of a sync job.
func (db *DB) UpdateSyncJobStatus(ctx context.Context, jobID, status string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_jobs SET status = $1, updated_at = NOW() WHERE id = $2`,
		status, jobID,
	)
	return err
}

// TouchSyncJob persists liveness for a worker that is still processing a
// potentially long-running shard.
func (db *DB) TouchSyncJob(ctx context.Context, jobID string) error {
	if jobID == "" {
		return nil
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_jobs SET updated_at = NOW()
		 WHERE id = $1 AND status NOT IN ('completed', 'failed')`,
		jobID,
	)
	return err
}

// RecordSyncJobShard stores idempotent progress for a completed shard and
// refreshes the job-level processed count from completed index shards.
func (db *DB) RecordSyncJobShard(ctx context.Context, jobID, stage string, shardIndex, filesProcessed int) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO sync_job_shards (job_id, stage, shard_index, status, files_processed, finished_at)
		 VALUES ($1, $2, $3, 'completed', $4, NOW())
		 ON CONFLICT (job_id, stage, shard_index) DO UPDATE SET
			status = EXCLUDED.status,
			files_processed = EXCLUDED.files_processed,
			finished_at = EXCLUDED.finished_at`,
		jobID, stage, shardIndex, filesProcessed,
	); err != nil {
		return err
	}

	if stage == syncStageIndex {
		if _, err := tx.Exec(ctx,
			`UPDATE sync_jobs
			 SET processed = (
				SELECT SUM(files_processed)
				FROM sync_job_shards
				WHERE job_id = $1 AND stage = $2 AND status = 'completed'
			 )
			 WHERE id = $1`,
			jobID, syncStageIndex,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_jobs SET updated_at = NOW() WHERE id = $1`, jobID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// CompleteSyncJob marks a sync job as completed or failed.
func (db *DB) CompleteSyncJob(ctx context.Context, jobID, status string, errors []map[string]string) error {
	if errors == nil {
		errors = []map[string]string{}
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE sync_jobs SET status = $1, finished_at = NOW(), updated_at = NOW(), errors = $2 WHERE id = $3`,
		status, errors, jobID,
	)
	return err
}

// GetSyncJob retrieves a sync job by ID.
func (db *DB) GetSyncJob(ctx context.Context, orgID, jobID string) (*models.SyncJob, error) {
	job := &models.SyncJob{}
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, COALESCE(root_id, ''), user_id, status, total_files, processed, errors, started_at, updated_at, finished_at
		 FROM sync_jobs WHERE id = $1 AND org_id = $2`, jobID, orgID,
	).Scan(&job.ID, &job.OrgID, &job.RootID, &job.UserID, &job.Status, &job.TotalFiles,
		&job.Processed, &job.Errors, &job.StartedAt, &job.UpdatedAt, &job.FinishedAt)
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
		`SELECT id, org_id, COALESCE(root_id, ''), user_id, status, total_files, processed, errors, started_at, updated_at, finished_at
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
			&j.Processed, &j.Errors, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt); err != nil {
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
		`SELECT id, org_id, COALESCE(root_id, ''), user_id, status, total_files, processed, errors, started_at, updated_at, finished_at
		 FROM sync_jobs WHERE org_id = $1 AND root_id = $2
		 ORDER BY started_at DESC LIMIT 1`,
		orgID, rootID,
	).Scan(&job.ID, &job.OrgID, &job.RootID, &job.UserID, &job.Status, &job.TotalFiles,
		&job.Processed, &job.Errors, &job.StartedAt, &job.UpdatedAt, &job.FinishedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListSyncJobsForReconciliation returns active jobs whose persisted progress
// heartbeat is stale, or whose generation has already failed.
func (db *DB) ListSyncJobsForReconciliation(ctx context.Context, staleBefore time.Time, limit int) ([]models.SyncJob, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT j.id, j.org_id, COALESCE(j.root_id, ''), j.user_id, j.status,
		        j.total_files, j.processed, j.errors, j.started_at, j.updated_at, j.finished_at
		 FROM sync_jobs j
		 WHERE j.status NOT IN ('completed', 'failed')
		   AND (
		     j.updated_at < $1
		     OR EXISTS (
		       SELECT 1 FROM sync_generations g
		       WHERE g.sync_job_id = j.id AND g.status = 'failed'
		     )
		   )
		 ORDER BY j.updated_at ASC
		 LIMIT $2`,
		staleBefore, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]models.SyncJob, 0)
	for rows.Next() {
		var job models.SyncJob
		if err := rows.Scan(&job.ID, &job.OrgID, &job.RootID, &job.UserID, &job.Status,
			&job.TotalFiles, &job.Processed, &job.Errors, &job.StartedAt, &job.UpdatedAt, &job.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
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
