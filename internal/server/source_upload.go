package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/sourcecapture"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	maxSourceUploadBytes      = int64(10 << 30)
	defaultMultipartPartBytes = int64(16 << 20)
	minMultipartPartBytes     = int64(5 << 20)
	maxMultipartPartBytes     = int64(5 << 30)
	maxMultipartParts         = 10000
	multipartPartURLLifetime  = time.Hour
	maxMultipartControlBytes  = int64(1 << 20)
	minSourceRangeBytes       = int64(1 << 20)
)

type multipartObjectStore interface {
	CreateMultipartUpload(ctx context.Context, key, contentType string) (string, error)
	PresignMultipartPart(ctx context.Context, key, uploadID string, partNumber int32, contentLength int64, expires time.Duration) (string, map[string][]string, error)
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, size int64, parts []storage.CompletedPart) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
}

func sourceWorkRangeBytes() int64 {
	// Leave room for the range planner's line/UTF-8 boundary slack so a
	// completed range still fits the execution shard's estimated work budget.
	const defaultBytes = int64(defaultSyncShardMaxChunks*2000 - (64 << 10) - 8)
	value, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("PUFFERFS_SOURCE_RANGE_BYTES")), 10, 64)
	if value < minSourceRangeBytes {
		return defaultBytes
	}
	return min(value, defaultBytes)
}

func multipartPartBytes(size int64) (int64, int, error) {
	partSize, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("PUFFERFS_MULTIPART_PART_BYTES")), 10, 64)
	if partSize < minMultipartPartBytes {
		partSize = defaultMultipartPartBytes
	}
	partSize = min(partSize, maxMultipartPartBytes)
	minimum := (size + maxMultipartParts - 1) / maxMultipartParts
	if minimum > partSize {
		const alignment = int64(1 << 20)
		partSize = ((minimum + alignment - 1) / alignment) * alignment
	}
	if partSize > maxMultipartPartBytes {
		return 0, 0, fmt.Errorf("source is too large for an S3 multipart upload")
	}
	partCount := int((size + partSize - 1) / partSize)
	if partCount < 1 || partCount > maxMultipartParts {
		return 0, 0, fmt.Errorf("multipart upload requires %d parts; maximum is %d", partCount, maxMultipartParts)
	}
	return partSize, partCount, nil
}

func uploadCapturedSource(ctx context.Context, store objectStore, key, filePath string, body io.Reader, expectedSize int64) (models.SourceUploadResponse, error) {
	hash := sha256.New()
	counter := &byteCounter{}
	ranges := sourcecapture.NewRangePlanner(localChunkable(filePath), sourceWorkRangeBytes())
	body = io.TeeReader(body, io.MultiWriter(hash, counter, ranges))
	if err := store.UploadStream(ctx, key, body, "application/octet-stream"); err != nil {
		return models.SourceUploadResponse{}, err
	}
	if expectedSize >= 0 && counter.n != expectedSize {
		_ = store.DeleteMany(ctx, []string{key})
		return models.SourceUploadResponse{}, fmt.Errorf("%w: expected %d bytes, received %d", errUploadLengthMismatch, expectedSize, counter.n)
	}
	return models.SourceUploadResponse{
		Key:          key,
		ContentHash:  "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		Size:         counter.n,
		SourceRanges: ranges.Finish(),
	}, nil
}

func (s *Server) handleMultipartSourceInit(w http.ResponseWriter, r *http.Request) {
	var req models.MultipartSourceInitRequest
	if !decodeSourceUploadRequest(w, r, &req) {
		return
	}
	filePath, generation, ok := s.authorizeSourceUpload(w, r, req.GenerationID, req.Path)
	if !ok {
		return
	}
	if req.Size < 1 || req.Size > maxSourceUploadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("source size must be between 1 and %d bytes", maxSourceUploadBytes)})
		return
	}
	partSize, partCount, err := multipartPartBytes(req.Size)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	store, ok := s.s3.(multipartObjectStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "direct multipart uploads are unavailable"})
		return
	}
	key := syncSourceCaptureFileKey(generation.ID, uuid.NewString(), filePath)
	uploadID, err := store.CreateMultipartUpload(r.Context(), key, "application/octet-stream")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "starting multipart upload: " + err.Error()})
		return
	}
	if err := s.finishGenerationUpload(r.Context(), generation); err != nil {
		_ = store.AbortMultipartUpload(context.WithoutCancel(r.Context()), key, uploadID)
		writeGenerationUploadLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models.MultipartSourceInitResponse{
		Key:        key,
		UploadID:   uploadID,
		PartSize:   partSize,
		PartCount:  partCount,
		RangeBytes: sourceRangeBytes(filePath),
	})
}

