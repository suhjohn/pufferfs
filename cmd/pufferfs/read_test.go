package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestParseReadRange(t *testing.T) {
	got, err := parseReadRange("200:400")
	if err != nil {
		t.Fatalf("parseReadRange: %v", err)
	}
	if got.Start != 200 || got.End != 400 {
		t.Fatalf("range = %#v, want 200:400", got)
	}
}

func TestParseReadRangeRejectsInvalidRange(t *testing.T) {
	if _, err := parseReadRange("400:200"); err == nil {
		t.Fatal("parseReadRange accepted descending range")
	}
	if _, err := parseReadRange("200"); err == nil {
		t.Fatal("parseReadRange accepted missing end")
	}
}

func TestWriteReadResultsLines(t *testing.T) {
	resp := models.ReadFileResponse{
		Mode: "lines",
		Lines: []models.ReadLineResult{
			{LineNumber: 200, Content: "first"},
			{LineNumber: 201, Content: "second"},
		},
	}

	var out bytes.Buffer
	writeReadResults(&out, resp, nil)
	got := out.String()
	if !strings.Contains(got, "200\tfirst\n") || !strings.Contains(got, "201\tsecond\n") {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestWriteReadResultsPagesShowsImageFile(t *testing.T) {
	imageURL := "/roots/root-1/assets?key=chunks%2Froot-1%2Fdoc.pdf.0.jpg"
	resp := models.ReadFileResponse{
		Mode: "pages",
		Pages: []models.ReadPageResult{{
			Page:     1,
			Content:  "page text",
			ImageURL: &imageURL,
		}},
	}

	var out bytes.Buffer
	writeReadResults(&out, resp, map[int]string{1: "out/doc-page-1.jpg"})
	got := out.String()
	for _, want := range []string{"page: 1", "image_url: " + imageURL, "image_file: out/doc-page-1.jpg", "content: page text"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
