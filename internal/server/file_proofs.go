package server

import (
	"context"
	"maps"
	"slices"

	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func (db *DB) RecordCapturedProofs(ctx context.Context, orgID, userID, rootID string, versions []RegisteredFileVersion) error {
	ids := make([]string, len(versions))
	for i, version := range versions {
		ids[i] = version.VersionID
	}
	_, err := db.pool.Exec(ctx, `INSERT INTO file_content_proofs(org_id,user_id,root_id,path,sequence,content_hash,deleted)
		SELECT r.org_id,$2,f.root_id,f.path,v.sequence,v.content_hash,v.deleted
		FROM file_versions v JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
		WHERE r.org_id=$1 AND f.root_id=$3 AND v.id=ANY($4) AND r.deleting_at IS NULL
		ON CONFLICT(org_id,user_id,root_id,path) DO UPDATE
		SET sequence=EXCLUDED.sequence,content_hash=EXCLUDED.content_hash,deleted=EXCLUDED.deleted
		WHERE file_content_proofs.sequence<EXCLUDED.sequence`, orgID, userID, rootID, ids)
	return err
}

// Reads and searches share the same post-provider access check. A search passes
// all populated roots together; a file read passes one root and one row set.
func (s *Server) filterRowsAccess(ctx context.Context, id *auth.Identity, root *models.RootMetadata, rows []map[string]any) ([]map[string]any, error) {
	sets := [][]map[string]any{rows}
	err := s.filterSearchRowsAccess(ctx, id, []models.RootMetadata{*root}, sets)
	return sets[0], err
}

func (s *Server) filterSearchRowsAccess(ctx context.Context, id *auth.Identity, roots []models.RootMetadata, sets [][]map[string]any) error {
	var lookups []rootPathLookup
	needsProof := make([]bool, len(roots))
	for i, root := range roots {
		if len(sets[i]) == 0 {
			continue
		}
		needsProof[i] = root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin)
		paths := make(map[string]bool)
		if needsProof[i] {
			for _, row := range sets[i] {
				paths[strVal(row, "file_path")] = true
			}
		}
		lookups = append(lookups, rootPathLookup{Slot: i, RootID: root.ID, Paths: slices.Sorted(maps.Keys(paths))})
	}
	if len(lookups) == 0 {
		return nil
	}
	access, err := s.db.pool.Query(ctx, `WITH requested AS (
        SELECT * FROM jsonb_to_recordset($3) AS q(slot int,root_id text,paths text[]))
        SELECT q.slot,TRUE,a.path_prefix,'' FROM root_acls a JOIN requested q ON q.root_id=a.root_id
        WHERE a.org_id=$1 AND a.permission='none' AND a.grant_to=ANY($4)
        UNION ALL SELECT q.slot,FALSE,p.path,p.content_hash FROM file_content_proofs p JOIN requested q ON q.root_id=p.root_id
        WHERE p.org_id=$1 AND p.user_id=$2 AND NOT p.deleted AND p.path=ANY(q.paths)`,
		id.OrgID, id.UserID, lookups, []string{id.UserID, "user:" + id.UserID, "role:" + string(id.Role), "*"})
	if err != nil {
		return errFilePermissionsUnavailable
	}
	defer access.Close()
	denied := make([][]string, len(roots))
	proofs := make([]map[string]string, len(roots))
	for access.Next() {
		var slot int
		var deny bool
		var path, hash string
		if err := access.Scan(&slot, &deny, &path, &hash); err != nil {
			return errFilePermissionsUnavailable
		}
		if deny {
			denied[slot] = append(denied[slot], path)
		} else {
			if proofs[slot] == nil {
				proofs[slot] = make(map[string]string)
			}
			proofs[slot][path] = hash
		}
	}
	if access.Err() != nil {
		return errFilePermissionsUnavailable
	}
	for i, rows := range sets {
		rows = filterDeniedQueryRows(rows, denied[i])
		if needsProof[i] {
			filtered := rows[:0]
			for _, row := range rows {
				hash := strVal(row, "file_hash")
				if hash != "" && proofs[i][strVal(row, "file_path")] == hash {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
		sets[i] = rows
	}
	return nil
}
