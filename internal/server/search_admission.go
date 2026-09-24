package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errSearchAdmissionFull = errors.New("search capacity is busy; retry shortly")

type searchLimits struct {
	global int
	tenant int
}

// ConfigureSearchAdmissionFromEnv applies the same capacity to all API replicas.
// Slots count concurrent namespace requests, including publication retries.
func (s *Server) ConfigureSearchAdmissionFromEnv() error {
	for name, target := range map[string]*int{
		"PUFFERFS_SEARCH_CONCURRENCY":        &s.searchLimits.global,
		"PUFFERFS_SEARCH_TENANT_CONCURRENCY": &s.searchLimits.tenant,
	} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 4096 {
				return fmt.Errorf("%s must be an integer between 1 and 4096", name)
			}
			*target = n
		}
	}
	if s.searchLimits.tenant > s.searchLimits.global {
		return errors.New("PUFFERFS_SEARCH_TENANT_CONCURRENCY cannot exceed PUFFERFS_SEARCH_CONCURRENCY")
	}
	return nil
}

// Reserve a query's maximum parallelism once, without holding a connection
// during provider IO. The transaction lock serializes admission across replicas;
// a separate statement after locking sees the preceding admission's commit.
// All provider calls use the query's 30s deadline. A 45s lease also bounds leaked
// capacity after a process crash. Full capacity fails fast instead of queuing an
// unbounded number of waiting requests/connections inside the API.
func (s *Server) admitSearch(ctx context.Context, orgID string, slots int) (func(), error) {
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pufferfs/search-admission',0))`); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM search_leases WHERE expires_at<=clock_timestamp()`); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	result, err := tx.Exec(ctx, `INSERT INTO search_leases(id,org_id,slots,expires_at)
		SELECT $1,$2,$3::integer,clock_timestamp()+INTERVAL '45 seconds'
		WHERE (SELECT COALESCE(sum(slots),0) FROM search_leases)+$3::integer<=$4::bigint
		AND (SELECT COALESCE(sum(slots),0) FROM search_leases WHERE org_id=$2)+$3::integer<=$5::bigint`,
		id, orgID, slots, s.searchLimits.global, s.searchLimits.tenant)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if result.RowsAffected() == 0 {
		return nil, errSearchAdmissionFull
	}
	return func() {
		// Client cancellation must still release the reservation immediately.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := s.db.pool.Exec(releaseCtx, `DELETE FROM search_leases WHERE id=$1`, id); err != nil {
			log.Printf("search admission release failed; reservation expires automatically: %v", err)
		}
	}, nil
}
