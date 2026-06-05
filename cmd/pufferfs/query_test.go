package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestWriteQueryResultsUsesSimpleKeyValueOutput(t *testing.T) {
	longContent := strings.Repeat("x", 600)
	resp := models.QueryResponse{
		Results: []models.QueryResult{{
			FilePath:   "notes.md",
			ChunkIndex: 2,
			Content:    longContent,
			Score:      0.812345,
			FileType:   "markdown",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	if strings.Contains(got, "---") {
		t.Fatalf("output contains decorative separator: %q", got)
	}
	if !strings.Contains(got, "1.\n") {
		t.Fatalf("output does not contain numbered result: %q", got)
	}
	if !strings.Contains(got, "score: 0.8123") {
		t.Fatalf("output does not contain formatted score: %q", got)
	}
	if !strings.Contains(got, longContent) {
		t.Fatalf("output truncated content: %q", got)
	}
}

func TestWriteQueryResultsFlattensJSONContent(t *testing.T) {
	resp := models.QueryResponse{
		Results: []models.QueryResult{{
			FilePath:   "data.json",
			ChunkIndex: 0,
			Content:    `{"user":{"name":"Ada","roles":["admin","editor"]},"active":true}`,
			Score:      1,
			FileType:   "json",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	for _, want := range []string{
		"content.active: true",
		"content.user.name: Ada",
		"content.user.roles[0]: admin",
		"content.user.roles[1]: editor",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
