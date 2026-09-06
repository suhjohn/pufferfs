package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// CapturedFileVersion describes an already persisted source manifest. The API
// validates its format; registration authorizes byte ranges transactionally.
type CapturedFileVersion struct {
	Path              string                `json:"path"`
	PreviousVersionID string                `json:"previous_version_id"`
	ContentHash       string                `json:"content_hash"`
	Size              int64                 `json:"size"`
	SourceManifestRef string                `json:"source_manifest_ref"`
	Deleted           bool                  `json:"deleted"`
	Extents           []models.SourceExtent `json:"-"`
}

type RegisteredFileVersion = models.RegisteredFileVersion

var ErrFileVersionConflict = errors.New("captured file version changed")
var ErrCapturePathForbidden = errors.New("capture path is not writable")

type retiredSourcePacksError struct{ Keys []string }

func (e *retiredSourcePacksError) Error() string {
	return "source packs retired or lack upload provenance; re-upload the retained captured bytes"
}

var errSourceExtentUnavailable = errors.New("source extent is unavailable, outside its object, or not authorized for this file")
var errSourceCatalogPending = errors.New("source catalog backfill pending; retry the same capture")

// RegisterFileVersions atomically advances captured heads and records the SQS
// handoffs that must be published. It never waits for or advances indexing.
// Repeating an identical capture is idempotent even after a later capture.
func (db *DB) RegisterFileVersions(ctx context.Context, identity *auth.Identity, rootID, captureID, revision string, files []CapturedFileVersion) ([]RegisteredFileVersion, error) {
	if identity == nil || identity.OrgID == "" || identity.UserID == "" || captureID == "" || revision == "" || len(files) == 0 {
		return nil, fmt.Errorf("capture ID, extraction revision and files are required")
	}
	orgID := identity.OrgID
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		path, err := cleanFilePath(f.Path)
		if err != nil || path != f.Path || seen[path] {
			return nil, fmt.Errorf("invalid or repeated capture path %q", f.Path)
		}
		seen[path] = true
		if f.Size < 0 || (!f.Deleted && (f.SourceManifestRef == "" || !validSHA256(f.ContentHash))) || (f.Deleted && (f.Size != 0 || f.SourceManifestRef != "" || f.ContentHash != "")) {
			return nil, fmt.Errorf("invalid captured version for %s", f.Path)
		}
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// A short registration lock coordinates root deletion and concurrent capture
	// transactions, not transformation or indexing. No lock survives this method.
	var deletingAt *time.Time
	root := &models.RootMetadata{ID: rootID, OrgID: orgID}
	if err = tx.QueryRow(ctx, `SELECT deleting_at,scope,COALESCE(owner_user_id,'') FROM roots WHERE id=$1 AND org_id=$2 FOR UPDATE`, rootID, orgID).Scan(&deletingAt, &root.Scope, &root.OwnerUserID); err != nil {
		return nil, err
	}
	if deletingAt != nil {
		return nil, errRootDeleting
	}
	role, err := authorizeCaptureCommit(ctx, tx, identity, root)
	if err != nil {
		return nil, err
	}
	// The initial API check precedes S3 IO. Recheck folder denies while holding
	// the same root lock as deny insertion, so a committed revocation cannot be
	// bypassed by a manifest request that was already in flight.
	acls, err := getACLsForUser(ctx, tx, orgID, rootID, identity.UserID, role)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if !checkPermission(acls, file.Path, "write") {
			return nil, ErrCapturePathForbidden
		}
	}
	result := make([]RegisteredFileVersion, 0, len(files))
	newPacks := make(map[string]bool)
	var reuploadKeys []string
	for _, f := range files {
		if _, err = tx.Exec(ctx, `INSERT INTO file_catalog(id,root_id,path) VALUES($1,$2,$3) ON CONFLICT(root_id,path) DO NOTHING`, uuid.NewString(), rootID, f.Path); err != nil {
			return nil, err
		}
		var fileID string
		var head *string
		if err = tx.QueryRow(ctx, `SELECT id,captured_version_id FROM file_catalog WHERE root_id=$1 AND path=$2 FOR UPDATE`, rootID, f.Path).Scan(&fileID, &head); err != nil {
			return nil, err
		}
		versionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fileID+":"+captureID)).String()
		var sequence int64
		var hash, ref string
		var size int64
		var deleted bool
		var previous *string
		err = tx.QueryRow(ctx, `SELECT sequence,content_hash,size_bytes,source_manifest_ref,deleted,previous_version_id FROM file_versions WHERE id=$1`, versionID).Scan(&sequence, &hash, &size, &ref, &deleted, &previous)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if exists {
			if hash != f.ContentHash || size != f.Size || ref != f.SourceManifestRef || deleted != f.Deleted || pointerString(previous) != f.PreviousVersionID {
				return nil, fmt.Errorf("capture ID reused with different metadata for %s", f.Path)
			}
		} else {
			if pointerString(head) != f.PreviousVersionID {
				return nil, fmt.Errorf("%w: %s", ErrFileVersionConflict, f.Path)
			}
			// Validate after the root lock, before advancing any catalog head.
			// GC takes this lock too. An identical accepted capture above remains
			// replayable even when its historical source bytes have expired.
			if err = authorizeSourceExtents(ctx, tx, identity, rootID, captureID, f, newPacks); err != nil {
				var reupload *retiredSourcePacksError
				if errors.As(err, &reupload) {
					reuploadKeys = append(reuploadKeys, reupload.Keys...)
					continue
				}
				return nil, err
			}
			err = tx.QueryRow(ctx, `INSERT INTO file_versions(id,file_id,capture_id,previous_version_id,content_hash,size_bytes,source_manifest_ref,deleted)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING sequence`, versionID, fileID, captureID, head, f.ContentHash, f.Size, f.SourceManifestRef, f.Deleted).Scan(&sequence)
			if err != nil {
				return nil, err
			}
			for ordinal, extent := range f.Extents {
				if _, err = tx.Exec(ctx, `INSERT INTO file_version_extents(version_id,ordinal,object_key,byte_offset,byte_length) VALUES($1,$2,$3,$4,$5)`, versionID, ordinal, extent.ObjectKey, extent.Offset, extent.Length); err != nil {
					return nil, err
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE file_versions SET extents_indexed_at=NOW() WHERE id=$1`, versionID); err != nil {
				return nil, err
			}
			if _, err = tx.Exec(ctx, `UPDATE file_catalog SET captured_version_id=$1,deleted=$2,updated_at=NOW() WHERE id=$3`, versionID, f.Deleted, fileID); err != nil {
				return nil, err
			}
		}
		fileRevision := revision
		if exists {
			// A lost capture response may be replayed after a worker upgrade.
			// Keep its original extraction identity, inputs and paid provider work.
			if err = tx.QueryRow(ctx, `SELECT revision FROM file_extractions WHERE version_id=$1 ORDER BY sequence LIMIT 1`, versionID).Scan(&fileRevision); err != nil {
				return nil, err
			}
		}
		extractionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(versionID+":"+fileRevision)).String()
		status, stage := "pending", "transform"
		if f.Deleted {
			status, stage = "complete", "index"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO file_extractions(id,version_id,revision,status) VALUES($1,$2,$3,$4) ON CONFLICT(version_id,revision) DO NOTHING`, extractionID, versionID, fileRevision, status); err != nil {
			return nil, err
		}
		workID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(extractionID+":"+stage)).String()
		if _, err = tx.Exec(ctx, `INSERT INTO file_work(id,extraction_id,stage) VALUES($1,$2,$3) ON CONFLICT(extraction_id,stage) DO NOTHING`, workID, extractionID, stage); err != nil {
			return nil, err
		}
		result = append(result, RegisteredFileVersion{FileID: fileID, VersionID: versionID, Sequence: sequence, ExtractionID: extractionID, WorkID: workID, Stage: stage})
	}
	// Roll back the entire capture and report all expired packs together; the
	// client can re-upload one bounded batch instead of failing once per file.
	if len(reuploadKeys) > 0 {
		return nil, &retiredSourcePacksError{Keys: reuploadKeys}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// FileWorkDelivery contains only durable identities. Content is read from S3
// by the worker, never relayed in an SQS message or through the API server.
type FileWorkDelivery struct {
	ID           string `json:"work_id"`
	OrgID        string `json:"org_id"`
	RootID       string `json:"root_id"`
	FileID       string `json:"file_id"`
	VersionID    string `json:"version_id"`
	ExtractionID string `json:"extraction_id"`
	Stage        string `json:"stage"`
}

// UnpublishedFileWork is a bounded recovery scan for the catalog-to-SQS gap.
// It does not claim or execute jobs. Consumers must tolerate republishing.
func (db *DB) UnpublishedFileWork(ctx context.Context, limit int) ([]FileWorkDelivery, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("delivery limit must be 1..1000")
	}
	rows, err := db.pool.Query(ctx, `SELECT w.id,r.org_id,f.root_id,f.id,v.id,e.id,w.stage
		FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
		JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
		JOIN roots r ON r.id=f.root_id
		WHERE w.enqueued_at IS NULL AND w.status='pending' AND r.deleting_at IS NULL
		ORDER BY w.updated_at,w.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileWorkDelivery
	for rows.Next() {
		var d FileWorkDelivery
		if err = rows.Scan(&d.ID, &d.OrgID, &d.RootID, &d.FileID, &d.VersionID, &d.ExtractionID, &d.Stage); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkFileWorkEnqueued is called only after SQS accepts the message. A crash
// before this update causes harmless redelivery, rather than lost work.
func (db *DB) MarkFileWorkEnqueued(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx, `UPDATE file_work SET enqueued_at=COALESCE(enqueued_at,NOW()) WHERE id=$1`, id)
	return err
}

func validSHA256(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return strings.HasPrefix(value, "sha256:") && err == nil && len(decoded) == sha256.Size
}
