package server

import (
	"context"
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

// Preserve the existing client-reported path/hash check. This does not introduce
// a cryptographic proof-of-possession challenge or bypass root/path access checks.
func (s *Server) filterByContentProof(ctx context.Context, orgID, userID, rootID string, rows []map[string]any) []map[string]any {
	if len(rows) == 0 {
		return nil
	}
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, strVal(row, "file_path"))
	}
	proofRows, err := s.db.pool.Query(ctx, `SELECT path,content_hash,deleted FROM file_content_proofs
		WHERE org_id=$1 AND user_id=$2 AND root_id=$3 AND path=ANY($4)`, orgID, userID, rootID, paths)
	if err != nil {
		return nil
	}
	type fileProof struct {
		hash    string
		deleted bool
	}
	proofs := make(map[string]fileProof)
	for proofRows.Next() {
		var path string
		var proof fileProof
		if err = proofRows.Scan(&path, &proof.hash, &proof.deleted); err != nil {
			break
		}
		proofs[path] = proof
	}
	if err == nil {
		err = proofRows.Err()
	}
	proofRows.Close()
	if err != nil {
		return nil
	}
	var filtered []map[string]any
	for _, row := range rows {
		path, hash := strVal(row, "file_path"), strVal(row, "file_hash")
		if path == "" || hash == "" {
			continue
		}
		if proof, exists := proofs[path]; exists {
			if !proof.deleted && proof.hash == hash {
				filtered = append(filtered, row)
			}
		}
	}
	return filtered
}