func (s *Server) handleMultipartSourcePart(w http.ResponseWriter, r *http.Request) {
	var req models.MultipartSourcePartRequest
	if !decodeSourceUploadRequest(w, r, &req) {
		return
	}
	filePath, ok := multipartSourcePath(req.GenerationID, req.Key)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart key must reference this generation's source file"})
		return
	}
	_, generation, authorized := s.authorizeSourceUpload(w, r, req.GenerationID, filePath)
	if !authorized {
		return
	}
	if req.UploadID == "" || req.PartNumber < 1 || req.PartNumber > maxMultipartParts || req.Size < 1 || req.Size > maxMultipartPartBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart part request"})
		return
	}
	store, ok := s.s3.(multipartObjectStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "direct multipart uploads are unavailable"})
		return
	}
	partNumber := int32(req.PartNumber)
	partURL, headers, err := store.PresignMultipartPart(r.Context(), req.Key, req.UploadID, partNumber, req.Size, multipartPartURLLifetime)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "signing multipart part: " + err.Error()})
		return
	}
	if err := s.finishGenerationUpload(r.Context(), generation); err != nil {
		writeGenerationUploadLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models.MultipartSourcePartResponse{URL: partURL, Headers: headers})
}

func (s *Server) handleMultipartSourceComplete(w http.ResponseWriter, r *http.Request) {
	var req models.MultipartSourceCompleteRequest
	if !decodeSourceUploadRequest(w, r, &req) {
		return
	}
	filePath, generation, ok := s.authorizeSourceUpload(w, r, req.GenerationID, req.Path)
	if !ok {
		return
	}
	if req.Size < 1 || req.Size > maxSourceUploadBytes || req.UploadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart completion metadata"})
		return
	}
	storedPath, validKey := multipartSourcePath(req.GenerationID, req.Key)
	if !validKey || storedPath != filePath {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart key does not match source path"})
		return
	}
	if req.PartSize < minMultipartPartBytes || req.PartSize > maxMultipartPartBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart completion metadata"})
		return
	}
	partCount := int((req.Size + req.PartSize - 1) / req.PartSize)
	if partCount < 1 || partCount > maxMultipartParts || partCount != len(req.Parts) || !validSHA256(req.ContentHash) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart completion metadata"})
		return
	}
	if err := validateSourceRanges(filePath, req.Size, req.SourceRanges); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	parts := make([]storage.CompletedPart, len(req.Parts))
	for i, part := range req.Parts {
		if part.PartNumber != i+1 || strings.TrimSpace(part.ETag) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart parts must be complete and ordered"})
			return
		}
		parts[i] = storage.CompletedPart{PartNumber: int32(part.PartNumber), ETag: part.ETag}
	}
	store, ok := s.s3.(multipartObjectStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "direct multipart uploads are unavailable"})
		return
	}
	if err := store.CompleteMultipartUpload(r.Context(), req.Key, req.UploadID, req.Size, parts); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "completing multipart upload: " + err.Error()})
		return
	}
	if err := s.finishGenerationUpload(r.Context(), generation); err != nil {
		_ = s.s3.DeleteMany(context.WithoutCancel(r.Context()), []string{req.Key})
		writeGenerationUploadLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models.SourceUploadResponse{
		Key: req.Key, ContentHash: req.ContentHash, Size: req.Size, SourceRanges: req.SourceRanges,
	})
}

func sourceRangeBytes(filePath string) int64 {
	if !localChunkable(filePath) {
		return 0
	}
	return sourceWorkRangeBytes()
}

