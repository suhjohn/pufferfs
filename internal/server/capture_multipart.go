package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const capturePartSize = int64(16 << 20)

func (s *Server) handleCaptureMultipartInit(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.SourceMultipartInitRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	requestID, err := uuid.Parse(input.RequestID)
	if err != nil || input.Size < 1 || input.Size > 128<<20 {
		writeJSON(w, 400, map[string]string{"error": "request_id must be a UUID; pack size must be 1..128 MiB"})
		return
	}
	store := s.s3
	rootID := r.PathValue("id")
	key := fmt.Sprintf("sources/%s/%s/multipart/%s", id.OrgID, rootID, requestID)
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "upload registration failed"})
		return
	}
	defer tx.Rollback(r.Context())
	var root string
	err = tx.QueryRow(r.Context(), `SELECT id FROM roots WHERE id=$1 AND org_id=$2 AND deleting_at IS NULL FOR UPDATE`, rootID, id.OrgID).Scan(&root)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO source_objects(object_key,org_id,root_id,size_bytes,uploader_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, key, id.OrgID, rootID, input.Size, id.UserID)
	}
	if err == nil {
		err = tx.QueryRow(r.Context(), `UPDATE source_objects SET authorized_until=NOW()+INTERVAL '15 minutes' WHERE object_key=$1 AND size_bytes=$2 AND uploader_id=$3 AND retired_at IS NULL RETURNING object_key`, key, input.Size, id.UserID).Scan(&key)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO source_multipart_uploads(object_key,part_size) SELECT object_key,$2 FROM source_objects WHERE object_key=$1 AND size_bytes=$3 ON CONFLICT DO NOTHING`, key, capturePartSize, input.Size)
	}
	var result models.SourceMultipartInitResponse
	result.ObjectKey = key
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT m.upload_id,m.part_size,o.completed_at IS NOT NULL FROM source_multipart_uploads m JOIN source_objects o USING(object_key) WHERE object_key=$1 AND o.size_bytes=$2`, key, input.Size).Scan(&result.UploadID, &result.PartSize, &result.Complete)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if s.writeRetiredSourcePack(w, r, key, id.UserID) {
			return
		}
		writeJSON(w, 409, map[string]string{"error": "upload identity/size conflict or root unavailable"})
		return
	}
	result.PartCount = int((input.Size + result.PartSize - 1) / result.PartSize)
	if result.UploadID == "" {
		created, createErr := store.CreateImmutableMultipartUpload(r.Context(), key, key)
		if createErr != nil {
			writeJSON(w, 503, map[string]string{"error": "multipart creation failed; retry the same request_id"})
			return
		}
		// A concurrent initializer may win. Persist one session before exposing
		// any signed part URLs; abort a known losing session outside the DB.
		var winner string
		err = s.db.pool.QueryRow(r.Context(), `UPDATE source_multipart_uploads m SET upload_id=CASE WHEN upload_id='' THEN $2 ELSE upload_id END FROM source_objects o WHERE m.object_key=$1 AND o.object_key=m.object_key AND o.retired_at IS NULL RETURNING upload_id`, key, created).Scan(&winner)
		if err == nil && winner != created {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			_ = store.AbortMultipartUpload(cleanup, key, created)
			cancel()
		}
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "upload persistence failed; retry the same request_id"})
			return
		}
		result.UploadID = winner
	} else if !result.Complete {
		complete, checkErr := store.CheckImmutableMultipartUpload(r.Context(), key, result.UploadID, key, input.Size)
		if errors.Is(checkErr, storage.ErrMultipartUploadExpired) {
			writeJSON(w, 409, map[string]string{"code": models.SourceMultipartExpired,
				"error": "multipart upload expired; retain captured bytes and start a new request_id"})
			return
		}
		if checkErr != nil {
			writeJSON(w, 503, map[string]string{"error": "multipart status unavailable; retry the same request_id"})
			return
		}
		if complete {
			// An S3 completion may have succeeded before its DB acknowledgement.
			// Accept only our frozen completion manifest and a still-live root.
			var acceptedKey string
			err = s.db.pool.QueryRow(r.Context(), `UPDATE source_objects o SET completed_at=COALESCE(o.completed_at,NOW())
				FROM roots r,source_multipart_uploads m WHERE o.object_key=$1 AND o.org_id=$2 AND o.root_id=$3
				AND r.id=o.root_id AND r.deleting_at IS NULL AND o.retired_at IS NULL AND m.object_key=o.object_key
				AND m.upload_id=$4 AND m.completion_parts IS NOT NULL RETURNING o.object_key`,
				key, id.OrgID, rootID, result.UploadID).Scan(&acceptedKey)
			if err != nil {
				writeJSON(w, 503, map[string]string{"error": "completion persistence failed; retry the same request_id"})
				return
			}
			result.Complete = true
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCaptureMultipartPart(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.SourceMultipartPartRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	var uploadID string
	var size, partSize int64
	err := s.db.pool.QueryRow(r.Context(), `UPDATE source_objects o SET authorized_until=NOW()+INTERVAL '15 minutes' FROM source_multipart_uploads m
		WHERE o.object_key=m.object_key AND o.object_key=$1 AND o.org_id=$2 AND o.root_id=$3 AND o.uploader_id=$4 AND o.retired_at IS NULL AND o.completed_at IS NULL AND m.completion_parts IS NULL AND m.upload_id<>'' RETURNING m.upload_id,o.size_bytes,m.part_size`, input.ObjectKey, id.OrgID, r.PathValue("id"), id.UserID).Scan(&uploadID, &size, &partSize)
	if err != nil {
		if s.writeRetiredSourcePack(w, r, input.ObjectKey, id.UserID) {
			return
		}
		writeJSON(w, 409, map[string]string{"error": "multipart upload unavailable or completion already started"})
		return
	}
	count := (size + partSize - 1) / partSize
	if input.PartNumber < 1 || int64(input.PartNumber) > count {
		writeJSON(w, 400, map[string]string{"error": "invalid part number"})
		return
	}
	store := s.s3
	length := min(partSize, size-int64(input.PartNumber-1)*partSize)
	url, headers, err := store.PresignMultipartPart(r.Context(), input.ObjectKey, uploadID, input.PartNumber, length, 15*time.Minute)
	if err == nil {
		err = s.pinSignedSourceUpload(r.Context(), input.ObjectKey, id.UserID, url)
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "part authorization failed"})
		return
	}
	writeJSON(w, 200, models.SourceMultipartPartResponse{URL: url, Headers: headers, Size: length})
}

