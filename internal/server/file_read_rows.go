package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// Pin routing and publication once, including a metadata fallback after an
// empty range. Reads never allocate namespace state or hold a DB connection
// during provider IO. Root creation initializes namespace routing.
type fileReadSnapshot struct {
	namespace  string
	path       string
	extraction string
	fileID     string
	fileHash   string
	rowFormat  int
	segments   []string
}

func (s *Server) loadFileReadSnapshot(ctx context.Context, root *models.RootMetadata, path string) (fileReadSnapshot, error) {
	snapshot := fileReadSnapshot{path: path}
	err := s.db.pool.QueryRow(ctx, `SELECT f.indexed_extraction_id,n.namespace,f.id,v.content_hash,e.row_format
        FROM roots r JOIN file_catalog f ON f.root_id=r.id AND f.path=$3
        JOIN file_extractions e ON e.id=f.indexed_extraction_id
        JOIN file_versions v ON v.id=f.indexed_version_id
        JOIN root_index_namespaces n ON n.root_id=r.id AND n.org_id=r.org_id AND n.retired_at IS NULL
        WHERE r.org_id=$1 AND r.id=$2 AND r.deleting_at IS NULL
        AND NOT f.deleted AND f.indexed_extraction_id IS NOT NULL`, root.OrgID, root.ID, path).Scan(&snapshot.extraction, &snapshot.namespace,
		&snapshot.fileID, &snapshot.fileHash, &snapshot.rowFormat)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, errQueryRootNotFound
	}
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (snapshot fileReadSnapshot) filters(extra any) any {
	identity := []any{"extraction_id", "Eq", snapshot.extraction}
	if snapshot.rowFormat == 2 {
		identity = []any{"segment_id", "In", snapshot.segments}
	}
	parts := []any{[]any{"file_path", "Eq", snapshot.path}, identity}
	if extra != nil {
		parts = append(parts, extra)
	}
	return tpAndFilter(parts)
}

// A requested page or line can span arbitrarily many chunks. Keep one pinned
// publication across provider pages; never silently truncate an oversized read.
func (s *Server) readFileRows(ctx context.Context, snapshot fileReadSnapshot, filters any, bounds segmentReadRange) ([]map[string]any, error) {
	if snapshot.rowFormat == 2 {
		return s.readSegmentRows(ctx, snapshot, filters, bounds)
	}
	return collectFileRows(ctx, func(after int) ([]map[string]any, error) {
		parts := []any{snapshot.filters(filters)}
		if after >= 0 {
			parts = append(parts, []any{"chunk_index", "Gt", after})
		}
		return s.tp.Query(ctx, snapshot.namespace, TPQuery{RankBy: []any{"chunk_index", "asc"}, Limit: 512,
			Filters: tpAndFilter(parts), ExcludeAttributes: readExcludedAttrs()})
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
