package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	// pgx stdlib adapter for goose
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/migrations"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// DB wraps the Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
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

var errRootDeleting = errors.New("root is being deleted")

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
	var schema fs.FS = migrations.Files
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		schema = os.DirFS(dir)
	}
	gooseDB, err := goose.OpenDBWithDriver("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer gooseDB.Close()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, gooseDB, schema,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migration provider: %w", err)
	}
	_, err = provider.Up(context.Background())
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
	var keyID, orgID, userID, role string
	var scopes []string
	err := db.pool.QueryRow(ctx,
		`SELECT ak.id, ak.org_id, ak.user_id, om.role, ak.scopes
		 FROM api_keys ak
		 JOIN org_members om ON om.org_id = ak.org_id AND om.user_id = ak.user_id
		 WHERE ak.key_hash = $1
		   AND (ak.expires_at IS NULL OR ak.expires_at > NOW())`,
		keyHash,
	).Scan(&keyID, &orgID, &userID, &role, &scopes)
	if err != nil {
		return nil, err
	}

	var email string
	_ = db.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)

	return &auth.Identity{
		UserID:   userID,
		OrgID:    orgID,
		Role:     auth.Role(role),
		Email:    email,
		Scopes:   scopes,
		APIKeyID: keyID,
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

// PrepareRootDeletion makes deletion terminal before remote cleanup starts.
// The root row stays present so a failed cleanup can be retried, while the
// deleting marker prevents concurrent capture registration. Historical jobs
// are cancelled as an upgrade safeguard; this runtime creates none.
func (db *DB) PrepareRootDeletion(ctx context.Context, orgID, rootID string) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE roots
		 SET deleting_at = COALESCE(deleting_at, NOW()), updated_at = NOW()
		 WHERE id = $1 AND org_id = $2`,
		rootID, orgID,
	)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, pgx.ErrNoRows
	}

	if _, err := tx.Exec(ctx,
		`UPDATE sync_generations
		 SET status = 'failed'
		 WHERE org_id = $1 AND root_id = $2 AND status = 'building'`,
		orgID, rootID,
	); err != nil {
		return 0, err
	}

	tag, err = tx.Exec(ctx,
		`UPDATE sync_jobs
		 SET status = 'failed',
		     finished_at = NOW(),
		     updated_at = NOW(),
		     errors = errors || '[{"error":"root deleted while sync was active"}]'::jsonb
		 WHERE org_id = $1 AND root_id = $2
		   AND status NOT IN ('completed', 'failed')`,
		orgID, rootID,
	)
	if err != nil {
		return 0, err
	}
	cancelled := int(tag.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return cancelled, nil
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
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rootGrantPermissions])
	if err != nil {
		return nil, "", err
	}
	permissions, source := effectiveRootPermissions(root, userID, role, grants)
	return permissions, source, nil
}

type rootGrantPermissions struct {
	PrincipalType string
	Permissions   []string
}

// Permission policy is shared by ordinary reads and locked capture snapshots.
func effectiveRootPermissions(root *models.RootMetadata, userID string, role auth.Role, grants []rootGrantPermissions) ([]string, string) {
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

	for _, grant := range grants {
		add(grant.PrincipalType, grant.Permissions...)
	}
	return sortedRootPermissions(permissions), source
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
	// Serialize deny insertion with per-file capture acceptance, not its slow
	// source upload. A deny committed before capture takes the lock is observed.
	tag, err := db.pool.Exec(ctx,
		`WITH capture_root AS MATERIALIZED (
		 SELECT id FROM roots WHERE id=$3 AND org_id=$2 AND deleting_at IS NULL FOR UPDATE)
		 INSERT INTO root_acls (id, org_id, root_id, path_prefix, grant_to, permission, created_at)
		 SELECT $1, $2, $3, $4, $5, $6, $7 FROM capture_root`,
		acl.ID, acl.OrgID, acl.RootID, acl.PathPrefix, acl.GrantTo, acl.Permission, acl.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, pgx.ErrNoRows
	}
	return acl, nil
}

type aclQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// GetACLsForUser returns all ACL entries that apply to a user for a root.
// Matches documented user/role/wildcard targets and historical bare user IDs.
func (db *DB) GetACLsForUser(ctx context.Context, orgID, rootID, userID string, role auth.Role) ([]models.RootACL, error) {
	return getACLsForUser(ctx, db.pool, orgID, rootID, userID, role)
}

func getACLsForUser(ctx context.Context, queryer aclQueryer, orgID, rootID, userID string, role auth.Role) ([]models.RootACL, error) {
	grantTargets := []string{
		userID,
		"user:" + userID,
		"role:" + string(role),
		"*",
	}

	rows, err := queryer.Query(ctx,
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
	return acls, rows.Err()
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

func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}
