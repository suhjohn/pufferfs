package server

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Load routing once for the selected roots. An indexed existence probe skips
// roots with no captured files; pending/deleted catalogs still use bounded
// candidate-publication validation instead of a root-wide publication scan.
func (db *DB) queryNamespaces(ctx context.Context, orgID string, roots []models.RootMetadata) (map[string][]models.RootIndexNamespace, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ids := make([]string, len(roots))
	for i, root := range roots {
		ids[i] = root.ID
	}
	rows, err := db.pool.Query(ctx, `SELECT r.id,CASE WHEN EXISTS (
		SELECT 1 FROM file_catalog f WHERE f.root_id=r.id) THEN (
		SELECT jsonb_agg(to_jsonb(n) ORDER BY n.shard_index) FROM root_index_namespaces n
		WHERE n.org_id=r.org_id AND n.root_id=r.id AND n.retired_at IS NULL) END
		FROM roots r WHERE r.org_id=$1 AND r.id=ANY($2) AND r.deleting_at IS NULL`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]models.RootIndexNamespace, len(roots))
	for rows.Next() {
		var id string
		var namespaces []models.RootIndexNamespace
		if err := rows.Scan(&id, &namespaces); err != nil {
			return nil, err
		}
		result[id] = namespaces
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(roots) {
		return nil, errQueryRootNotFound
	}
	return result, nil
}

// Merge already validated namespace ranks. Vector distances are comparable;
// FTS/hybrid preserve their existing reciprocal-rank fusion across shards.
func mergeNamespaceRows(resultSets [][]map[string]any, mode string, limit int) []map[string]any {
	var rows []map[string]any
	switch {
	case len(resultSets) == 0:
		return nil
	case len(resultSets) == 1:
		rows = resultSets[0]
	case mode == "vector":
		rows = slices.Concat(resultSets...)
		sort.SliceStable(rows, func(i, j int) bool { return floatVal(rows[i], "$dist") < floatVal(rows[j], "$dist") })
	default:
		rows = reciprocalRankFusion(resultSets, 60)
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}
