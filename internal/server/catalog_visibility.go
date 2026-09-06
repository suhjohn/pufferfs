package server

import (
	"context"
	"errors"
)

// File reads keep one path-scoped publication snapshot throughout pagination.
// Search uses candidate validation instead; never allow a root-wide catalog scan.
func (s *Server) catalogVisibilitySnapshot(ctx context.Context, orgID, rootID, path string) (any, error) {
	if path == "" {
		return nil, errors.New("file visibility requires a path")
	}
	publications, err := s.candidatePublications(ctx, orgID, rootID, []string{path})
	if err != nil {
		return nil, err
	}
	publication := publications[path]
	if publication.deleted || publication.extraction == "" {
		return nil, errQueryRootNotFound
	}
	return []any{"extraction_id", "Eq", publication.extraction}, nil
}
