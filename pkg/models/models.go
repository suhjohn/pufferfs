// Package models defines shared data types for PufferFs.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// MakeChunkID produces a deterministic chunk ID from root, path, and index.
func MakeChunkID(rootID, filePath string, chunkIndex int) string {
	h := sha256.Sum256([]byte(rootID + ":" + filePath))
	pathHash := hex.EncodeToString(h[:])[:16]
	return fmt.Sprintf("%s:%d", pathHash, chunkIndex)
}

// RootMetadata represents a synced directory root.
type RootMetadata struct {
	ID         string    `json:"id" db:"id"`
	Name       string    `json:"name" db:"name"`
	SourcePath string    `json:"source_path" db:"source_path"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time `json:"updated_at" db:"updated_at"`
}

// FileState stores the last-known state of a single file for diff computation.
type FileState struct {
	Size        int64   `json:"size"`
	ContentHash string  `json:"content_hash"`
	Mtime       float64 `json:"mtime"`
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

// Chunk is a text chunk extracted from a file.
type Chunk struct {
	ID          string `json:"id"`
	RootID      string `json:"root_id"`
	FilePath    string `json:"file_path"`
	ChunkIndex  int    `json:"chunk_index"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	FileType    string `json:"file_type"`
	PageNumber  *int   `json:"page_number,omitempty"`
	ImagePath   *string `json:"image_path,omitempty"`
}

// ChunkWithEmbedding is a Chunk plus its embedding vector.
type ChunkWithEmbedding struct {
	Chunk     Chunk     `json:"chunk"`
	Embedding []float64 `json:"embedding"`
}

// SyncRequest is sent from CLI to server to trigger a sync.
type SyncRequest struct {
	RootID  string       `json:"root_id"`
	Changes []FileChange `json:"changes"`
	State   map[string]FileState `json:"state"`
}

// SyncResponse is returned from server after sync completes.
type SyncResponse struct {
	RootID       string    `json:"root_id"`
	ChunksAdded  int       `json:"chunks_added"`
	ChunksRemoved int      `json:"chunks_removed"`
	ChunksMoved  int       `json:"chunks_moved"`
	FilesProcessed int     `json:"files_processed"`
}

// QueryRequest is sent from CLI to server to query indexed content.
type QueryRequest struct {
	Query  string `json:"query"`
	Mode   string `json:"mode"`   // "fts", "vector", "hybrid"
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
