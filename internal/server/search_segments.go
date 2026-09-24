package server

import "context"

type segmentCandidate struct {
	Path       string `json:"path"`
	Segment    string `json:"segment"`
	Extraction string `json:"extraction"`
}

type segmentLookup struct {
	Slot   int    `json:"slot"`
	RootID string `json:"root_id"`
	segmentCandidate
}

// Validate only newly seen candidate memberships, against the publication
// pinned by this request. A reused segment remains visible even though its
// immutable rows carry their original extraction/version identifiers.
func (s *Server) candidateSegments(ctx context.Context, orgID string, searches []namespaceSearch, pending []int) error {
	var lookups []segmentLookup
	for _, slot := range pending {
		search := &searches[slot]
		for _, rows := range search.sets {
			for _, row := range rows {
				path := strVal(row, "file_path")
				publication := search.publications[path]
				segment := strVal(row, "segment_id")
				if publication.deleted || publication.rowFormat != 2 || segment == "" {
					continue
				}
				candidate := segmentCandidate{Path: path, Segment: segment, Extraction: publication.extraction}
				if _, seen := search.segments[candidate]; seen {
					continue
				}
				if len(search.segments) >= maxPublicationCandidatePaths {
					return errSearchPublicationBusy
				}
				search.segments[candidate] = false
				lookups = append(lookups, segmentLookup{Slot: slot, RootID: search.rootID, segmentCandidate: candidate})
			}
		}
	}
	if len(lookups) == 0 {
		return nil
	}
	rows, err := s.db.pool.Query(ctx, `SELECT q.slot,q.path,q.segment,q.extraction
		FROM jsonb_to_recordset($2) AS q(slot int,root_id text,path text,segment text,extraction text)
		JOIN roots r ON r.id=q.root_id AND r.org_id=$1 AND r.deleting_at IS NULL
		JOIN file_catalog f ON f.root_id=r.id AND f.path=q.path
		JOIN file_versions v ON v.file_id=f.id
		JOIN file_extractions e ON e.version_id=v.id AND e.id=q.extraction AND e.row_format=2
		JOIN extraction_segments m ON m.extraction_id=e.id AND m.segment_id=q.segment
		JOIN file_segments s ON s.id=m.segment_id AND s.file_id=f.id AND s.retired_at IS NULL`, orgID, lookups)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slot int
		var candidate segmentCandidate
		if err := rows.Scan(&slot, &candidate.Path, &candidate.Segment, &candidate.Extraction); err != nil {
			return err
		}
		searches[slot].segments[candidate] = true
	}
	return rows.Err()
}
