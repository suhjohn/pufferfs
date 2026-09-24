package server

import (
	"context"
	"errors"
)

type segmentReadRange struct {
	kind       string
	start, end int
}

type readSegment struct {
	id         string
	start, end int64
}

// Seek the pinned extraction's memberships in bounded pages. A read never
// constructs an unbounded provider filter containing an entire large file.
func (s *Server) readSegments(ctx context.Context, snapshot fileReadSnapshot, after int64, bounds segmentReadRange, limit int) ([]readSegment, error) {
	condition := ""
	args := []any{snapshot.extraction, after, snapshot.fileID, limit}
	switch bounds.kind {
	case "line":
		condition = " AND s.line_end >= $5 AND s.line_start <= $6"
	case "page":
		condition = " AND s.page_end >= $5 AND s.page_start <= $6"
	case "bounds":
		condition = ` AND m.ordinal_start IN (
			(SELECT ordinal_start FROM extraction_segments WHERE extraction_id=$1 ORDER BY ordinal_start LIMIT 1),
			(SELECT ordinal_start FROM extraction_segments WHERE extraction_id=$1 ORDER BY ordinal_start DESC LIMIT 1))`
	case "":
	default:
		return nil, errors.New("invalid segment read range")
	}
	if bounds.kind == "line" || bounds.kind == "page" {
		args = append(args, bounds.start, bounds.end)
	}
	rows, err := s.db.pool.Query(ctx, `SELECT s.id,m.ordinal_start,m.ordinal_start+s.chunk_count
		FROM extraction_segments m JOIN file_segments s ON s.id=m.segment_id
		WHERE m.extraction_id=$1 AND m.ordinal_start>$2 AND s.file_id=$3 AND s.retired_at IS NULL`+
		condition+` ORDER BY m.ordinal_start LIMIT $4`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var segments []readSegment
	for rows.Next() {
		var segment readSegment
		if err = rows.Scan(&segment.id, &segment.start, &segment.end); err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

func (s *Server) readSegmentRows(ctx context.Context, snapshot fileReadSnapshot, filters any, bounds segmentReadRange) ([]map[string]any, error) {
	var result []map[string]any
	var bytes int
	lastSegment, lastChunk := int64(-1), -1
	for {
		segments, err := s.readSegments(ctx, snapshot, lastSegment, bounds, 8)
		if err != nil {
			return nil, err
		}
		if len(segments) == 0 {
			return result, nil
		}
		snapshot.segments = nil
		allowed := make(map[string]readSegment, len(segments))
		for _, segment := range segments {
			snapshot.segments = append(snapshot.segments, segment.id)
			allowed[segment.id] = segment
		}
		// Eight segments contain at most 512 rows. The membership bound
		// makes a second provider query to discover an empty page unnecessary.
		rows, err := s.tp.Query(ctx, snapshot.namespace, TPQuery{RankBy: []any{"chunk_index", "asc"}, Limit: 512,
			Filters: snapshot.filters(filters), ExcludeAttributes: readExcludedAttrs()})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			ordinal := intFromAny(row["chunk_index"], -1)
			segment, ok := allowed[strVal(row, "segment_id")]
			if !ok || ordinal <= lastChunk || int64(ordinal) < segment.start || int64(ordinal) >= segment.end {
				return nil, errors.New("indexed row is outside the pinned file segments")
			}
			lastChunk = ordinal
			// An append reuses immutable rows whose original file hash belongs
			// to an older version. Proofs authorize the pinned publication.
			row["file_hash"] = snapshot.fileHash
			bytes += len(strVal(row, "content"))
			if bytes > 32*1024*1024 {
				return nil, errors.New("read exceeds 32 MiB; request a smaller range")
			}
			result = append(result, row)
		}
		lastSegment = segments[len(segments)-1].start
		if len(segments) < 8 {
			return result, nil
		}
	}
}

func (s *Server) readFileMetadata(ctx context.Context, snapshot fileReadSnapshot) ([]map[string]any, error) {
	if snapshot.rowFormat == 2 {
		// The first and last segments supply true file bounds without loading
		// every membership or truncating metadata at an arbitrary first page.
		segments, err := s.readSegments(ctx, snapshot, -1, segmentReadRange{kind: "bounds"}, 2)
		if err != nil {
			return nil, err
		}
		if len(segments) == 0 {
			return nil, nil
		}
		for _, segment := range segments {
			snapshot.segments = append(snapshot.segments, segment.id)
		}
	}
	rows, err := s.tp.Query(ctx, snapshot.namespace, TPQuery{RankBy: []any{"chunk_index", "asc"}, Limit: 1000,
		Filters: snapshot.filters(nil), ExcludeAttributes: append(readExcludedAttrs(), "content")})
	if err == nil && snapshot.rowFormat == 2 {
		for _, row := range rows {
			row["file_hash"] = snapshot.fileHash
		}
	}
	return rows, err
}
