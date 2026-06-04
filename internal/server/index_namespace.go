package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	defaultRootIndexNamespaceShards = 1
	maxRootIndexNamespaceShards     = 256
)

func rootIndexNamespaceShardCount() int {
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_TP_NAMESPACE_SHARDS"))
	if raw == "" {
		return defaultRootIndexNamespaceShards
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultRootIndexNamespaceShards
	}
	if n > maxRootIndexNamespaceShards {
		return maxRootIndexNamespaceShards
	}
	return n
}

func rootIndexNamespaceName(orgID, rootID string, shardIndex int) string {
	return fmt.Sprintf("pfs_%s_%s_s%03d", shortNamespaceHash(orgID), shortNamespaceHash(rootID), shardIndex)
}

func shortNamespaceHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}

func pathShardIndex(filePath string, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	sum := sha256.Sum256([]byte(filePath))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(shardCount))
}

func rootIndexNamespaceForPath(namespaces []models.RootIndexNamespace, filePath string) (models.RootIndexNamespace, error) {
	active := activeRootIndexNamespaces(namespaces)
	if len(active) == 0 {
		return models.RootIndexNamespace{}, fmt.Errorf("root has no active index namespaces")
	}
	shardCount := active[0].ShardCount
	if shardCount <= 0 {
		shardCount = len(active)
	}
	byShard := make(map[int]models.RootIndexNamespace, len(active))
	for _, ns := range active {
		if ns.ShardCount > 0 && ns.ShardCount != shardCount {
			return models.RootIndexNamespace{}, fmt.Errorf("root index namespace shard count mismatch: got %d and %d", shardCount, ns.ShardCount)
		}
		if ns.ShardIndex < 0 || ns.ShardIndex >= shardCount {
			return models.RootIndexNamespace{}, fmt.Errorf("root index namespace %s has invalid shard index %d for shard count %d", ns.Namespace, ns.ShardIndex, shardCount)
		}
		byShard[ns.ShardIndex] = ns
	}
	if len(byShard) != shardCount {
		return models.RootIndexNamespace{}, fmt.Errorf("root has %d active index namespaces, want %d", len(byShard), shardCount)
	}
	shard := pathShardIndex(filePath, shardCount)
	ns, ok := byShard[shard]
	if !ok {
		return models.RootIndexNamespace{}, fmt.Errorf("root missing index namespace shard %d", shard)
	}
	return ns, nil
}

func activeRootIndexNamespaces(namespaces []models.RootIndexNamespace) []models.RootIndexNamespace {
	active := make([]models.RootIndexNamespace, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns.RetiredAt == nil {
			active = append(active, ns)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].ShardIndex < active[j].ShardIndex
	})
	return active
}

func queueIndexNamespaces(namespaces []models.RootIndexNamespace) []queue.IndexNamespace {
	active := activeRootIndexNamespaces(namespaces)
	out := make([]queue.IndexNamespace, 0, len(active))
	for _, ns := range active {
		out = append(out, queue.IndexNamespace{
			Namespace:  ns.Namespace,
			ShardIndex: ns.ShardIndex,
			ShardCount: ns.ShardCount,
		})
	}
	return out
}

func modelIndexNamespaces(namespaces []queue.IndexNamespace, orgID, rootID string) []models.RootIndexNamespace {
	out := make([]models.RootIndexNamespace, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, models.RootIndexNamespace{
			OrgID:      orgID,
			RootID:     rootID,
			Namespace:  ns.Namespace,
			ShardIndex: ns.ShardIndex,
			ShardCount: ns.ShardCount,
		})
	}
	return out
}
