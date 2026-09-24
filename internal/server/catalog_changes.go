package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pufferfs/pufferfs/pkg/models"
)

type catalogCursor struct {
	Root     string `json:"root"`
	Org      string `json:"org"`
	User     string `json:"user"`
	Access   string `json:"access"`
	Revision int64  `json:"revision"`
}

func (s *Server) encodeCatalogCursor(cursor catalogCursor) string {
	raw, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, s.jwtSecret)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) decodeCatalogCursor(value string) (catalogCursor, error) {
	var cursor catalogCursor
	parts := strings.Split(value, ".")
	if len(value) > 2048 || len(parts) != 2 {
		return cursor, errors.New("invalid catalog cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return cursor, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return cursor, err
	}
	mac := hmac.New(sha256.New, s.jwtSecret)
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return cursor, errors.New("invalid catalog cursor")
	}
	err = json.Unmarshal(raw, &cursor)
	return cursor, err
}

// Serialize only the assignment of committed events to revisions. Capture and
// publication never wait on this row. A late committing event gets a later
// revision on the next call, regardless of its original outbox sequence.
func (db *DB) advanceCatalogChanges(ctx context.Context, rootID string) (int64, bool, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO catalog_change_state(root_id) VALUES($1) ON CONFLICT DO NOTHING`, rootID); err != nil {
		return 0, false, err
	}
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM catalog_change_state WHERE root_id=$1 FOR UPDATE`, rootID).Scan(&revision); err != nil {
		return 0, false, err
	}
	rows, err := tx.Query(ctx, `WITH consumed AS (
		DELETE FROM catalog_change_outbox WHERE id IN (
			SELECT id FROM catalog_change_outbox WHERE root_id=$1 ORDER BY id LIMIT 2000
		) RETURNING file_id
	) SELECT DISTINCT file_id FROM consumed ORDER BY file_id`, rootID)
	if err != nil {
		return 0, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, false, err
	}
	if len(ids) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO catalog_file_changes(root_id,file_id,revision)
			SELECT $1,file_id,$3::bigint+ordinal FROM unnest($2::text[]) WITH ORDINALITY AS changed(file_id,ordinal)
			ON CONFLICT(file_id) DO UPDATE SET revision=EXCLUDED.revision`, rootID, ids, revision)
		if err != nil {
			return 0, false, err
		}
		revision += int64(len(ids))
		if _, err = tx.Exec(ctx, `UPDATE catalog_change_state SET revision=$2 WHERE root_id=$1`, rootID, revision); err != nil {
			return 0, false, err
		}
	}
	var more bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_change_outbox WHERE root_id=$1)`, rootID).Scan(&more); err != nil {
		return 0, false, err
	}
	return revision, more, tx.Commit(ctx)
}

func (s *Server) handleCatalogChanges(w http.ResponseWriter, r *http.Request) {
	id := s.captureIdentity(w, r)
	if id == nil {
		return
	}
	rootID := r.PathValue("id")
	limit := 500
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 1000 {
			writeJSON(w, 400, map[string]string{"error": "limit must be 1..1000"})
			return
		}
	}
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
	slices.Sort(denied)
	accessJSON, _ := json.Marshal(struct {
		Role   string
		Denied []string
	}{string(id.Role), slices.Compact(denied)})
	digest := sha256.Sum256(accessJSON)
	cursor := catalogCursor{Root: rootID, Org: id.OrgID, User: id.UserID, Access: hex.EncodeToString(digest[:])}
	reset := func() {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "catalog_cursor_reset", "error": "catalog access changed; refresh local catalog metadata"})
	}
	if value := r.URL.Query().Get("cursor"); value != "" {
		previous, err := s.decodeCatalogCursor(value)
		if err != nil || previous.Root != cursor.Root || previous.Org != cursor.Org || previous.User != cursor.User || previous.Access != cursor.Access || previous.Revision < 0 {
			reset()
			return
		}
		cursor.Revision = previous.Revision
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	revision, pending, err := s.db.advanceCatalogChanges(ctx, rootID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "catalog changes unavailable"})
		return
	}
	if cursor.Revision > revision {
		reset()
		return
	}
	rows, err := s.db.pool.Query(ctx, `SELECT c.revision,f.id,f.path,v.id,v.sequence,
		COALESCE(f.indexed_version_id,''),v.content_hash,v.size_bytes,v.deleted,v.source_manifest_ref,
		COALESCE(p.sequence=v.sequence AND p.content_hash=v.content_hash AND p.deleted=v.deleted,FALSE)
		FROM catalog_file_changes c JOIN file_catalog f ON f.id=c.file_id
		JOIN file_versions v ON v.id=f.captured_version_id JOIN roots r ON r.id=f.root_id
		LEFT JOIN file_content_proofs p ON p.org_id=r.org_id AND p.root_id=f.root_id AND p.path=f.path AND p.user_id=$5
		WHERE c.root_id=$1 AND r.org_id=$2 AND r.deleting_at IS NULL AND c.revision>$3 AND c.revision<=$6
		ORDER BY c.revision LIMIT $4`, rootID, id.OrgID, cursor.Revision, limit+1, id.UserID, revision)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "catalog changes unavailable"})
		return
	}
	defer rows.Close()
	result := models.CatalogChangesResponse{Files: []models.CapturedFileHead{}, More: pending}
	count := 0
	pageRemaining := false
	for rows.Next() {
		var file models.CapturedFileHead
		var position int64
		if err = rows.Scan(&position, &file.FileID, &file.Path, &file.VersionID, &file.Sequence, &file.IndexedVersionID,
			&file.ContentHash, &file.Size, &file.Deleted, &file.SourceManifestRef, &file.ProofCurrent); err != nil {
			break
		}
		if count == limit {
			pageRemaining = true
			result.More = true
			break
		}
		count++
		cursor.Revision = position
		if checkPermission(acls, file.Path, "write") {
			result.Files = append(result.Files, file)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "catalog changes decoding failed"})
		return
	}
	if !pageRemaining {
		cursor.Revision = revision
	}
	result.Cursor = s.encodeCatalogCursor(cursor)
	writeJSON(w, 200, result)
}
