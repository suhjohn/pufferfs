package server

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// localChunkable returns true if the file type can be chunked locally without Modal.
func localChunkable(filePath string) bool {
	ft := detectFileType(filePath)
	// PDF, DOCX, PPTX, images require Modal (Gemini vision, LibreOffice, PyMuPDF)
	switch ft {
	case "pdf", "docx", "pptx", "image":
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

// chunkCode splits by lines (300 lines/chunk, 50 overlap) — mirrors Python chunk_code.
func chunkCode(text, rootID, filePath, fileType string) []map[string]any {
	const chunkLines = 300
	const overlapLines = 50

	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 {
		return nil
	}

	var chunks []map[string]any
	start := 0
	idx := 0

	for start < len(lines) {
		end := start + chunkLines
		if end > len(lines) {
			end = len(lines)
		}
		piece := strings.Join(lines[start:end], "")
		if strings.TrimSpace(piece) != "" {
			chunks = append(chunks, makeChunkMap(rootID, filePath, idx, piece, fileType))
			idx++
		}
		if end >= len(lines) {
			break
		}
		start = end - overlapLines
	}
	return chunks
}

// chunkMarkdown splits by headings then by size — mirrors Python chunk_markdown.
func chunkMarkdown(text, rootID, filePath, fileType string) []map[string]any {
	const maxSectionChars = 2000
	const sectionOverlapChars = 200

	sections := splitByHeadings(text)
	var chunks []map[string]any
	idx := 0

	for _, section := range sections {
		for _, piece := range splitLarge(section, maxSectionChars, sectionOverlapChars) {
			if strings.TrimSpace(piece) != "" {
				chunks = append(chunks, makeChunkMap(rootID, filePath, idx, piece, fileType))
				idx++
			}
		}
	}
	return chunks
}

var headingRE = regexp.MustCompile(`(?m)^#{1,6}\s`)

func splitByHeadings(text string) []string {
	locs := headingRE.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
	var sections []string
	if locs[0][0] > 0 {
		sections = append(sections, text[:locs[0][0]])
	}
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections = append(sections, text[loc[0]:end])
	}
	return sections
}

func splitLarge(text string, maxChars, overlap int) []string {
	if len(text) <= maxChars {
		return []string{text}
	}
	var pieces []string
	start := 0
	for start < len(text) {
		end := start + maxChars
		if end > len(text) {
			end = len(text)
		}
		pieces = append(pieces, text[start:end])
		if end >= len(text) {
			break
		}
		start = end - overlap
	}
	return pieces
}

func makeChunkMap(rootID, filePath string, chunkIndex int, content, fileType string) map[string]any {
	return map[string]any{
		"id":           makeChunkID(rootID, filePath, chunkIndex),
		"root_id":      rootID,
		"file_path":    filePath,
		"chunk_index":  chunkIndex,
		"content":      content,
		"content_hash": hashContent(content),
		"file_type":    fileType,
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
