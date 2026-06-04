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
	if localChunkable("docs/manual.pdf") {
		t.Fatal("pdf should route to Modal")
	}
	if localChunkable("slides/deck.pptx") {
		t.Fatal("pptx should route to Modal")
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
