package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/sourcecapture"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const extractionRevision = "visual-gemini-3.5-flash-lite-v2"

func (s *Server) captureIdentity(w http.ResponseWriter, r *http.Request) *auth.Identity {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return nil
	}
	if _, ok, err := s.rootForPermission(r.Context(), id, r.PathValue("id"), models.RootPermissionSync); err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return nil
	}
	return id
}

func decodeCaptureRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid capture request"})
		return false
	}
	return true
}

func (s *Server) handleSourcePackInit(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.SourcePackInitRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	if input.Size < 1 || input.Size > 128<<20 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pack size must be 1..128 MiB"})
		return
	}
	store := s.s3
	key := fmt.Sprintf("sources/%s/%s/packs/%s", id.OrgID, r.PathValue("id"), uuid.NewString())
	// Registration locks against root deletion. No uploaded bytes are trusted
	// until the completion endpoint checks the actual object size.
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "source registration failed"})
		return
	}
	defer tx.Rollback(r.Context())
	var rootID string
	err = tx.QueryRow(r.Context(), `SELECT id FROM roots WHERE id=$1 AND org_id=$2 AND deleting_at IS NULL FOR UPDATE`, r.PathValue("id"), id.OrgID).Scan(&rootID)
	if err == nil {
		if input.ObjectKey == "" {
			_, err = tx.Exec(r.Context(), `INSERT INTO source_objects(object_key,org_id,root_id,size_bytes,uploader_id) VALUES($1,$2,$3,$4,$5)`, key, id.OrgID, rootID, input.Size, id.UserID)
		} else {
			// Renewal cannot allocate an arbitrary key or change its declared size.
			err = tx.QueryRow(r.Context(), `UPDATE source_objects o SET authorized_until=NOW()+INTERVAL '15 minutes' WHERE object_key=$1 AND org_id=$2 AND root_id=$3 AND size_bytes=$4 AND uploader_id=$5 AND retired_at IS NULL AND NOT EXISTS(SELECT 1 FROM source_multipart_uploads m WHERE m.object_key=o.object_key) RETURNING object_key`, input.ObjectKey, id.OrgID, rootID, input.Size, id.UserID).Scan(&key)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if s.writeRetiredSourcePack(w, r, input.ObjectKey, id.UserID) {
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": "root unavailable for capture"})
		return
	}
	url, headers, err := store.PresignImmutablePut(r.Context(), key, input.Size)
	if err == nil {
		err = s.pinSignedSourceUpload(r.Context(), key, id.UserID, url)
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "could not authorize source upload"})
		return
	}
	writeJSON(w, http.StatusOK, models.SourcePackInitResponse{ObjectKey: key, URL: url, Headers: headers})
}

// Persist the ACTUAL signed deadline before exposing the URL. If cleanup won
// while signing/credentials were stalled, fail without giving out that URL.
func (s *Server) pinSignedSourceUpload(ctx context.Context, key, userID, signedURL string) error {
	parsed, err := url.Parse(signedURL)
	if err != nil {
		return errors.New("invalid source upload authorization")
	}
	issued, err := time.Parse("20060102T150405Z", parsed.Query().Get("X-Amz-Date"))
	if err != nil {
		return errors.New("invalid source upload authorization time")
	}
	seconds, err := strconv.Atoi(parsed.Query().Get("X-Amz-Expires"))
	if err != nil || seconds < 1 || seconds > 900 {
		return errors.New("invalid source upload authorization lifetime")
	}
	var pinned string
	return s.db.pool.QueryRow(ctx, `UPDATE source_objects o SET authorized_until=GREATEST(authorized_until,$3)
        FROM roots r WHERE o.object_key=$1 AND o.uploader_id=$2 AND o.retired_at IS NULL
        AND r.id=o.root_id AND r.deleting_at IS NULL RETURNING o.object_key`, key, userID, issued.Add(time.Duration(seconds)*time.Second)).Scan(&pinned)
}

func (s *Server) handleSourcePackComplete(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.SourcePackCompleteRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	var expected int64
	err := s.db.pool.QueryRow(r.Context(), `UPDATE source_objects o SET authorized_until=NOW()+INTERVAL '15 minutes' WHERE object_key=$1 AND org_id=$2 AND root_id=$3 AND uploader_id=$4 AND retired_at IS NULL AND NOT EXISTS(SELECT 1 FROM source_multipart_uploads m WHERE m.object_key=o.object_key) RETURNING size_bytes`, input.ObjectKey, id.OrgID, r.PathValue("id"), id.UserID).Scan(&expected)
	if err != nil {
		if s.writeRetiredSourcePack(w, r, input.ObjectKey, id.UserID) {
			return
		}
		writeJSON(w, 404, map[string]string{"error": "source upload not found"})
		return
	}
	store := s.s3
	actual, err := store.ObjectSize(r.Context(), input.ObjectKey)
	if err != nil || actual != expected {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "source upload incomplete or size mismatch"})
		return
	}
	var acceptedKey string
	err = s.db.pool.QueryRow(r.Context(), `UPDATE source_objects o SET completed_at=COALESCE(completed_at,NOW()) FROM roots r WHERE o.object_key=$1 AND o.org_id=$2 AND o.root_id=$3 AND o.uploader_id=$4 AND o.retired_at IS NULL AND r.id=o.root_id AND r.deleting_at IS NULL RETURNING o.object_key`, input.ObjectKey, id.OrgID, r.PathValue("id"), id.UserID).Scan(&acceptedKey)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "source completion failed"})
		return
	}
	writeJSON(w, http.StatusOK, models.SourcePackCompleteResponse{ObjectKey: input.ObjectKey, Status: "complete"})
}

