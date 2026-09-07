package server

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/auth"
)

// The caller holds the root lock. Validate only NEW versions, against one
// locked snapshot of their source objects. Unbound packs may serve multiple
// files in this batch; previously bound packs permit only this file's prior
// ranges. Source bodies stay in S3, and no lock spans object-store IO.
func authorizeCaptureSources(ctx context.Context, tx pgx.Tx, id *auth.Identity, rootID, captureID string, files []captureWrite) error {
	hasExtents := false
	for _, file := range files {
		hasExtents = hasExtents || len(file.Extents) > 0
	}
	if !hasExtents {
		return nil
	}
	rows, err := tx.Query(ctx, `WITH requested AS MATERIALIZED (
		SELECT i.previous_version_id,e.* FROM jsonb_to_recordset($4) AS i(previous_version_id text,extents jsonb)
		CROSS JOIN LATERAL jsonb_to_recordset(COALESCE(i.extents,'[]'::jsonb)) AS e(object_key text,"offset" bigint,length bigint)
	), objects AS MATERIALIZED (
		SELECT object_key,size_bytes,uploader_id,capture_id,retired_at FROM source_objects
		WHERE org_id=$1 AND root_id=$2 AND completed_at IS NOT NULL
		AND object_key IN (SELECT object_key FROM requested) ORDER BY object_key FOR UPDATE
	), checked AS (
		SELECT e.object_key,CASE
			WHEN o.object_key IS NULL OR e."offset"<0 OR e.length<=0
				OR e."offset">o.size_bytes OR e.length>o.size_bytes-e."offset" THEN 'unavailable'
			WHEN EXISTS (SELECT 1 FROM file_version_extents old
				WHERE old.version_id=NULLIF(e.previous_version_id,'') AND old.object_key=e.object_key
				AND e."offset">=old.byte_offset AND e."offset"-old.byte_offset<=old.byte_length
				AND e.length<=old.byte_length-(e."offset"-old.byte_offset))
				THEN CASE WHEN o.retired_at IS NULL THEN '' ELSE 'retired' END
			WHEN o.uploader_id<>$3 OR o.capture_id IS NOT NULL THEN 'unavailable'
			WHEN o.retired_at IS NOT NULL THEN 'retired'
			ELSE '' END AS problem
		FROM requested e LEFT JOIN objects o ON o.object_key=e.object_key
	), problems AS MATERIALIZED (
		SELECT DISTINCT object_key,problem FROM checked WHERE problem<>''
	), bound AS (
		UPDATE source_objects s SET capture_id=$5 FROM objects o
		WHERE s.object_key=o.object_key AND o.capture_id IS NULL AND o.uploader_id=$3
		AND NOT EXISTS (SELECT 1 FROM problems)
	) SELECT object_key,problem FROM problems ORDER BY object_key,problem`, id.OrgID, rootID, id.UserID, files, captureID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var retired []string
	for rows.Next() {
		var key, problem string
		if err = rows.Scan(&key, &problem); err != nil {
			return err
		}
		switch problem {
		case "unavailable":
			return errSourceExtentUnavailable
		case "retired":
			retired = append(retired, key)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(retired) > 0 {
		return &retiredSourcePacksError{Keys: retired}
	}
	return nil
}
