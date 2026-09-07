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
	"github.com/pufferfs/pufferfs/internal/queue"
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
	Extents           []models.SourceExtent `json:"extents,omitempty"`
}

type RegisteredFileVersion = models.RegisteredFileVersion

type registeredCapture struct {
	versions   []RegisteredFileVersion
	deliveries []queue.JobMessage
}

var ErrFileVersionConflict = errors.New("captured file version changed")
var ErrCapturePathForbidden = errors.New("capture path is not writable")

type retiredSourcePacksError struct{ Keys []string }

func (e *retiredSourcePacksError) Error() string {
	return "source packs retired or lack upload provenance; re-upload the retained captured bytes"
}

var errSourceExtentUnavailable = errors.New("source extent is unavailable, outside its object, or not authorized for this file")

// RegisterFileVersions atomically advances captured heads and records the SQS
// handoffs that must be published. It never waits for or advances indexing.
// Repeating an identical capture is idempotent even after a later capture.
func (db *DB) RegisterFileVersions(ctx context.Context, identity *auth.Identity, rootID, captureID, revision string, files []CapturedFileVersion) (*registeredCapture, error) {
	if identity == nil || identity.OrgID == "" || identity.UserID == "" || captureID == "" || revision == "" || len(files) == 0 || len(files) > 128 {
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
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	if _, err = tx.Exec(ctx, `INSERT INTO file_catalog(id,root_id,path)
		SELECT gen_random_uuid()::text,$1,path FROM unnest($2::text[]) AS path
		ON CONFLICT(root_id,path) DO NOTHING`, rootID, paths); err != nil {
		return nil, err
	}
	// Read after the root lock and catalog insert. Replays use the original
	// extraction revision, even when a newer capture is already the head.
	rows, err := tx.Query(ctx, `SELECT f.id,f.path,COALESCE(f.captured_version_id,''),
		COALESCE(v.id,''),COALESCE(v.sequence,0),COALESCE(v.content_hash,''),
		COALESCE(v.size_bytes,0),COALESCE(v.source_manifest_ref,''),COALESCE(v.deleted,false),
		COALESCE(v.previous_version_id,''),COALESCE(first.revision,''),COALESCE(first.needs_delivery,false)
		FROM file_catalog f LEFT JOIN file_versions v ON v.file_id=f.id AND v.capture_id=$3
		LEFT JOIN LATERAL (SELECT e.revision,w.status='pending' AND w.enqueued_at IS NULL AS needs_delivery
			FROM file_extractions e LEFT JOIN file_work w ON w.extraction_id=e.id
				AND w.stage=CASE WHEN v.deleted THEN 'index' ELSE 'transform' END
			WHERE e.version_id=v.id ORDER BY e.sequence LIMIT 1) first ON true
		WHERE f.root_id=$1 AND f.path=ANY($2::text[]) ORDER BY f.path FOR UPDATE OF f`, rootID, paths, captureID)
	if err != nil {
		return nil, err
	}
	type capturedHead struct {
		fileID, head, versionID, revision string
		sequence                          int64
		needsDelivery                     bool
		file                              CapturedFileVersion
	}
	heads := make(map[string]capturedHead, len(files))
	for rows.Next() {
		var h capturedHead
		if err = rows.Scan(&h.fileID, &h.file.Path, &h.head, &h.versionID, &h.sequence,
			&h.file.ContentHash, &h.file.Size, &h.file.SourceManifestRef, &h.file.Deleted,
			&h.file.PreviousVersionID, &h.revision, &h.needsDelivery); err != nil {
			break
		}
		heads[h.file.Path] = h
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := &registeredCapture{versions: make([]RegisteredFileVersion, len(files))}
	var writes []captureWrite
	for i, f := range files {
		h, ok := heads[f.Path]
		if !ok {
			return nil, pgx.ErrNoRows
		}
		fileRevision := revision
		versionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(h.fileID+":"+captureID)).String()
		if h.versionID != "" {
			storedIdentity, err := sourceManifestIdentity(h.file.SourceManifestRef)
			if err != nil {
				return nil, err
			}
			replayIdentity, err := sourceManifestIdentity(f.SourceManifestRef)
			if err != nil {
				return nil, err
			}
			if h.file.ContentHash != f.ContentHash || h.file.Size != f.Size || storedIdentity != replayIdentity || h.file.Deleted != f.Deleted || h.file.PreviousVersionID != f.PreviousVersionID {
				return nil, fmt.Errorf("capture ID reused with different metadata for %s", f.Path)
			}
			if h.revision == "" {
				return nil, pgx.ErrNoRows
			}
			versionID, fileRevision = h.versionID, h.revision
		} else if h.head != f.PreviousVersionID {
			return nil, fmt.Errorf("%w: %s", ErrFileVersionConflict, f.Path)
		}
		extractionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(versionID+":"+fileRevision)).String()
		stage := "transform"
		if f.Deleted {
			stage = "index"
		}
		workID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(extractionID+":"+stage)).String()
		result.versions[i] = RegisteredFileVersion{FileID: h.fileID, VersionID: versionID, Sequence: h.sequence,
			ExtractionID: extractionID, WorkID: workID, Stage: stage}
		if h.versionID == "" || h.needsDelivery {
			result.deliveries = append(result.deliveries, queue.JobMessage{JobID: workID, WorkID: workID,
				OrgID: orgID, RootID: rootID, FileID: h.fileID, VersionID: versionID, ExtractionID: extractionID, Stage: stage})
		}
		if h.versionID == "" {
			writes = append(writes, captureWrite{CapturedFileVersion: f, RegisteredFileVersion: result.versions[i], Revision: fileRevision, Slot: i})
		}
	}
	if len(writes) > 0 {
		// Validate the entire batch before binding source packs or advancing any
		// head. Identical historical retries do not need retained source bytes.
		if err = authorizeCaptureSources(ctx, tx, identity, rootID, captureID, writes); err != nil {
			return nil, err
		}
		if err = writeCapturedVersions(ctx, tx, captureID, writes, result.versions); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Only new versions enter this bounded write batch; replay receipts are already
// complete. Input order is returned through Slot, not database execution order.
type captureWrite struct {
	CapturedFileVersion
	RegisteredFileVersion
	Revision string `json:"revision"`
	Slot     int    `json:"slot"`
}

func writeCapturedVersions(ctx context.Context, tx pgx.Tx, captureID string, writes []captureWrite, result []RegisteredFileVersion) error {
	rows, err := tx.Query(ctx, `WITH input AS MATERIALIZED (
		SELECT * FROM jsonb_to_recordset($2) AS i(slot int,file_id text,version_id text,
			previous_version_id text,content_hash text,size bigint,source_manifest_ref text,
			deleted boolean,extents jsonb,extraction_id text,revision text,work_id text,stage text)
	), versions AS (
		INSERT INTO file_versions(id,file_id,capture_id,previous_version_id,content_hash,size_bytes,
			source_manifest_ref,deleted)
		SELECT version_id,file_id,$1,NULLIF(previous_version_id,''),content_hash,size,
			source_manifest_ref,deleted FROM input RETURNING id,sequence
	), extents AS (
		INSERT INTO file_version_extents(version_id,ordinal,object_key,byte_offset,byte_length)
		SELECT v.id,e.ordinal-1,e.value->>'object_key',(e.value->>'offset')::bigint,(e.value->>'length')::bigint
		FROM versions v JOIN input i ON i.version_id=v.id
		CROSS JOIN LATERAL jsonb_array_elements(COALESCE(i.extents,'[]'::jsonb)) WITH ORDINALITY e(value,ordinal)
	), heads AS (
		UPDATE file_catalog f SET captured_version_id=v.id,deleted=i.deleted,updated_at=NOW()
		FROM versions v JOIN input i ON i.version_id=v.id WHERE f.id=i.file_id
	), extractions AS (
		INSERT INTO file_extractions(id,version_id,revision,status)
		SELECT i.extraction_id,v.id,i.revision,CASE WHEN i.deleted THEN 'complete' ELSE 'pending' END
		FROM versions v JOIN input i ON i.version_id=v.id RETURNING id
	), work AS (
		INSERT INTO file_work(id,extraction_id,stage)
		SELECT i.work_id,e.id,i.stage FROM extractions e JOIN input i ON i.extraction_id=e.id
	) SELECT i.slot,v.sequence FROM versions v JOIN input i ON i.version_id=v.id`, captureID, writes)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slot int
		var sequence int64
		if err = rows.Scan(&slot, &sequence); err != nil {
			return err
		}
		result[slot].Sequence = sequence
	}
	return rows.Err()
}

// MarkFileWorkEnqueued records only confirmed SQS sends. A crash
// before this update causes harmless redelivery, rather than lost work.
func (db *DB) MarkFileWorkEnqueued(ctx context.Context, ids []string) error {
	_, err := db.pool.Exec(ctx, `UPDATE file_work SET enqueued_at=NOW() WHERE id=ANY($1::text[]) AND enqueued_at IS NULL`, ids)
	return err
}

func validSHA256(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return strings.HasPrefix(value, "sha256:") && err == nil && len(decoded) == sha256.Size
}
