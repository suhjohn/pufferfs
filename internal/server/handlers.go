package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

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
	s.mux.HandleFunc("POST /roots/{id}/upload", s.handleUpload)
	s.mux.HandleFunc("POST /roots/{id}/sync", s.handleSync)
	s.mux.HandleFunc("GET /roots/{id}/state", s.handleGetState)
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

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("id")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path query param required"})
		return
	}

	// Limit upload size to 512 MB
	const maxUploadSize = 512 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()})
		return
	}

	s3Key := fmt.Sprintf("files/%s/%s", rootID, filePath)
	if err := s.s3.Upload(r.Context(), s3Key, data, "application/octet-stream"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": s3Key})
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("id")
	state, err := s.db.LoadState(r.Context(), rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
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
	// For MODIFIED files, delete old chunks first
	if change.Status == models.StatusModified {
		filter := []any{"file_path", "Eq", change.Path}
		if err := s.tp.DeleteByFilter(rootID, filter); err != nil {
			log.Printf("warning: failed to delete old chunks for %s: %v", change.Path, err)
		}
	}

	// The CLI uploads the file to S3 before calling sync
	s3Key := fmt.Sprintf("files/%s/%s", rootID, change.Path)

	// Download file content from S3 to send inline to Modal
	fileData, err := s.s3.Download(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("downloading %s from S3: %w", s3Key, err)
	}

	// Call Modal to chunk the file (send content inline as base64)
	chunkResp, err := s.modal.ChunkFile(ChunkFileRequest{
		S3Key:      s3Key,
		FilePath:   change.Path,
		FileType:   detectFileType(change.Path),
		RootID:     rootID,
		ContentB64: base64.StdEncoding.EncodeToString(fileData),
	})
	if err != nil {
		return fmt.Errorf("chunking: %w", err)
	}

	if len(chunkResp.Chunks) == 0 {
		return nil
	}

	// Check embedding cache — skip re-embedding for unchanged chunks
	var contentHashes []string
	for _, c := range chunkResp.Chunks {
		if h, ok := c["content_hash"].(string); ok {
			contentHashes = append(contentHashes, h)
		}
	}

	cached, err := s.db.GetCachedEmbeddings(ctx, contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup failed: %v", err)
		cached = make(map[string][]float64)
	}

	// Split chunks into cached (have embedding) and uncached (need embedding)
	var uncachedChunks []map[string]any
	cachedRows := make(map[int][]float64)

	for i, c := range chunkResp.Chunks {
		hash, _ := c["content_hash"].(string)
		if emb, ok := cached[hash]; ok {
			cachedRows[i] = emb
		} else {
			uncachedChunks = append(uncachedChunks, c)
		}
	}

	log.Printf("file %s: %d chunks total, %d cached, %d to embed",
		change.Path, len(chunkResp.Chunks), len(cachedRows), len(uncachedChunks))

	// Embed only uncached chunks via Modal
	var embedResults []map[string]any
	if len(uncachedChunks) > 0 {
		embedResp, err := s.modal.EmbedChunks(uncachedChunks)
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		embedResults = embedResp.Results

		// Save new embeddings to cache
		newCacheEntries := make(map[string][]float64)
		for _, r := range embedResults {
			chunk, _ := r["chunk"].(map[string]any)
			hash, _ := chunk["content_hash"].(string)
			if emb, ok := r["embedding"].([]any); ok && hash != "" {
				embFloat := make([]float64, len(emb))
				for k, v := range emb {
					if f, ok := v.(float64); ok {
						embFloat[k] = f
					}
				}
				newCacheEntries[hash] = embFloat
			}
		}
		if err := s.db.SaveCachedEmbeddings(ctx, newCacheEntries); err != nil {
			log.Printf("warning: failed to save embedding cache: %v", err)
		}
	}

	// Build Turbopuffer rows: merge cached + freshly embedded
	rows := make([]map[string]any, len(chunkResp.Chunks))
	embedIdx := 0
	for i, c := range chunkResp.Chunks {
		row := map[string]any{
			"id":           c["id"],
			"content":      c["content"],
			"file_path":    c["file_path"],
			"chunk_index":  c["chunk_index"],
			"content_hash": c["content_hash"],
			"file_type":    c["file_type"],
			"root_id":      rootID,
		}
		if pn, ok := c["page_number"]; ok && pn != nil {
			row["page_number"] = pn
		}
		if ip, ok := c["image_path"]; ok && ip != nil {
			row["image_path"] = ip
		}

		if emb, ok := cachedRows[i]; ok {
			row["vector"] = emb
		} else if embedIdx < len(embedResults) {
			r := embedResults[embedIdx]
			row["vector"], _ = r["embedding"].([]any)
			embedIdx++
		}

		rows[i] = row
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

	// 4. Re-upsert with new file path using cached embeddings (no re-embedding)
	var contentHashes []string
	for _, row := range rows {
		if h, ok := row["content_hash"].(string); ok {
			contentHashes = append(contentHashes, h)
		}
	}

	cached, err := s.db.GetCachedEmbeddings(ctx, contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup for move failed: %v", err)
		cached = make(map[string][]float64)
	}

	// Build new rows; fall back to re-embedding only for chunks missing from cache
	var uncachedChunks []map[string]any
	newRows := make([]map[string]any, len(rows))
	for i, row := range rows {
		chunkIdx := 0
		if ci, ok := row["chunk_index"]; ok {
			if f, ok := ci.(float64); ok {
				chunkIdx = int(f)
			}
		}
		newRows[i] = map[string]any{
			"id":           models.MakeChunkID(rootID, change.Path, chunkIdx),
			"content":      row["content"],
			"file_path":    change.Path,
			"chunk_index":  chunkIdx,
			"content_hash": row["content_hash"],
			"file_type":    row["file_type"],
			"root_id":      rootID,
			"page_number":  row["page_number"],
			"image_path":   row["image_path"],
		}

		hash, _ := row["content_hash"].(string)
		if emb, ok := cached[hash]; ok {
			newRows[i]["vector"] = emb
		} else {
			uncachedChunks = append(uncachedChunks, newRows[i])
		}
	}

	// Only call Modal for chunks not in cache
	if len(uncachedChunks) > 0 {
		log.Printf("move %s: %d/%d chunks need re-embedding (cache miss)", change.Path, len(uncachedChunks), len(rows))
		embedResp, err := s.modal.EmbedChunks(uncachedChunks)
		if err != nil {
			return fmt.Errorf("re-embedding moved chunks: %w", err)
		}
		embedIdx := 0
		for i := range newRows {
			if _, hasVec := newRows[i]["vector"]; !hasVec && embedIdx < len(embedResp.Results) {
				newRows[i]["vector"], _ = embedResp.Results[embedIdx]["embedding"].([]any)
				embedIdx++
			}
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

	// Build glob filter if specified
	var filters any
	if req.Glob != "" {
		filters = []any{"file_path", "Glob", req.Glob}
	}

	var rows []map[string]any
	var err error

	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, err = s.tp.Query(req.RootID, rankBy, req.TopK, filters, includeAttrs)

	case "vector":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, err = s.tp.Query(req.RootID, rankBy, req.TopK, filters, includeAttrs)

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
	ext := strings.ToLower(filepath.Ext(path))
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
