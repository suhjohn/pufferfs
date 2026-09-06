package server

import (
	"context"
	"errors"
	"maps"
	"slices"
)

const (
	maxPublicationSearchPasses   = 16
	maxPublicationExclusions     = 4096
	maxPublicationFilterBytes    = 1 << 20
	maxPublicationCandidatePaths = 8192
)

var errSearchPublicationBusy = errors.New("search publication validation could not finish; retry after indexing or cleanup progresses")

type filePublication struct {
	extraction string
	deleted    bool
}

// The unique (root_id,path) index limits this lookup to candidate files. The
// outer join distinguishes an uncataloged path from a deleted root.
const candidatePublicationsSQL = `SELECT f.path,COALESCE(f.indexed_extraction_id,''),COALESCE(f.deleted,FALSE)
	FROM roots r LEFT JOIN file_catalog f ON f.root_id=r.id AND f.path=ANY($3)
	WHERE r.org_id=$1 AND r.id=$2 AND r.deleting_at IS NULL`

func (s *Server) candidatePublications(ctx context.Context, orgID, rootID string, paths []string) (map[string]filePublication, error) {
	rows, err := s.db.pool.Query(ctx, candidatePublicationsSQL, orgID, rootID, paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]filePublication, len(paths))
	rootExists := false
	for rows.Next() {
		rootExists = true
		var path *string
		var publication filePublication
		if err := rows.Scan(&path, &publication.extraction, &publication.deleted); err != nil {
			return nil, err
		}
		if path != nil {
			result[*path] = publication
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !rootExists {
		return nil, errQueryRootNotFound
	}
	return result, nil
}

// Validate each complete ranked candidate set before returning it. Re-query
// after excluding rejected extractions, rather than
// dropping bad rows from a fixed top-k and silently losing current hits. Hybrid
// search must validate both rank lists before fusion. No results are accumulated
// across passes and no database transaction/connection is held during index IO.
// Pin each file's first observed publication for this search. Otherwise an
// excluded pending extraction could become current between passes and make both
// versions disappear. This is a per-file snapshot, not a whole-root snapshot.
func (s *Server) queryPublishedRows(ctx context.Context, orgID, rootID string, query func(any) ([][]map[string]any, error)) ([][]map[string]any, error) {
	excluded := make(map[string]bool)
	publications := make(map[string]filePublication)
	filterBytes := 0
	for range maxPublicationSearchPasses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		filter := searchPublicationFilter(slices.Sorted(maps.Keys(excluded)))
		sets, err := query(filter)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		paths := make(map[string]bool)
		for _, rows := range sets {
			for _, row := range rows {
				path := strVal(row, "file_path")
				if path == "" {
					return nil, errors.New("index candidate is missing its file path")
				}
				if strVal(row, "extraction_id") == "" {
					return nil, errors.New("index candidate has an invalid extraction identity")
				}
				paths[path] = true
			}
		}
		if len(paths) == 0 {
			return sets, nil
		}
		var unseen []string
		for path := range paths {
			if _, exists := publications[path]; !exists {
				unseen = append(unseen, path)
			}
		}
		if len(publications)+len(unseen) > maxPublicationCandidatePaths {
			return nil, errSearchPublicationBusy
		}
		if len(unseen) > 0 {
			slices.Sort(unseen)
			observed, err := s.candidatePublications(ctx, orgID, rootID, unseen)
			if err != nil {
				return nil, err
			}
			for _, path := range unseen {
				publications[path] = observed[path]
			}
		}
		before := len(excluded)
		rejected := false
		for _, rows := range sets {
			for _, row := range rows {
				path, extraction := strVal(row, "file_path"), strVal(row, "extraction_id")
				published := publications[path]
				if !published.deleted && published.extraction == extraction {
					continue
				}
				rejected = true
				if !excluded[extraction] {
					excluded[extraction] = true
					// Conservative JSON escaping bound; avoid serializing the filter
					// a second time merely to measure it.
					filterBytes += 6*len(extraction) + 32
				}
			}
		}
		if !rejected {
			return sets, nil
		}
		after := len(excluded)
		if after == before || after > maxPublicationExclusions || filterBytes > maxPublicationFilterBytes {
			return nil, errSearchPublicationBusy
		}
	}
	return nil, errSearchPublicationBusy
}

func searchPublicationFilter(excludedExtractions []string) any {
	current := []any{[]any{"extraction_id", "NotEq", nil}}
	if len(excludedExtractions) > 0 {
		current = append(current, []any{"extraction_id", "NotIn", excludedExtractions})
	}
	return tpAndFilter(current)
}
