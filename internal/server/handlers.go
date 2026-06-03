package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Server holds the dependencies for HTTP handlers.
type Server struct {
	db      *DB
	s3      *storage.Client
	modal   *ModalClient
	tp      *TPClient
	mux     *http.ServeMux
}

// New creates a new Server with all dependencies.
func New(db *DB, s3 *storage.Client, modal *ModalClient, tp *TPClient) *Server {
	s := &Server{
		db:    db,
		s3:    s3,
		modal: modal,
		tp:    tp,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /roots", s.handleCreateRoot)
	s.mux.HandleFunc("GET /roots", s.handleListRoots)
	s.mux.HandleFunc("GET /roots/{id}", s.handleGetRoot)
	s.mux.HandleFunc("POST /roots/{id}/sync", s.handleSync)
	s.mux.HandleFunc("POST /query", s.handleQuery)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateRoot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		SourcePath string `json:"source_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	root, err := s.db.CreateRoot(r.Context(), req.Name, req.SourcePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, root)
}

func (s *Server) handleListRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.db.ListRoots(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, roots)
}

func (s *Server) handleGetRoot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	writeJSON(w, http.StatusOK, root)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("id")

	var req models.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.RootID = rootID

	// Verify root exists
	_, err := s.db.GetRoot(r.Context(), rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	resp, err := s.processSync(r.Context(), &req)
	if err != nil {
		log.Printf("sync error for root %s: %v", rootID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Persist state
	if err := s.db.SaveState(r.Context(), rootID, req.State); err != nil {
		log.Printf("error saving state for root %s: %v", rootID, err)
	}
	if err := s.db.UpdateRootTimestamp(r.Context(), rootID); err != nil {
		log.Printf("error updating timestamp for root %s: %v", rootID, err)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) processSync(ctx context.Context, req *models.SyncRequest) (*models.SyncResponse, error) {
	resp := &models.SyncResponse{RootID: req.RootID}

	for _, change := range req.Changes {
		switch change.Status {
		case models.StatusAdded, models.StatusModified:
			if err := s.processFileAdd(ctx, req.RootID, change); err != nil {
				return nil, fmt.Errorf("processing %s (%s): %w", change.Path, change.Status, err)
			}
			resp.ChunksAdded++
			resp.FilesProcessed++

		case models.StatusRemoved:
			if err := s.processFileRemove(ctx, req.RootID, change); err != nil {
				return nil, fmt.Errorf("removing %s: %w", change.Path, err)
			}
			resp.ChunksRemoved++
			resp.FilesProcessed++

		case models.StatusMoved, models.StatusRenamed:
			if err := s.processFileMove(ctx, req.RootID, change); err != nil {
				return nil, fmt.Errorf("moving %s -> %s: %w", change.OldPath, change.Path, err)
			}
			resp.ChunksMoved++
			resp.FilesProcessed++

		case models.StatusUnchanged:
			// nothing to do
		}
	}

	return resp, nil
}

func (s *Server) processFileAdd(ctx context.Context, rootID string, change models.FileChange) error {
	// The CLI uploads the file to S3 before calling sync
	s3Key := fmt.Sprintf("files/%s/%s", rootID, change.Path)

	// Call Modal to chunk the file
	chunkResp, err := s.modal.ChunkFile(ChunkFileRequest{
		S3Key:    s3Key,
		FilePath: change.Path,
		FileType: detectFileType(change.Path),
		RootID:   rootID,
	})
	if err != nil {
		return fmt.Errorf("chunking: %w", err)
	}

	if len(chunkResp.Chunks) == 0 {
		return nil
	}

	// Call Modal to embed the chunks
	embedResp, err := s.modal.EmbedChunks(chunkResp.Chunks)
	if err != nil {
		return fmt.Errorf("embedding: %w", err)
	}

	// Upsert to Turbopuffer
	rows := make([]map[string]any, len(embedResp.Results))
	for i, r := range embedResp.Results {
		chunk, _ := r["chunk"].(map[string]any)
		embedding, _ := r["embedding"].([]any)

		rows[i] = map[string]any{
			"id":           chunk["id"],
			"vector":       embedding,
			"content":      chunk["content"],
			"file_path":    chunk["file_path"],
			"chunk_index":  chunk["chunk_index"],
			"content_hash": chunk["content_hash"],
			"file_type":    chunk["file_type"],
			"root_id":      rootID,
		}
		if pn, ok := chunk["page_number"]; ok && pn != nil {
			rows[i]["page_number"] = pn
		}
		if ip, ok := chunk["image_path"]; ok && ip != nil {
			rows[i]["image_path"] = ip
		}
	}

	return s.tp.UpsertRows(rootID, rows, "cosine_distance")
}

func (s *Server) processFileRemove(ctx context.Context, rootID string, change models.FileChange) error {
	// Delete from Turbopuffer by file_path filter
	filter := []any{"file_path", "Eq", change.Path}
	return s.tp.DeleteByFilter(rootID, filter)
}

func (s *Server) processFileMove(ctx context.Context, rootID string, change models.FileChange) error {
	// 1. Rename S3 objects
	oldFileKey := fmt.Sprintf("files/%s/%s", rootID, change.OldPath)
	newFileKey := fmt.Sprintf("files/%s/%s", rootID, change.Path)
	if err := s.s3.Rename(ctx, oldFileKey, newFileKey); err != nil {
		log.Printf("warning: S3 rename failed (may not exist): %v", err)
	}

	// 2. Query existing chunks to get their vectors and data
	rows, err := s.tp.Query(rootID,
		[]any{"file_path", "asc"},
		10000,
		[]any{"file_path", "Eq", change.OldPath},
		[]string{"content", "file_path", "chunk_index", "content_hash", "file_type", "page_number", "image_path"},
	)
	if err != nil {
		return fmt.Errorf("querying old chunks: %w", err)
	}

	if len(rows) == 0 {
		return nil
	}

	// 3. Delete old IDs
	oldIDs := make([]string, len(rows))
	for i, row := range rows {
		oldIDs[i] = fmt.Sprintf("%v", row["id"])
	}
	if err := s.tp.DeleteIDs(rootID, oldIDs); err != nil {
		return fmt.Errorf("deleting old chunks: %w", err)
	}

	// 4. Re-embed with new file path (need the vector data)
	// Since we can't get vectors back from tp queries easily,
	// and content is identical, we re-embed via Modal.
	// This is a tradeoff: move operations do re-embed.
	// TODO: optimize by caching embeddings in S3.
	chunkDicts := make([]map[string]any, len(rows))
	for i, row := range rows {
		chunkIdx := 0
		if ci, ok := row["chunk_index"]; ok {
			if f, ok := ci.(float64); ok {
				chunkIdx = int(f)
			}
		}
		chunkDicts[i] = map[string]any{
			"id":           models.MakeChunkID(rootID, change.Path, chunkIdx),
			"root_id":      rootID,
			"file_path":    change.Path,
			"chunk_index":  chunkIdx,
			"content":      row["content"],
			"content_hash": row["content_hash"],
			"file_type":    row["file_type"],
			"page_number":  row["page_number"],
			"image_path":   row["image_path"],
		}
	}

	embedResp, err := s.modal.EmbedChunks(chunkDicts)
	if err != nil {
		return fmt.Errorf("re-embedding moved chunks: %w", err)
	}

	newRows := make([]map[string]any, len(embedResp.Results))
	for i, r := range embedResp.Results {
		chunk, _ := r["chunk"].(map[string]any)
		embedding, _ := r["embedding"].([]any)
		newRows[i] = map[string]any{
			"id":           chunk["id"],
			"vector":       embedding,
			"content":      chunk["content"],
			"file_path":    chunk["file_path"],
			"chunk_index":  chunk["chunk_index"],
			"content_hash": chunk["content_hash"],
			"file_type":    chunk["file_type"],
			"root_id":      rootID,
			"page_number":  chunk["page_number"],
			"image_path":   chunk["image_path"],
		}
	}

	return s.tp.UpsertRows(rootID, newRows, "cosine_distance")
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req models.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	if req.RootID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "root_id is required"})
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	if req.Mode == "" {
		req.Mode = "hybrid"
	}

	includeAttrs := []string{"content", "file_path", "chunk_index", "file_type", "page_number", "image_path"}

	var rows []map[string]any
	var err error

	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, err = s.tp.Query(req.RootID, rankBy, req.TopK, nil, includeAttrs)

	case "vector":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, err = s.tp.Query(req.RootID, rankBy, req.TopK, nil, includeAttrs)

	case "hybrid":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rows, err = s.tp.HybridSearch(req.RootID, req.Query, embedding, req.TopK, req.Glob)

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be fts, vector, or hybrid"})
		return
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	results := make([]models.QueryResult, len(rows))
	for i, row := range rows {
		results[i] = models.QueryResult{
			FilePath: strVal(row, "file_path"),
			Content:  strVal(row, "content"),
			FileType: strVal(row, "file_type"),
			Score:    floatVal(row, "$dist"),
		}
		if ci, ok := row["chunk_index"]; ok {
			if f, ok := ci.(float64); ok {
				results[i].ChunkIndex = int(f)
			}
		}
		if pn, ok := row["page_number"]; ok && pn != nil {
			if f, ok := pn.(float64); ok {
				n := int(f)
				results[i].PageNumber = &n
			}
		}
		if ip, ok := row["image_path"]; ok && ip != nil {
			if s, ok := ip.(string); ok {
				results[i].ImagePath = &s
			}
		}
	}

	writeJSON(w, http.StatusOK, models.QueryResponse{
		Results: results,
		Query:   req.Query,
		Mode:    req.Mode,
	})
}

func detectFileType(path string) string {
	ext := filepath.Ext(path)
	switch ext {
	case ".pdf":
		return "pdf"
	case ".docx", ".doc":
		return "docx"
	case ".pptx", ".ppt":
		return "pptx"
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp":
		return "image"
	default:
		return "auto"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func floatVal(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}
