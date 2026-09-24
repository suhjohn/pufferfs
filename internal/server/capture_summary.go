package server

import (
	"encoding/json"
	"net/http"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Summarize one database snapshot rather than transferring every catalog row.
// Explicit selections are joined by the existing root/path index, so waiting on
// a small local subset does not enumerate unrelated remote files. ACL-denied or
// missing requested paths are reported only as the supplied path, uncaptured.
func (s *Server) handleCaptureSummary(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	var input models.CaptureStatusRequest
	selected := r.Method == http.MethodPost
	if selected {
		if !decodeCaptureRequest(w, r, &input) {
			return
		}
		if len(input.Files) > 1000 {
			writeJSON(w, 400, map[string]string{"error": "status selection must contain at most 1000 files"})
			return
		}
		seen := make(map[string]bool, len(input.Files))
		for _, file := range input.Files {
			path, err := cleanFilePath(file.Path)
			if err != nil || path != file.Path || seen[path] || file.Size < 0 || !validSHA256(file.ContentHash) {
				writeJSON(w, 400, map[string]string{"error": "invalid status selection"})
				return
			}
			seen[path] = true
		}
	}
	rootID := r.PathValue("id")
	acls, err := s.db.GetACLsForUser(r.Context(), id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "catalog permissions unavailable"})
		return
	}
	denied := []string{}
	for _, acl := range acls {
		if acl.Permission == "none" {
			denied = append(denied, acl.PathPrefix)
		}
	}
	if input.Files == nil {
		input.Files = []models.CaptureStatusSelection{}
	}
	// Select paths before loading versions/work. The default CTE inlining lets
	// Postgres use root/path lookups for a subset and one root scan otherwise.
	var raw []byte
	err = s.db.pool.QueryRow(r.Context(), `WITH requested AS (
		SELECT * FROM jsonb_to_recordset($4) AS q(path text,content_hash text,size bigint)
	), visible AS NOT MATERIALIZED (
		SELECT f.* FROM file_catalog f JOIN roots r ON r.id=f.root_id
		WHERE f.root_id=$1 AND r.org_id=$2 AND r.deleting_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM unnest($3::text[]) denied(prefix) WHERE starts_with('/'||f.path,denied.prefix))
	), files AS (
		SELECT f.id,f.path,f.captured_version_id,f.indexed_extraction_id,
			NULL::text AS expected_hash,NULL::bigint AS expected_size
		FROM visible f WHERE NOT $5
		UNION ALL
		SELECT f.id,q.path,f.captured_version_id,f.indexed_extraction_id,q.content_hash,q.size
		FROM requested q LEFT JOIN visible f ON f.path=q.path WHERE $5
	), statuses AS MATERIALIZED (
		SELECT f.path,COALESCE(v.id,'') AS version_id,
			CASE WHEN $5 AND (v.id IS NULL OR v.deleted OR v.content_hash<>f.expected_hash OR v.size_bytes<>f.expected_size) THEN 'uncaptured'
				WHEN e.id IS NULL THEN 'missing'
				WHEN f.indexed_extraction_id=e.id THEN 'complete'
				WHEN e.status IN ('failed','superseded','waiting_provider') THEN e.status
				WHEN w.status='complete' THEN 'inconsistent'
				ELSE COALESCE(w.status,'pending') END AS status,
			CASE WHEN e.id IS NULL THEN NULL ELSE jsonb_build_object('extraction_id',e.id,'revision',e.revision,
				'stage',CASE WHEN e.status='complete' THEN 'index' ELSE 'transform' END,'attempt_count',COALESCE(w.attempt_count,0)) END AS processing
		FROM files f LEFT JOIN file_versions v ON v.id=f.captured_version_id
		LEFT JOIN LATERAL (SELECT id,revision,status FROM file_extractions WHERE version_id=v.id ORDER BY sequence DESC LIMIT 1) e ON TRUE
		LEFT JOIN file_work w ON w.extraction_id=e.id AND w.stage=CASE WHEN e.status='complete' THEN 'index' ELSE 'transform' END
	)
	SELECT jsonb_build_object('root_id',$1::text,'total',(SELECT count(*) FROM statuses),
		'states',COALESCE((SELECT jsonb_object_agg(status,n) FROM (SELECT status,count(*) AS n FROM statuses GROUP BY status) counts),'{}'::jsonb),
		'examples',COALESCE((SELECT jsonb_agg(jsonb_build_object('path',path,'version_id',version_id,'status',status,
			'processing',processing||jsonb_build_object('status',status)))
			FROM (SELECT * FROM statuses WHERE status<>'complete' ORDER BY path LIMIT 20) examples),'[]'::jsonb))`,
		rootID, id.OrgID, denied, input.Files, selected).Scan(&raw)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "capture summary unavailable"})
		return
	}
	var result models.CaptureStatusResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		writeJSON(w, 500, map[string]string{"error": "capture summary decoding failed"})
		return
	}
	result.Status = captureSummaryStatus(result.Total, result.States)
	writeJSON(w, 200, result)
}

func captureSummaryStatus(total int, states map[string]int) string {
	if total == 0 {
		return "empty"
	}
	if states["complete"] == total {
		return "complete"
	}
	if states["failed"]+states["superseded"]+states["missing"]+states["inconsistent"] > 0 {
		return "failed"
	}
	return "processing"
}
