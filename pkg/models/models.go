// Package models defines shared data types for PufferFs.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MakeChunkID produces a deterministic chunk ID from root, path, and index.
func MakeChunkID(rootID, filePath string, chunkIndex int) string {
	h := sha256.Sum256([]byte(rootID + ":" + filePath))
	pathHash := hex.EncodeToString(h[:])[:16]
	return fmt.Sprintf("%s:%d", pathHash, chunkIndex)
}

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

// Organization represents a tenant.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// User represents an authenticated user.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"created_at"`
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
	ID         string    `json:"id" db:"id"`
	OrgID      string    `json:"org_id" db:"org_id"`
	Name       string    `json:"name" db:"name"`
	SourcePath string    `json:"source_path" db:"source_path"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time `json:"updated_at" db:"updated_at"`
}

// RootWithSimHash is a root with its SimHash for similarity matching.
type RootWithSimHash struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SourcePath string `json:"source_path"`
	SimHash    string `json:"simhash"`
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
	StatusUnchanged        FileChangeStatus = "UNCHANGED"
	StatusAdded            FileChangeStatus = "ADDED"
	StatusRemoved          FileChangeStatus = "REMOVED"
	StatusModified         FileChangeStatus = "MODIFIED"
	StatusMoved            FileChangeStatus = "MOVED"
	StatusRenamed          FileChangeStatus = "RENAMED"
	StatusCopied           FileChangeStatus = "COPIED"
	StatusMovedAndModified FileChangeStatus = "MOVED_AND_MODIFIED"
)

// FileChange describes a single file's change between two states.
type FileChange struct {
	Path        string           `json:"path"`
	Status      FileChangeStatus `json:"status"`
	OldPath     string           `json:"old_path,omitempty"`
	ContentHash string           `json:"content_hash"`
	Size        int64            `json:"size"`
}

// DiffStats summarises counts per status.
type DiffStats struct {
	Unchanged int `json:"unchanged"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Modified  int `json:"modified"`
	Moved     int `json:"moved"`
	Renamed   int `json:"renamed"`
	Copied    int `json:"copied"`
}

// DiffResult is the output of a directory diff.
type DiffResult struct {
	Changes []FileChange `json:"changes"`
	Stats   DiffStats    `json:"stats"`
}

// ---------------------------------------------------------------------------
// Chunks & Embeddings
// ---------------------------------------------------------------------------

// Chunk is a text chunk extracted from a file.
type Chunk struct {
	ID          string  `json:"id"`
	RootID      string  `json:"root_id"`
	FilePath    string  `json:"file_path"`
	ChunkIndex  int     `json:"chunk_index"`
	Content     string  `json:"content"`
	ContentHash string  `json:"content_hash"`
	FileType    string  `json:"file_type"`
	PageNumber  *int    `json:"page_number,omitempty"`
	ImagePath   *string `json:"image_path,omitempty"`
}

// ChunkWithEmbedding is a Chunk plus its embedding vector.
type ChunkWithEmbedding struct {
	Chunk     Chunk     `json:"chunk"`
	Embedding []float64 `json:"embedding"`
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

// SyncRequest is sent from CLI to server to trigger a sync.
type SyncRequest struct {
	RootID       string               `json:"root_id"`
	Changes      []FileChange         `json:"changes"`
	State        map[string]FileState `json:"state"`
	SimHash      string               `json:"simhash,omitempty"`
	ContentProof *ContentProofData    `json:"content_proof,omitempty"`
}

// ContentProofData is the serialized content proof sent with sync/query requests.
type ContentProofData struct {
	FileHashes map[string]string `json:"file_hashes"`
	DirHashes  map[string]string `json:"dir_hashes"`
	RootHash   string            `json:"root_hash"`
}

// SyncResponse is returned from server after sync completes.
type SyncResponse struct {
	RootID         string `json:"root_id"`
	SyncJobID      string `json:"sync_job_id,omitempty"`
	ChunksAdded    int    `json:"chunks_added"`
	ChunksRemoved  int    `json:"chunks_removed"`
	ChunksMoved    int    `json:"chunks_moved"`
	FilesProcessed int    `json:"files_processed"`
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

// QueryRequest is sent from CLI to server to query indexed content.
type QueryRequest struct {
	Query  string `json:"query"`
	Mode   string `json:"mode"`
	RootID string `json:"root_id"`
	Glob   string `json:"glob,omitempty"`
	TopK   int    `json:"top_k"`
}

// QueryResult is a single search result returned to the user.
type QueryResult struct {
	FilePath   string  `json:"file_path"`
	ChunkIndex int     `json:"chunk_index"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	FileType   string  `json:"file_type"`
	PageNumber *int    `json:"page_number,omitempty"`
	ImagePath  *string `json:"image_path,omitempty"`
}

// QueryResponse wraps query results.
type QueryResponse struct {
	Results []QueryResult `json:"results"`
	Query   string        `json:"query"`
	Mode    string        `json:"mode"`
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

// ---------------------------------------------------------------------------
// Sync Jobs
// ---------------------------------------------------------------------------

// SyncJob tracks the lifecycle of a sync operation.
type SyncJob struct {
	ID         string              `json:"id"`
	OrgID      string              `json:"org_id"`
	RootID     string              `json:"root_id"`
	UserID     string              `json:"user_id"`
	Status     string              `json:"status"`
	TotalFiles int                 `json:"total_files"`
	Processed  int                 `json:"processed"`
	Errors     json.RawMessage     `json:"errors"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
}