func (s *Server) handleMultipartSourceAbort(w http.ResponseWriter, r *http.Request) {
	var req models.MultipartSourceAbortRequest
	if !decodeSourceUploadRequest(w, r, &req) {
		return
	}
	filePath, ok := multipartSourcePath(req.GenerationID, req.Key)
	if !ok || req.UploadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart abort request"})
		return
	}
	if _, _, authorized := s.authorizeSourceUpload(w, r, req.GenerationID, filePath); !authorized {
		return
	}
	store, ok := s.s3.(multipartObjectStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "direct multipart uploads are unavailable"})
		return
	}
	if err := store.AbortMultipartUpload(r.Context(), req.Key, req.UploadID); err != nil && !isObjectNotFound(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "aborting multipart upload: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func decodeSourceUploadRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMultipartControlBytes)).Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}

func (s *Server) authorizeSourceUpload(w http.ResponseWriter, r *http.Request, generationID, rawPath string) (string, *SyncGeneration, bool) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return "", nil, false
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return "", nil, false
	}
	rootID := r.PathValue("id")
	filePath, err := cleanFilePath(rawPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return "", nil, false
	}
	if _, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync); err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return "", nil, false
	}
	if !s.checkWriteACL(r.Context(), id, rootID, filePath) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no write permission for this path"})
		return "", nil, false
	}
	generation, err := s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, strings.TrimSpace(generationID))
	if err != nil {
		writeGenerationUploadLookupError(w, err)
		return "", nil, false
	}
	return filePath, generation, true
}

func multipartSourcePath(generationID, key string) (string, bool) {
	prefix := syncSourceFilePrefix(generationID)
	if prefix == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(key, prefix)
	slash := strings.IndexByte(remainder, '/')
	if slash <= 0 || slash == len(remainder)-1 {
		return "", false
	}
	captureID := strings.TrimPrefix(remainder[:slash], ".capture-")
	if captureID == remainder[:slash] {
		return "", false
	}
	if _, err := uuid.Parse(captureID); err != nil {
		return "", false
	}
	return remainder[slash+1:], true
}

func validSHA256(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return strings.HasPrefix(value, "sha256:") && err == nil && len(decoded) == sha256.Size
}

func validateSourceRanges(filePath string, size int64, ranges []models.SourceRange) error {
	if len(ranges) == 0 {
		return nil
	}
	if !localChunkable(filePath) {
		return fmt.Errorf("source_ranges are only valid for locally chunkable files")
	}
	if len(ranges) < 2 {
		return fmt.Errorf("source_ranges must contain at least two ranges")
	}
	targetBytes := sourceWorkRangeBytes()
	maxRanges := (size + targetBytes - 1) / targetBytes
	if int64(len(ranges)) > maxRanges {
		return fmt.Errorf("source_ranges contains %d ranges; maximum is %d", len(ranges), maxRanges)
	}
	maxRangeBytes := sourcecapture.MaximumRangeBytes(targetBytes)
	nextOffset := int64(0)
	lastLine := int64(1)
	for i, sourceRange := range ranges {
		if i == 0 && sourceRange.LineStart != 1 {
			return fmt.Errorf("source_ranges[0] must start at line 1")
		}
		if sourceRange.Offset != nextOffset || sourceRange.Length <= 0 || sourceRange.LineStart < lastLine {
			return fmt.Errorf("source_ranges[%d] is not contiguous and ordered", i)
		}
		if sourceRange.Length > maxRangeBytes {
			return fmt.Errorf("source_ranges[%d] exceeds the %d-byte execution limit", i, maxRangeBytes)
		}
		if i < len(ranges)-1 && sourceRange.Length < targetBytes {
			return fmt.Errorf("source_ranges[%d] is smaller than the %d-byte target", i, targetBytes)
		}
		if sourceRange.LineStart > size+1 {
			return fmt.Errorf("source_ranges[%d] has an invalid starting line", i)
		}
		nextOffset += sourceRange.Length
		if nextOffset < 0 || nextOffset > size {
			return fmt.Errorf("source_ranges exceed source size")
		}
		lastLine = sourceRange.LineStart
	}
	if nextOffset != size {
		return fmt.Errorf("source_ranges cover %d bytes; expected %d", nextOffset, size)
	}
	return nil
}
