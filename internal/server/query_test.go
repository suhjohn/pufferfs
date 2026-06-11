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

func TestReadLineRangeUnavailableErrorSuggestsPagesForPDF(t *testing.T) {
	err := readLineRangeUnavailableError("docs/manual.pdf", &models.ReadRange{Start: 200, End: 400}, []map[string]any{
		{"file_type": "pdf", "page_number": 0},
		{"file_type": "pdf", "page_number": 41},
	})
	want := "line ranges unavailable for docs/manual.pdf; file_type=pdf; supports page reads instead (available pages 1:42, use --pages 1:42)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestReadLineRangeUnavailableErrorReportsIndexedLineRange(t *testing.T) {
	err := readLineRangeUnavailableError("src/main.go", &models.ReadRange{Start: 200, End: 400}, []map[string]any{
		{"file_type": "go", "line_start": 1, "line_end": 120},
	})
	want := "line range 200:400 unavailable for src/main.go; file_type=go; indexed line range is 1:120"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestReadLineRangeUnavailableErrorReportsMissingLineMetadata(t *testing.T) {
	err := readLineRangeUnavailableError("notes.md", &models.ReadRange{Start: 1, End: 20}, []map[string]any{
		{"file_type": "markdown"},
	})
	want := "line ranges unavailable for notes.md; file_type=markdown was indexed without line metadata; resync this file or root and retry --lines"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
