// Package models defines shared data types for PufferFs.
package models

import (
	"time"
)

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

// Organization represents a tenant.
type Organization struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	ExternalID string    `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// User represents an authenticated user.
type User struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	AvatarURL  string    `json:"avatar_url"`
	Provider   string    `json:"provider"`
	ExternalID string    `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AuthMeResponse describes the identity and credential scopes for the current request.
type AuthMeResponse struct {
	User   User     `json:"user"`
	OrgID  string   `json:"org_id"`
	Role   string   `json:"role"`
	Scopes []string `json:"scopes"`
}

// OrgMember is a user's membership in an org.
type OrgMember struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url"`
	Role      string    `json:"role"`
	JoinedAt  time.Time `json:"joined_at"`
}

// Group is an organization-scoped collection of users used for root grants.
type Group struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	Name       string    `json:"name"`
	ExternalID string    `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// GroupMember is a user's membership in an organization group.
type GroupMember struct {
	GroupID  string    `json:"group_id"`
	UserID   string    `json:"user_id"`
	Email    string    `json:"email,omitempty"`
	Name     string    `json:"name,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
}

// OrgInvite is a pending email invitation to join an org.
type OrgInvite struct {
	ID              string    `json:"id"`
	Email           string    `json:"email"`
	Role            string    `json:"role"`
	InvitedByUserID string    `json:"invited_by_user_id"`
	CreatedAt       time.Time `json:"created_at"`
}

// IgnorePolicy stores a raw gitignore-style policy document.
type IgnorePolicy struct {
	OrgID           string    `json:"org_id,omitempty"`
	UserID          string    `json:"user_id,omitempty"`
	Patterns        string    `json:"patterns"`
	UpdatedByUserID string    `json:"updated_by_user_id,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// EffectiveIgnorePolicy is the centrally managed policy visible to a caller.
type EffectiveIgnorePolicy struct {
	OrgPatterns  string `json:"org_patterns"`
	UserPatterns string `json:"user_patterns"`
}

// IgnorePolicyUpdateRequest updates a raw gitignore-style policy document.
type IgnorePolicyUpdateRequest struct {
	Patterns string `json:"patterns"`
}

// ---------------------------------------------------------------------------
// API Keys
// ---------------------------------------------------------------------------

// APIKey represents an API key (never includes the raw key value).
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Roots & Files
// ---------------------------------------------------------------------------

// RootMetadata represents a synced directory root.
type RootMetadata struct {
	ID             string    `json:"id" db:"id"`
	OrgID          string    `json:"org_id" db:"org_id"`
	Name           string    `json:"name" db:"name"`
	SourcePath     string    `json:"source_path" db:"source_path"`
	Scope          string    `json:"scope" db:"scope"`
	OwnerUserID    string    `json:"owner_user_id,omitempty" db:"owner_user_id"`
	VectorDisabled bool      `json:"vector_disabled" db:"vector_disabled"`
	Access         []string  `json:"access,omitempty" db:"-"`
	AccessSource   string    `json:"access_source,omitempty" db:"-"`
	CreatedAt      time.Time `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time `json:"updated_at" db:"updated_at"`
}

