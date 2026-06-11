package server

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// localChunkable returns true if the file type can be chunked locally without Modal.
func localChunkable(filePath string) bool {
	if strings.EqualFold(extOf(filePath), ".svg") {
		return true
	}
	ft := detectFileType(filePath)
	// Binary and structured formats require Modal for specialized extraction.
	switch ft {
	case "pdf", "docx", "pptx", "image", "eml", "msg", "vcf", "ics", "audio", "video":
		return false
	}
	// Everything else (code, markdown, text) can be chunked locally.
	return true
}

// chunkLocally splits text content into chunks matching the Python chunker output format.
// Returns []map[string]any matching Modal's Chunk dataclass (asdict).
func chunkLocally(content []byte, rootID, filePath string) []map[string]any {
	text := string(content)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	ft := detectLocalFileType(filePath)
	if isCodeFile(filePath) {
		return chunkCode(text, rootID, filePath, ft)
	}
	return chunkMarkdown(text, rootID, filePath, ft)
}

const (
	textChunkChars   = 2400
	textOverlapChars = 400
	codeChunkChars   = 3000
	codeOverlapChars = 1000
)

// chunkCode uses a sliding window with line-boundary overlap.
func chunkCode(text, rootID, filePath, fileType string) []map[string]any {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 {
		return nil
	}

	var chunks []map[string]any
	start := 0
	idx := 0

	for start < len(lines) {
		end := start
		size := 0
		for end < len(lines) && (size == 0 || size+len(lines[end]) <= codeChunkChars) {
			size += len(lines[end])
			end++
		}
		piece := strings.Join(lines[start:end], "")
		if strings.TrimSpace(piece) != "" {
			lineEnd := end
			if end == len(lines) && lines[end-1] == "" {
				lineEnd--
			}
			chunks = append(chunks, makeChunkMap(rootID, filePath, idx, piece, fileType, start+1, lineEnd))
			idx++
		}
		if end >= len(lines) {
			break
		}
		overlapStart := end
		overlapSize := 0
		for overlapStart > start && overlapSize < codeOverlapChars {
			overlapStart--
			overlapSize += len(lines[overlapStart])
		}
		if overlapStart <= start {
			start = end
		} else {
			start = overlapStart
		}
	}
	return chunks
}

// chunkMarkdown splits by headings, then uses a boundary-aware sliding window with overlap.
func chunkMarkdown(text, rootID, filePath, fileType string) []map[string]any {
	sections := splitByHeadingsWithOffsets(text)
	var chunks []map[string]any
	idx := 0

	for _, section := range sections {
		for _, piece := range splitTextWithOverlapOffsets(section.text, section.start, textChunkChars, textOverlapChars) {
			if strings.TrimSpace(piece.text) != "" {
				lineStart, lineEnd := lineSpanForByteRange(text, piece.start, piece.end)
				chunks = append(chunks, makeChunkMap(rootID, filePath, idx, piece.text, fileType, lineStart, lineEnd))
				idx++
			}
		}
	}
	return chunks
}

var headingRE = regexp.MustCompile(`(?m)^#{1,6}\s`)

type textSection struct {
	text  string
	start int
}

type textPiece struct {
	text  string
	start int
	end   int
}

func splitByHeadings(text string) []string {
	sections := splitByHeadingsWithOffsets(text)
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		out = append(out, section.text)
	}
	return out
}

func splitByHeadingsWithOffsets(text string) []textSection {
	locs := headingRE.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []textSection{{text: text, start: 0}}
	}
	var sections []textSection
	if locs[0][0] > 0 {
		sections = append(sections, textSection{text: text[:locs[0][0]], start: 0})
	}
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections = append(sections, textSection{text: text[loc[0]:end], start: loc[0]})
	}
	return sections
}

func splitTextWithOverlap(text string, targetChars, overlapChars int) []string {
	pieces := splitTextWithOverlapOffsets(text, 0, targetChars, overlapChars)
	out := make([]string, 0, len(pieces))
	for _, piece := range pieces {
		out = append(out, piece.text)
	}
	return out
}

