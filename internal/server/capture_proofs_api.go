package server

import (
	"encoding/json"
	"net/http"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Proof-only bootstrap does not register versions, upload bytes or enqueue work.
// This retains the existing client-reported hash contract, not a possession challenge.
func (s *Server) handleCapturedProofs(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.CapturedProofsRequest
	if !decodeCaptureRequest(w, r, &input) {
		return
	}
	if len(input.Files) < 1 || len(input.Files) > 128 {
		writeJSON(w, 400, map[string]string{"error": "files must contain 1..128 proofs"})
		return
	}
	rootID := r.PathValue("id")
	acls, err := s.db.GetACLsForUser(r.Context(), id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "proof permissions unavailable"})
		return
	}
	seen := make(map[string]bool, len(input.Files))
	for _, file := range input.Files {
		path, err := cleanFilePath(file.Path)
		if err != nil || path != file.Path || seen[path] || file.VersionID == "" || file.ContentHash == "" {
			writeJSON(w, 400, map[string]string{"error": "invalid or duplicate file proof"})
			return
		}
		if len(acls) != 0 && !checkPermission(acls, path, "write") {
			writeJSON(w, 403, map[string]string{"error": "proof path is not writable"})
			return
		}
		seen[path] = true
	}
	data, _ := json.Marshal(input.Files)
	tx, err := s.db.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "proof persistence failed"})
		return
	}
	defer tx.Rollback(r.Context())
	var root string
	err = tx.QueryRow(r.Context(), `SELECT id FROM roots WHERE id=$1 AND org_id=$2 AND deleting_at IS NULL FOR UPDATE`, rootID, id.OrgID).Scan(&root)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "root unavailable"})
		return
	}
	// The root lock serializes this validation with per-file capture/deletion.
	// Reject the complete batch on any stale/mismatching member: no partial proof.
	var matched int
	err = tx.QueryRow(r.Context(), `SELECT count(*) FROM jsonb_to_recordset($1::jsonb) AS p(path text,version_id text,content_hash text)
		JOIN file_catalog f ON f.root_id=$2 AND f.path=p.path AND NOT f.deleted
		JOIN file_versions v ON v.id=f.captured_version_id AND v.id=p.version_id AND v.content_hash=p.content_hash AND NOT v.deleted`, data, rootID).Scan(&matched)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "proof validation failed"})
		return
	}
	if matched != len(input.Files) {
		writeJSON(w, 409, map[string]string{"error": "proof does not match current captured versions; refresh catalog"})
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO file_content_proofs(org_id,user_id,root_id,path,sequence,content_hash,deleted)
		SELECT $1,$2,$3,p.path,v.sequence,v.content_hash,FALSE
		FROM jsonb_to_recordset($4::jsonb) AS p(path text,version_id text,content_hash text)
		JOIN file_versions v ON v.id=p.version_id
		ON CONFLICT(org_id,user_id,root_id,path) DO UPDATE SET sequence=EXCLUDED.sequence,content_hash=EXCLUDED.content_hash,deleted=FALSE
		WHERE file_content_proofs.sequence<=EXCLUDED.sequence`, id.OrgID, id.UserID, rootID, data)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "proof persistence failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "complete", "files": matched})
}