// RootIndexNamespace maps a logical root to one physical Turbopuffer namespace shard.
type RootIndexNamespace struct {
	ID         string     `json:"id" db:"id"`
	OrgID      string     `json:"org_id" db:"org_id"`
	RootID     string     `json:"root_id" db:"root_id"`
	Namespace  string     `json:"namespace" db:"namespace"`
	ShardIndex int        `json:"shard_index" db:"shard_index"`
	ShardCount int        `json:"shard_count" db:"shard_count"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	RetiredAt  *time.Time `json:"retired_at,omitempty" db:"retired_at"`
}

const (
	RootScopeOrg        = "org"
	RootScopeUser       = "user"
	RootScopeRestricted = "restricted"
)

const (
	RootPermissionRead   = "read"
	RootPermissionSync   = "sync"
	RootPermissionDelete = "delete"
	RootPermissionAdmin  = "admin"
)

// RootGrant gives a principal access to a root.
type RootGrant struct {
	ID            string    `json:"id"`
	OrgID         string    `json:"org_id"`
	RootID        string    `json:"root_id"`
	PrincipalType string    `json:"principal_type"`
	PrincipalID   string    `json:"principal_id"`
	Permissions   []string  `json:"permissions"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// FileState stores the last-known state of a single file for diff computation.
type FileState struct {
	Size        int64  `json:"size"`
	ContentHash string `json:"content_hash"`
	Mtime       int64  `json:"mtime"`
}

// FileChangeStatus enumerates possible diff outcomes.
type FileChangeStatus string

const (
	StatusUnchanged FileChangeStatus = "UNCHANGED"
	StatusAdded     FileChangeStatus = "ADDED"
	StatusRemoved   FileChangeStatus = "REMOVED"
	StatusModified  FileChangeStatus = "MODIFIED"
)

// FileChange describes a single file's change between two states.
type FileChange struct {
	Path         string           `json:"path"`
	AbsolutePath string           `json:"absolute_path,omitempty"`
	Status       FileChangeStatus `json:"status"`
	ContentHash  string           `json:"content_hash"`
	Size         int64            `json:"size"`
}

// DiffStats summarises counts per status.
type DiffStats struct {
	Unchanged int `json:"unchanged"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Modified  int `json:"modified"`
}

// DiffResult is the output of a directory diff.
type DiffResult struct {
	Changes []FileChange `json:"changes"`
	Stats   DiffStats    `json:"stats"`
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

const SyncProtocolVersion = 2

// CLIReleaseManifest describes the currently supported CLI release range and
// the download assets available for direct upgrades.
type CLIReleaseManifest struct {
	Latest      string                 `json:"latest"`
	Minimum     string                 `json:"minimum"`
	ProtocolMin int                    `json:"protocol_min"`
	ProtocolMax int                    `json:"protocol_max"`
	Downloads   map[string]CLIDownload `json:"downloads,omitempty"`
	NotesURL    string                 `json:"notes_url,omitempty"`
}

// CLIDownload is a platform-specific CLI binary asset.
type CLIDownload struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// QueryRequest is sent from CLI to server to query indexed content.
type QueryRequest struct {
	Query    string   `json:"query"`
	Mode     string   `json:"mode"`
	RootID   string   `json:"root_id,omitempty"`
	RootIDs  []string `json:"root_ids,omitempty"`
	AllRoots bool     `json:"all_roots,omitempty"`
	Glob     string   `json:"glob,omitempty"`
	TopK     int      `json:"top_k"`
}

// QueryResult is a single search result returned to the user.
type QueryResult struct {
	RootID       string  `json:"root_id,omitempty"`
	RootName     string  `json:"root_name,omitempty"`
	FilePath     string  `json:"file_path"`
	AbsolutePath string  `json:"absolute_path,omitempty"`
	ChunkIndex   int     `json:"chunk_index"`
	Content      string  `json:"content"`
	Score        float64 `json:"score"`
	FileType     string  `json:"file_type"`
	PageNumber   *int    `json:"page_number,omitempty"`
}

// QueryResponse wraps query results.
type QueryResponse struct {
	Results       []QueryResult `json:"results"`
	Query         string        `json:"query"`
	Mode          string        `json:"mode"`
	RootsSearched int           `json:"roots_searched,omitempty"`
}

// ReadRange is a 1-based inclusive range for deterministic file reads.
type ReadRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ReadFileRequest asks the server for a deterministic slice of a known file.
type ReadFileRequest struct {
	Path  string     `json:"path"`
	Pages *ReadRange `json:"pages,omitempty"`
	Lines *ReadRange `json:"lines,omitempty"`
}

// ReadPageResult is one document page returned by a read request.
type ReadPageResult struct {
	Page         int    `json:"page"`
	PageNumber   int    `json:"page_number"`
	ChunkIndex   int    `json:"chunk_index"`
	Content      string `json:"content"`
	AbsolutePath string `json:"absolute_path,omitempty"`
	FileType     string `json:"file_type,omitempty"`
}

// ReadLineResult is one source line returned by a read request.
type ReadLineResult struct {
	LineNumber int    `json:"line_number"`
	Content    string `json:"content"`
}

// ReadFileResponse wraps deterministic file-slice results.
type ReadFileResponse struct {
	RootID       string           `json:"root_id"`
	RootName     string           `json:"root_name,omitempty"`
	FilePath     string           `json:"file_path"`
	AbsolutePath string           `json:"absolute_path,omitempty"`
	Mode         string           `json:"mode"`
	Pages        []ReadPageResult `json:"pages,omitempty"`
	Lines        []ReadLineResult `json:"lines,omitempty"`
}

// ---------------------------------------------------------------------------
// ACLs
// ---------------------------------------------------------------------------

// RootACL represents a folder-level access control entry.
type RootACL struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	RootID     string    `json:"root_id"`
	PathPrefix string    `json:"path_prefix"`
	GrantTo    string    `json:"grant_to"`
	Permission string    `json:"permission"`
	CreatedAt  time.Time `json:"created_at"`
}