func splitTextWithOverlapOffsets(text string, baseOffset, targetChars, overlapChars int) []textPiece {
	if len(text) <= targetChars {
		return []textPiece{{text: text, start: baseOffset, end: baseOffset + len(text)}}
	}

	var pieces []textPiece
	start := 0
	for start < len(text) {
		end := start + targetChars
		if end >= len(text) {
			end = len(text)
		} else {
			end = bestTextBoundary(text, start, end, targetChars/2)
		}
		pieces = append(pieces, textPiece{text: text[start:end], start: baseOffset + start, end: baseOffset + end})
		if end >= len(text) {
			break
		}
		nextStart := end - overlapChars
		if nextStart <= start {
			nextStart = end
		}
		start = nextStart
	}
	return pieces
}

func lineSpanForByteRange(text string, start, end int) (int, int) {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	lineStart := 1 + strings.Count(text[:start], "\n")
	lineEnd := lineStart
	if end > start {
		lineEnd = lineStart + strings.Count(text[start:end], "\n")
		if strings.HasSuffix(text[start:end], "\n") {
			lineEnd--
		}
	}
	if lineEnd < lineStart {
		lineEnd = lineStart
	}
	return lineStart, lineEnd
}

func bestTextBoundary(text string, start, hardEnd, minSize int) int {
	minEnd := start + minSize
	if minEnd >= hardEnd {
		return hardEnd
	}
	window := text[minEnd:hardEnd]
	for _, sep := range []string{"\n\n", "\n", ". ", " ", ""} {
		if sep == "" {
			break
		}
		if idx := strings.LastIndex(window, sep); idx >= 0 {
			return minEnd + idx + len(sep)
		}
	}
	return hardEnd
}

func makeChunkMap(rootID, filePath string, chunkIndex int, content, fileType string, lineStart, lineEnd int) map[string]any {
	return map[string]any{
		"id":           makeChunkID(rootID, filePath, chunkIndex),
		"root_id":      rootID,
		"file_path":    filePath,
		"chunk_index":  chunkIndex,
		"content":      content,
		"content_hash": hashContent(content),
		"file_type":    fileType,
		"line_start":   lineStart,
		"line_end":     lineEnd,
	}
}

func makeChunkID(rootID, filePath string, chunkIndex int) string {
	h := sha256.Sum256([]byte(rootID + ":" + filePath))
	return fmt.Sprintf("%x:%d", h[:8], chunkIndex)
}

func hashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// detectLocalFileType returns a file_type string matching the Python chunkers convention.
func detectLocalFileType(path string) string {
	if strings.EqualFold(extOf(path), ".svg") {
		return "svg"
	}
	ft := detectFileType(path)
	if ft != "auto" {
		return ft
	}
	if isCodeFile(path) {
		return codeFileType(path)
	}
	return "markdown"
}

var codeExtensions = map[string]string{
	".py": "python", ".js": "javascript", ".ts": "typescript", ".go": "go",
	".rs": "rust", ".java": "java", ".c": "c", ".cpp": "cpp", ".cc": "cpp",
	".cs": "csharp", ".rb": "ruby", ".php": "php", ".swift": "swift",
	".kt": "kotlin", ".scala": "scala", ".sh": "shell", ".bash": "bash",
	".lua": "lua", ".pl": "perl", ".r": "r", ".sql": "sql",
	".html": "html", ".css": "css", ".scss": "scss",
	".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".json": "json",
	".xml": "xml", ".proto": "proto", ".graphql": "graphql",
	".hcl": "hcl", ".tf": "terraform",
}

func isCodeFile(path string) bool {
	ext := strings.ToLower(extOf(path))
	_, ok := codeExtensions[ext]
	if ok {
		return true
	}
	base := strings.ToLower(baseName(path))
	switch base {
	case "dockerfile", "makefile":
		return true
	}
	return false
}

func codeFileType(path string) string {
	ext := strings.ToLower(extOf(path))
	if ft, ok := codeExtensions[ext]; ok {
		return ft
	}
	base := strings.ToLower(baseName(path))
	switch base {
	case "dockerfile":
		return "dockerfile"
	case "makefile":
		return "makefile"
	}
	return "text"
}

func extOf(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return path[i:]
}

func baseName(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return path
	}
	return path[i+1:]
}
