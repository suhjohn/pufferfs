package server

import (
	"context"
	"fmt"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Read a single file in chunk order using one visibility snapshot. A page or
// line range can span arbitrarily many chunks; requested page count is not a
// query limit. Oversized responses fail explicitly rather than losing text.
func (s *Server) readFileRows(ctx context.Context, root *models.RootMetadata, path string, filters any) ([]map[string]any, error) {
	namespaces, err := s.db.ListRootIndexNamespaces(ctx, root.OrgID, root.ID)
	if err != nil {
		return nil, err
	}
	if len(activeRootIndexNamespaces(namespaces)) == 0 {
		return nil, nil
	}
	ns, err := rootIndexNamespaceForPath(namespaces, path)
	if err != nil {
		return nil, err
	}
	visibility, err := s.catalogVisibilitySnapshot(ctx, root.OrgID, root.ID, path)
	if err != nil {
		return nil, err
	}
	return collectFileRows(ctx, func(after int) ([]map[string]any, error) {
		parts := []any{filters, visibility, []any{"file_path", "Eq", path}}
		if after >= 0 {
			parts = append(parts, []any{"chunk_index", "Gt", after})
		}
		return s.tp.Query(ctx, ns.Namespace, TPQuery{RankBy: []any{"chunk_index", "asc"}, Limit: 512, Filters: tpAndFilter(parts), ExcludeAttributes: readExcludedAttrs()})
	})
}

func collectFileRows(ctx context.Context, query func(int) ([]map[string]any, error)) ([]map[string]any, error) {
	var result []map[string]any
	after, bytes := -1, 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := query(after)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			index := intFromAny(row["chunk_index"], -1)
			if index <= after {
				return nil, fmt.Errorf("file chunk pagination did not advance")
			}
			after = index
			bytes += len(strVal(row, "content"))
			if bytes > 32*1024*1024 {
				return nil, fmt.Errorf("read exceeds 32 MiB; request a smaller range")
			}
			result = append(result, row)
		}
		if len(rows) < 512 {
			return result, nil
		}
	}
}
