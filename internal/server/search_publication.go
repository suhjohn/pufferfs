package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

const (
	maxPublicationSearchPasses    = 16
	maxPublicationExclusions      = 4096
	maxPublicationFilterBytes     = 1 << 20
	maxPublicationCandidatePaths  = 8192
	maxConcurrentSearchNamespaces = 16
)

var errSearchPublicationBusy = errors.New("search publication validation could not finish; retry after indexing or cleanup progresses")

type filePublication struct {
	extraction string
	deleted    bool
}

// A namespace keeps its validated ranks, pinned publications and exclusions
// within this request. Only namespaces with rejected candidates run again.
type namespaceSearch struct {
	rootID       string
	namespace    string
	rankings     []any
	sets         [][]map[string]any
	publications map[string]filePublication
	excluded     map[string]bool
	filterBytes  int
}

type rootPathLookup struct {
	Slot   int      `json:"slot"`
	RootID string   `json:"root_id"`
	Paths  []string `json:"paths"`
}

// One read for all newly observed candidate paths in a round. The existing
// (root_id,path) index bounds the catalog lookup; outer joins also report a
// deleted root or an uncataloged path, without a separate existence query.
const candidatePublicationsSQL = `SELECT q.slot,r.id IS NOT NULL,f.path,
	COALESCE(f.indexed_extraction_id,''),COALESCE(f.deleted,FALSE)
	FROM jsonb_to_recordset($2) AS q(slot int,root_id text,paths text[])
	LEFT JOIN roots r ON r.org_id=$1 AND r.id=q.root_id AND r.deleting_at IS NULL
	LEFT JOIN file_catalog f ON f.root_id=r.id AND f.path=ANY(q.paths)`

func (s *Server) candidatePublications(ctx context.Context, orgID string, searches []namespaceSearch, lookups []rootPathLookup) error {
	if len(lookups) == 0 {
		return nil
	}
	rows, err := s.db.pool.Query(ctx, candidatePublicationsSQL, orgID, lookups)
	if err != nil {
		return err
	}
	defer rows.Close()
	for _, lookup := range lookups {
		for _, path := range lookup.Paths {
			searches[lookup.Slot].publications[path] = filePublication{}
		}
	}
	for rows.Next() {
		var slot int
		var exists bool
		var path *string
		var publication filePublication
		if err := rows.Scan(&slot, &exists, &path, &publication.extraction, &publication.deleted); err != nil {
			return err
		}
		if !exists {
			return errQueryRootNotFound
		}
		if path != nil {
			searches[slot].publications[*path] = publication
		}
	}
	return rows.Err()
}

// Fetch each namespace's complete ranked lists, then validate all newly seen
// candidates in one SQL call. Hybrid lists are validated before fusion. No DB
// connection is held during provider IO; each file's first observed publication
// is pinned for the request so a pending version cannot displace it mid-retry.
// Bounds remain per namespace, including when other namespaces finish early.
func (s *Server) queryPublishedRows(ctx context.Context, orgID string, searches []namespaceSearch, query func(namespaceSearch, any) ([][]map[string]any, error)) error {
	pending := make([]int, len(searches))
	for i := range searches {
		pending[i] = i
		searches[i].publications = make(map[string]filePublication)
		searches[i].excluded = make(map[string]bool)
	}
	for range maxPublicationSearchPasses {
		if len(pending) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Bound provider concurrency across the whole request, rather than
		// spawning one goroutine for every shard of an arbitrarily large root.
		jobs := make(chan int, len(pending))
		for _, i := range pending {
			jobs <- i
		}
		close(jobs)
		errs := make([]error, len(searches))
		var wg sync.WaitGroup
		for range min(len(pending), maxConcurrentSearchNamespaces) {
			wg.Go(func() {
				for i := range jobs {
					search := &searches[i]
					search.sets, errs[i] = query(*search, searchPublicationFilter(slices.Sorted(maps.Keys(search.excluded))))
				}
			})
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		var lookups []rootPathLookup
		for _, i := range pending {
			if errs[i] != nil {
				return fmt.Errorf("querying turbopuffer namespace %s: %w", searches[i].namespace, errs[i])
			}
			search := &searches[i]
			unseen := make(map[string]bool)
			for _, rows := range search.sets {
				for _, row := range rows {
					path := strVal(row, "file_path")
					if path == "" {
						return errors.New("index candidate is missing its file path")
					}
					if strVal(row, "extraction_id") == "" {
						return errors.New("index candidate has an invalid extraction identity")
					}
					if _, exists := search.publications[path]; !exists {
						unseen[path] = true
					}
				}
			}
			if len(search.publications)+len(unseen) > maxPublicationCandidatePaths {
				return errSearchPublicationBusy
			}
			if len(unseen) > 0 {
				lookups = append(lookups, rootPathLookup{Slot: i, RootID: search.rootID, Paths: slices.Sorted(maps.Keys(unseen))})
			}
		}
		if err := s.candidatePublications(ctx, orgID, searches, lookups); err != nil {
			return err
		}
		next := pending[:0]
		for _, i := range pending {
			search := &searches[i]
			before := len(search.excluded)
			rejected := false
			for _, rows := range search.sets {
				for _, row := range rows {
					path, extraction := strVal(row, "file_path"), strVal(row, "extraction_id")
					published := search.publications[path]
					if !published.deleted && published.extraction == extraction {
						continue
					}
					rejected = true
					if !search.excluded[extraction] {
						search.excluded[extraction] = true
						search.filterBytes += 6*len(extraction) + 32 // Conservative JSON escaping bound.
					}
				}
			}
			if rejected {
				if len(search.excluded) == before || len(search.excluded) > maxPublicationExclusions || search.filterBytes > maxPublicationFilterBytes {
					return errSearchPublicationBusy
				}
				next = append(next, i)
			}
		}
		pending = next
	}
	if len(pending) == 0 {
		return nil
	}
	return errSearchPublicationBusy
}

func searchPublicationFilter(excludedExtractions []string) any {
	current := []any{[]any{"extraction_id", "NotEq", nil}}
	if len(excludedExtractions) > 0 {
		current = append(current, []any{"extraction_id", "NotIn", excludedExtractions})
	}
	return tpAndFilter(current)
}
