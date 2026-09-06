package server

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/auth"
)

// Caller holds the root lock. New packs belong to their uploader and one
// capture. Reuse in later captures is restricted to ranges in THIS file's
// previous version, never a different path sharing the same physical pack.
func authorizeSourceExtents(ctx context.Context, tx pgx.Tx, id *auth.Identity, rootID, captureID string, file CapturedFileVersion, newPacks map[string]bool) error {
	if len(file.Extents) == 0 {
		return nil
	}
	keys := make([]string, 0, len(file.Extents))
	for _, extent := range file.Extents {
		keys = append(keys, extent.ObjectKey)
	}
	rows, err := tx.Query(ctx, `SELECT object_key,size_bytes,COALESCE(uploader_id,''),COALESCE(capture_id,''),retired_at IS NOT NULL
        FROM source_objects WHERE org_id=$1 AND root_id=$2 AND object_key=ANY($3)
        AND completed_at IS NOT NULL FOR UPDATE`, id.OrgID, rootID, keys)
	if err != nil {
		return err
	}
	type object struct {
		size              int64
		uploader, capture string
		retired           bool
	}
	objects := make(map[string]object)
	for rows.Next() {
		var key string
		var value object
		if err = rows.Scan(&key, &value.size, &value.uploader, &value.capture, &value.retired); err != nil {
			break
		}
		objects[key] = value
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	previous := make(map[string][][2]int64)
	if file.PreviousVersionID != "" {
		var ready bool
		if err = tx.QueryRow(ctx, `SELECT extents_indexed_at IS NOT NULL FROM file_versions WHERE id=$1`, file.PreviousVersionID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return errSourceCatalogPending
		}
		rows, err = tx.Query(ctx, `SELECT object_key,byte_offset,byte_length FROM file_version_extents WHERE version_id=$1`, file.PreviousVersionID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key string
			var offset, length int64
			if err = rows.Scan(&key, &offset, &length); err != nil {
				break
			}
			previous[key] = append(previous[key], [2]int64{offset, length})
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
	}
	var retired []string
	for _, extent := range file.Extents {
		object, ok := objects[extent.ObjectKey]
		if !ok || extent.Offset > object.size || extent.Length > object.size-extent.Offset {
			return errSourceExtentUnavailable
		}
		reusable := false
		for _, span := range previous[extent.ObjectKey] {
			if extent.Offset >= span[0] && extent.Offset-span[0] <= span[1] && extent.Length <= span[1]-(extent.Offset-span[0]) {
				reusable = true
				break
			}
		}
		// Pre-provenance uploads cannot be attributed to a user. A caller with
		// retained bytes can upload a fresh identity; knowledge of the old key
		// alone never authorizes new file bindings.
		if !reusable && object.uploader == "" {
			retired = append(retired, extent.ObjectKey)
			continue
		}
		if !reusable && !(object.uploader == id.UserID && (object.capture == "" || newPacks[extent.ObjectKey])) {
			return errSourceExtentUnavailable
		}
		if object.retired {
			retired = append(retired, extent.ObjectKey)
		}
	}
	if len(retired) > 0 {
		return &retiredSourcePacksError{Keys: retired}
	}
	// Only packs first bound in THIS transaction may serve another file in the
	// same batch. Reusing an old capture_id cannot authorize a new path later.
	for key, object := range objects {
		if object.capture == "" && object.uploader == id.UserID {
			newPacks[key] = true
		}
	}
	_, err = tx.Exec(ctx, `UPDATE source_objects SET capture_id=$1 WHERE object_key=ANY($2) AND capture_id IS NULL AND uploader_id=$3`, captureID, keys, id.UserID)
	return err
}
