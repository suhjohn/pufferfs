package server

import (
	"net/http"
	"strconv"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Bootstrap/reconciliation metadata for a sync-authorized capture agent.
// Pages are live reads, not a root snapshot. Registration still uses each
// file's previous_version_id to detect edits concurrent with enumeration.
func (s *Server) handleListCapturedFiles(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	processing := r.URL.Query().Get("processing")
	if processing != "" && processing != "true" && processing != "false" {
		writeJSON(w, 400, map[string]string{"error": "processing must be true or false"})
		return
	}
	limit := 500
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 1000 {
			writeJSON(w, 400, map[string]string{"error": "limit must be 1..1000"})
			return
		}
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 128 {
		writeJSON(w, 400, map[string]string{"error": "invalid cursor"})
		return
	}
	rootID := r.PathValue("id")
	// Load path ACLs once per page, never once per file. Fail closed on errors.
	acls, err := s.db.GetACLsForUser(r.Context(), id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "catalog permissions unavailable"})
		return
	}
	columns, joins := "", ""
	if processing == "true" {
		columns = `,COALESCE(e.id,''),COALESCE(e.revision,''),
			CASE WHEN e.status='complete' THEN 'index' ELSE 'transform' END,
			CASE WHEN e.id IS NULL THEN 'missing'
				WHEN f.indexed_extraction_id=e.id THEN 'complete'
				WHEN e.status IN ('failed','superseded','waiting_provider') THEN e.status
				WHEN w.status='complete' THEN 'inconsistent'
				ELSE COALESCE(w.status,'pending') END,
			COALESCE(w.attempt_count,0),COALESCE(w.acknowledged_batches,0),w.mutation_batch_count`
		joins = ` LEFT JOIN LATERAL (SELECT id,revision,status FROM file_extractions
			WHERE version_id=v.id ORDER BY sequence DESC LIMIT 1) e ON TRUE
			LEFT JOIN file_work w ON w.extraction_id=e.id
				AND w.stage=CASE WHEN e.status='complete' THEN 'index' ELSE 'transform' END `
	}
	rows, err := s.db.pool.Query(r.Context(), `SELECT f.id,f.path,v.id,v.sequence,
		COALESCE(f.indexed_version_id,''),v.content_hash,v.size_bytes,v.deleted,v.source_manifest_ref,
		COALESCE(p.sequence=v.sequence AND p.content_hash=v.content_hash AND p.deleted=v.deleted,FALSE)`+columns+`
		FROM file_catalog f JOIN file_versions v ON v.id=f.captured_version_id
		JOIN roots r ON r.id=f.root_id
		LEFT JOIN file_content_proofs p ON p.org_id=r.org_id AND p.root_id=f.root_id AND p.path=f.path AND p.user_id=$5 `+joins+`
		WHERE f.root_id=$1 AND r.org_id=$2 AND r.deleting_at IS NULL AND f.id>$3
		ORDER BY f.id LIMIT $4`, rootID, id.OrgID, cursor, limit+1, id.UserID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "captured catalog unavailable"})
		return
	}
	defer rows.Close()
	result := models.CapturedFilesResponse{Files: []models.CapturedFileHead{}}
	count, last := 0, ""
	for rows.Next() {
		var file models.CapturedFileHead
		fields := []any{&file.FileID, &file.Path, &file.VersionID, &file.Sequence,
			&file.IndexedVersionID, &file.ContentHash, &file.Size, &file.Deleted, &file.SourceManifestRef, &file.ProofCurrent}
		if processing == "true" {
			p := &models.FileProcessingStatus{}
			file.Processing = p
			fields = append(fields, &p.ExtractionID, &p.Revision, &p.Stage, &p.Status,
				&p.AttemptCount, &p.AcknowledgedBatches, &p.MutationBatchCount)
		}
		if err = rows.Scan(fields...); err != nil {
			writeJSON(w, 500, map[string]string{"error": "captured catalog decoding failed"})
			return
		}
		if count == limit {
			result.NextCursor = last
			break
		}
		count++
		last = file.FileID
		if len(acls) == 0 || checkPermission(acls, file.Path, "write") {
			result.Files = append(result.Files, file)
		}
	}
	if err = rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "captured catalog read failed"})
		return
	}
	// An ACL-filtered page can be empty and still have a continuation cursor.
	writeJSON(w, 200, result)
}
