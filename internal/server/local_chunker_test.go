package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestLocalChunkableSkipsModalOnlyForSimpleTextFormats(t *testing.T) {
	if !localChunkable("README.md") {
		t.Fatal("markdown should be chunked locally")
	}
	if !localChunkable("src/main.go") {
		t.Fatal("code should be chunked locally")
	}
	if !localChunkable("web/public/favicon.svg") {
		t.Fatal("svg should be chunked locally as text")
	}
	if got := detectLocalFileType("web/public/favicon.svg"); got != "svg" {
		t.Fatalf("detectLocalFileType(svg) = %q, want svg", got)
	}
	if localChunkable("docs/manual.pdf") {
		t.Fatal("pdf should route to Modal")
	}
	if localChunkable("slides/deck.pptx") {
		t.Fatal("pptx should route to Modal")
	}
	for _, path := range []string{
		"mail/message.eml",
		"mail/message.msg",
		"contacts/team.vcf",
		"calendar/launch.ics",
		"media/call.mp3",
		"media/call.wav",
		"media/demo.mp4",
		"media/demo.mov",
	} {
		if localChunkable(path) {
			t.Fatalf("%s should route to Modal", path)
		}
	}
}

func TestDetectFileTypeRecognizesStructuredAndMediaFiles(t *testing.T) {
	cases := map[string]string{
		"mail/message.eml":  "eml",
		"mail/message.msg":  "msg",
		"contacts/team.vcf": "vcf",
		"calendar/demo.ics": "ics",
		"media/call.mp3":    "audio",
		"media/call.wav":    "audio",
		"media/demo.mp4":    "video",
		"media/demo.mov":    "video",
	}
	for path, want := range cases {
		if got := detectFileType(path); got != want {
			t.Fatalf("detectFileType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestChunkMarkdownUsesSlidingOverlap(t *testing.T) {
	var b strings.Builder
	for i := range 80 {
		fmt.Fprintf(&b, "Sentence %03d describes local sliding window chunking behavior for retrieval quality. ", i)
	}

	chunks := chunkMarkdown(b.String(), "root", "docs/readme.md", "markdown")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	first := chunks[0]["content"].(string)
	second := chunks[1]["content"].(string)
	needle := first[len(first)-120:]
	if !strings.Contains(second, needle) {
		t.Fatalf("expected second chunk to contain overlap from first chunk")
	}
	if len(first) > textChunkChars {
		t.Fatalf("first chunk length %d exceeds target %d", len(first), textChunkChars)
	}
}

func TestChunkCodeUsesLineBoundarySlidingOverlap(t *testing.T) {
	var b strings.Builder
	for i := range 220 {
		fmt.Fprintf(&b, "func example%03d() { println(%d) }\n", i, i)
	}

	chunks := chunkCode(b.String(), "root", "src/main.go", "go")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	first := chunks[0]["content"].(string)
	second := chunks[1]["content"].(string)
	lines := strings.Split(strings.TrimSpace(first), "\n")
	if len(lines) == 0 {
		t.Fatal("expected first chunk to contain code lines")
	}
	overlapLine := lines[len(lines)-1]
	if !strings.Contains(second, overlapLine) {
		t.Fatalf("expected second chunk to overlap on line %q", overlapLine)
	}
	if !strings.HasSuffix(first, "\n") {
		t.Fatal("expected code chunk to preserve line boundary")
	}
}

func TestChunkCodeAddsLineSpanMetadata(t *testing.T) {
	chunks := chunkCode("line 1\nline 2\nline 3\n", "root", "src/main.go", "go")
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if got := chunks[0]["line_start"]; got != 1 {
		t.Fatalf("line_start = %#v, want 1", got)
	}
	if got := chunks[0]["line_end"]; got != 3 {
		t.Fatalf("line_end = %#v, want 3", got)
	}
}

func TestChunkMarkdownAddsLineSpanMetadata(t *testing.T) {
	text := "# Intro\nline 2\n\n# Next\nline 5\n"
	chunks := chunkMarkdown(text, "root", "docs/readme.md", "markdown")
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if got := chunks[0]["line_start"]; got != 1 {
		t.Fatalf("first line_start = %#v, want 1", got)
	}
	if got := chunks[0]["line_end"]; got != 3 {
		t.Fatalf("first line_end = %#v, want 3", got)
	}
	if got := chunks[1]["line_start"]; got != 4 {
		t.Fatalf("second line_start = %#v, want 4", got)
	}
	if got := chunks[1]["line_end"]; got != 5 {
		t.Fatalf("second line_end = %#v, want 5", got)
	}
}
