package server

import (
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestShardChangesSkipsUnchangedAndBoundsFiles(t *testing.T) {
	changes := []models.FileChange{
		{Path: "a.txt", Status: models.StatusAdded, Size: 10},
		{Path: "b.txt", Status: models.StatusUnchanged, Size: 10},
		{Path: "c.txt", Status: models.StatusModified, Size: 10},
		{Path: "d.txt", Status: models.StatusRemoved, Size: 10},
	}
	shards := shardChanges(changes, 2, 1<<20)
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	if len(shards[0]) != 2 || shards[0][0].Path != "a.txt" || shards[0][1].Path != "c.txt" {
		t.Fatalf("first shard = %#v", shards[0])
	}
	if len(shards[1]) != 1 || shards[1][0].Path != "d.txt" {
		t.Fatalf("second shard = %#v", shards[1])
	}
}

func TestShardChangesBoundsBytes(t *testing.T) {
	changes := []models.FileChange{
		{Path: "a.txt", Status: models.StatusAdded, Size: 80},
		{Path: "b.txt", Status: models.StatusAdded, SourceLength: 80},
		{Path: "c.txt", Status: models.StatusAdded, Size: 10},
	}
	shards := shardChanges(changes, 5000, 100)
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	if len(shards[0]) != 1 || len(shards[1]) != 2 {
		t.Fatalf("unexpected shard sizes: %d, %d", len(shards[0]), len(shards[1]))
	}
}
