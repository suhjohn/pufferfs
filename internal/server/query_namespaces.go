package server

import (
	"context"
	"time"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Load routing once for the selected roots. An indexed existence probe skips
// roots with no captured files; pending/deleted catalogs still use bounded
// candidate-publication validation instead of a root-wide publication scan.
func (db *DB) queryNamespaces(ctx context.Context, orgID string, roots []models.RootMetadata) (map[string]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ids := make([]string, len(roots))
	for i, root := range roots {
		ids[i] = root.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT r.id,COALESCE(CASE WHEN EXISTS (
		SELECT 1 FROM file_catalog f WHERE f.root_id=r.id) THEN (
		SELECT n.namespace FROM root_index_namespaces n
		WHERE n.org_id=r.org_id AND n.root_id=r.id AND n.retired_at IS NULL) END,'')
		FROM roots r WHERE r.org_id=$1 AND r.id=ANY($2) AND r.deleting_at IS NULL`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string, len(roots))
	for rows.Next() {
		var id string
		var namespace string
		if err := rows.Scan(&id, &namespace); err != nil {
			return nil, err
		}
		result[id] = namespace
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(roots) {
		return nil, errQueryRootNotFound
	}
	return result, nil
}
