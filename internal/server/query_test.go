package server

import (
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestFilterDeniedQueryRows(t *testing.T) {
	rows := []map[string]any{
		{"file_path": "allowed/a.md"},
		{"file_path": "private/b.md"},
		{"file_path": "private/nested/c.md"},
	}

	filtered := filterDeniedQueryRows(rows, []string{"/private"})

	if len(filtered) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(filtered))
	}
	if got := strVal(filtered[0], "file_path"); got != "allowed/a.md" {
		t.Fatalf("remaining file_path = %q", got)
	}
}

func TestQueryResultsFromRowsAddsRootMetadata(t *testing.T) {
	results := queryResultsFromRows(models.RootMetadata{
		ID:   "root-1",
		Name: "contracts",
	}, []map[string]any{{
		"file_path":     "agreement.md",
		"absolute_path": "/workspace/agreement.md",
		"content":       "renewal terms",
		"file_type":     "markdown",
		"$dist":         0.75,
		"chunk_index":   float64(2),
		"page_number":   float64(4),
		"image_path":    "pages/agreement-4.png",
	}})

	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	got := results[0]
	if got.RootID != "root-1" || got.RootName != "contracts" {
		t.Fatalf("root metadata = %q/%q", got.RootID, got.RootName)
	}
	if got.FilePath != "agreement.md" || got.AbsolutePath != "/workspace/agreement.md" || got.ChunkIndex != 2 {
		t.Fatalf("result = %#v", got)
	}
	if got.PageNumber == nil || *got.PageNumber != 4 {
		t.Fatalf("page number = %#v", got.PageNumber)
	}
	if got.ImagePath == nil || *got.ImagePath != "pages/agreement-4.png" {
		t.Fatalf("image path = %#v", got.ImagePath)
	}
}