func (s *Server) handleCaptureMultipartComplete(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.SourceMultipartCompleteRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	if len(input.Parts) < 1 || len(input.Parts) > 8 {
		writeJSON(w, 400, map[string]string{"error": "invalid completion parts"})
		return
	}
	parts := make([]storage.CompletedPart, len(input.Parts))
	for i, part := range input.Parts {
		if part.PartNumber != int32(i+1) || part.ETag == "" || len(part.ETag) > 1024 {
			writeJSON(w, 400, map[string]string{"error": "parts must be contiguous with nonempty ETags"})
			return
		}
		parts[i] = storage.CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag}
	}
	data, _ := json.Marshal(input.Parts)
	var uploadID string
	var size int64
	var complete bool
	// Freeze the exact completion manifest before S3. Retries may not change
	// ETags or complete the same durable upload with a different parts list.
	err := s.db.pool.QueryRow(r.Context(), `WITH live AS (
        UPDATE source_objects SET authorized_until=NOW()+INTERVAL '15 minutes'
        WHERE object_key=$1 AND org_id=$2 AND root_id=$3 AND uploader_id=$6 AND retired_at IS NULL RETURNING *)
        UPDATE source_multipart_uploads m SET completion_parts=$4::jsonb FROM live o
		WHERE m.object_key=o.object_key AND o.object_key=$1 AND o.org_id=$2 AND o.root_id=$3
		AND m.upload_id<>'' AND (m.completion_parts IS NULL OR m.completion_parts=$4::jsonb)
		AND (o.size_bytes+m.part_size-1)/m.part_size=$5
		RETURNING m.upload_id,o.size_bytes,o.completed_at IS NOT NULL`, input.ObjectKey, id.OrgID, r.PathValue("id"), data, len(parts), id.UserID).Scan(&uploadID, &size, &complete)
	if err != nil {
		if s.writeRetiredSourcePack(w, r, input.ObjectKey, id.UserID) {
			return
		}
		writeJSON(w, 409, map[string]string{"error": "multipart identity or completion parts conflict"})
		return
	}
	if !complete {
		if err = s.s3.CompleteImmutableMultipartUpload(r.Context(), input.ObjectKey, uploadID, input.ObjectKey, size, parts); err != nil {
			writeJSON(w, 503, map[string]string{"error": "multipart completion failed; retry the identical parts"})
			return
		}
		var acceptedKey string
		err = s.db.pool.QueryRow(r.Context(), `UPDATE source_objects o SET completed_at=COALESCE(o.completed_at,NOW()) FROM roots r WHERE o.object_key=$1 AND o.org_id=$2 AND o.root_id=$3 AND o.retired_at IS NULL AND r.id=o.root_id AND r.deleting_at IS NULL RETURNING o.object_key`, input.ObjectKey, id.OrgID, r.PathValue("id")).Scan(&acceptedKey)
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "completion persistence failed; retry the identical parts"})
			return
		}
	}
	writeJSON(w, 200, models.SourcePackCompleteResponse{ObjectKey: input.ObjectKey, Status: "complete"})
}