// Only the uploader can learn that a specific owned upload was retired.
// Clients must never interpret an arbitrary 403/404 or network error as GC.
func (s *Server) writeRetiredSourcePack(w http.ResponseWriter, r *http.Request, key, userID string) bool {
	var retired bool
	err := s.db.pool.QueryRow(r.Context(), `SELECT retired_at IS NOT NULL FROM source_objects WHERE object_key=$1 AND root_id=$2 AND uploader_id=$3`, key, r.PathValue("id"), userID).Scan(&retired)
	if err != nil || !retired {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{"code": "source_pack_reupload_required", "error": "source pack retired or lacks upload provenance; re-upload retained bytes", "object_keys": []string{key}})
	return true
}

func (s *Server) handleRegisterFileVersions(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	if s.queue == nil {
		writeJSON(w, 503, map[string]string{"error": "SQS processing is unavailable"})
		return
	}
	var input models.CaptureVersionsRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	if _, err := uuid.Parse(input.CaptureID); err != nil || len(input.Files) < 1 || len(input.Files) > 128 {
		writeJSON(w, 400, map[string]string{"error": "capture_id must be a UUID; files must contain 1..128 entries"})
		return
	}
	rootID := r.PathValue("id")
	// Root sync permission was checked by captureIdentity. Load path ACLs once
	// for the entire registration batch; database failure is not an empty ACL.
	acls, err := s.db.GetACLsForUser(r.Context(), id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": errFilePermissionsUnavailable.Error()})
		return
	}
	for _, f := range input.Files {
		path, err := cleanFilePath(f.Path)
		if err != nil || path != f.Path || !checkPermission(acls, path, "write") {
			writeJSON(w, 403, map[string]string{"error": "capture path is invalid or not writable"})
			return
		}
		if f.Deleted {
			if f.Source != nil {
				writeJSON(w, 400, map[string]string{"error": "deleted file cannot include source"})
				return
			}
			continue
		}
		if f.Source == nil {
			writeJSON(w, 400, map[string]string{"error": "source manifest is required"})
			return
		}
		if err = sourcecapture.ValidateManifest(*f.Source); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	pack, err := packSourceManifests(id.OrgID, rootID, input.Files)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if pack.key != "" {
		if err = s.s3.Upload(r.Context(), pack.key, pack.body, "application/x-ndjson"); err != nil {
			writeJSON(w, 503, map[string]string{"error": "source manifest persistence failed"})
			return
		}
	}
	versions := make([]CapturedFileVersion, 0, len(input.Files))
	for i, f := range input.Files {
		v := CapturedFileVersion{Path: f.Path, PreviousVersionID: f.PreviousVersionID, Deleted: f.Deleted}
		if !f.Deleted {
			v.ContentHash, v.Size, v.SourceManifestRef = f.Source.ContentHash, f.Source.Size, pack.refs[i]
			v.Extents = f.Source.Extents
		}
		versions = append(versions, v)
	}
	registered, err := s.db.RegisterFileVersions(r.Context(), id, rootID, input.CaptureID, extractionRevision, versions)
	if err != nil {
		status := http.StatusInternalServerError
		body := map[string]any{"error": err.Error()}
		if errors.Is(err, ErrFileVersionConflict) || errors.Is(err, errRootDeleting) {
			status = http.StatusConflict
		}
		if errors.Is(err, ErrFileVersionConflict) {
			body["code"] = "capture_version_conflict"
		}
		if errors.Is(err, ErrCapturePathForbidden) {
			status = http.StatusForbidden
		}
		var retired *retiredSourcePacksError
		if errors.As(err, &retired) {
			status = http.StatusConflict
			body["code"], body["object_keys"] = "source_pack_reupload_required", retired.Keys
		}
		if errors.Is(err, errSourceExtentUnavailable) {
			status = http.StatusBadRequest
		}

		writeJSON(w, status, body)
		return
	}
	// Acceptance is durable even if SQS is temporarily unavailable. The
	// reconciliation role republishes the unmarked delivery ledger records.
	// A proof-write failure is recoverable by retrying this same capture ID;
	// never acknowledge it while the user's captured hashes are unrecorded.
	if err = s.db.RecordCapturedProofs(r.Context(), id.OrgID, id.UserID, rootID, registered.versions); err != nil {
		writeJSON(w, 500, map[string]string{"error": "captured proof persistence failed; retry the same capture"})
		return
	}
	if err = s.publishFileWork(r.Context(), registered.deliveries); err != nil {
		log.Printf("file capture accepted; SQS handoff needs reconciliation: %v", err)
	}
	writeJSON(w, http.StatusAccepted, models.CaptureVersionsResponse{CaptureID: input.CaptureID, Versions: registered.versions})
}
