package server

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// authorizeCaptureCommit runs after S3 IO, with the root row already locked.
// Share locks keep the exact membership, credential and grants used by this
// decision valid until commit. A revocation that commits first is observed;
// one that arrives later waits for this short metadata transaction, never IO.
func authorizeCaptureCommit(ctx context.Context, tx pgx.Tx, id *auth.Identity, root *models.RootMetadata) (auth.Role, error) {
	var role auth.Role
	err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id=$1 AND user_id=$2 FOR SHARE`, id.OrgID, id.UserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCapturePathForbidden
	}
	if err != nil {
		return "", err
	}
	if id.APIKeyID != "" {
		var scopes []string
		err = tx.QueryRow(ctx, `SELECT scopes FROM api_keys WHERE id=$1 AND org_id=$2 AND user_id=$3
			AND (expires_at IS NULL OR expires_at>clock_timestamp()) FOR SHARE`, id.APIKeyID, id.OrgID, id.UserID).Scan(&scopes)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrCapturePathForbidden
		}
		if err != nil {
			return "", err
		}
		if !auth.HasScope(&auth.Identity{Scopes: scopes}, "sync", "write") {
			return "", ErrCapturePathForbidden
		}
	}
	// Use the locked group IDs below, not a second membership subquery that
	// could see newly inserted/unlocked membership and then race its deletion.
	rows, err := tx.Query(ctx, `SELECT gm.group_id FROM group_members gm
		WHERE gm.org_id=$1 AND gm.user_id=$2 AND EXISTS (
			SELECT 1 FROM root_grants rg WHERE rg.org_id=$1 AND rg.root_id=$3
			AND rg.principal_type='group' AND rg.principal_id=gm.group_id)
		ORDER BY gm.group_id FOR SHARE OF gm`, id.OrgID, id.UserID, root.ID)
	if err != nil {
		return "", err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	rows, err = tx.Query(ctx, `SELECT principal_type,permissions FROM root_grants
		WHERE org_id=$1 AND root_id=$2 AND (
			(principal_type='org' AND principal_id=$1) OR
			(principal_type='user' AND principal_id=$3) OR
			(principal_type='group' AND principal_id=ANY($4::text[])))
		ORDER BY id FOR SHARE`, id.OrgID, root.ID, id.UserID, groups)
	if err != nil {
		return "", err
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rootGrantPermissions])
	if err != nil {
		return "", err
	}
	permissions, _ := effectiveRootPermissions(root, id.UserID, role, grants)
	if !rootPermissionAllowed(permissions, models.RootPermissionSync) {
		return "", ErrCapturePathForbidden
	}
	return role, nil
}
