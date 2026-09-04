package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	localChunkStreamThreshold = 8 << 20
	localChunkReadBuffer      = 64 << 10
)

func (p *syncPipeline) chunkLocalSourceEach(ctx context.Context, key string, change models.FileChange, emit func(map[string]any) error) error {
	length := change.SourceLength
	if length <= 0 {
		length = change.Size
	}
	return p.chunkLocalSourceRangeEach(ctx, key, change, change.SourceOffset, length, 0, 1, emit)
}

func (p *syncPipeline) chunkLocalSourceRangeEach(ctx context.Context, key string, change models.FileChange, offset, length int64, chunkIndex, lineStart int, emit func(map[string]any) error) error {
	fileType := detectLocalFileType(change.Path)
	target, overlap := textChunkChars, textOverlapChars
	if isCodeFile(change.Path) {
		target, overlap = codeChunkChars, codeOverlapChars
	}
	pending := make([]byte, 0, localChunkReadBuffer+target)
	emitPiece := func(piece []byte) error {
		if len(bytes.TrimSpace(piece)) == 0 {
			return nil
		}
		content := string(piece)
		lineEnd := lineStart + bytes.Count(piece, []byte{'\n'})
		if piece[len(piece)-1] == '\n' {
			lineEnd--
		}
		if lineEnd < lineStart {
			lineEnd = lineStart
		}
		chunk := makeChunkMap(p.rootID, change.Path, chunkIndex, content, fileType, lineStart, lineEnd)
		chunkIndex++
		return emit(chunk)
	}
	drain := func(final bool) error {
		for len(pending) > target || (final && len(pending) > 0) {
			end := len(pending)
			if end > target {
				end = bestTextBoundary(string(pending[:target]), 0, target, target/2)
			}
			if err := emitPiece(pending[:end]); err != nil {
				return err
			}
			if end == len(pending) {
				pending = pending[:0]
				break
			}
			advance := end - overlap
			if advance <= 0 {
				advance = end
			}
			for advance < len(pending) && !utf8.RuneStart(pending[advance]) {
				advance++
			}
			lineStart += bytes.Count(pending[:advance], []byte{'\n'})
			copy(pending, pending[advance:])
			pending = pending[:len(pending)-advance]
		}
		return nil
	}
	body, err := p.server.s3.Open(ctx, key, offset, length)
	if err != nil {
		return fmt.Errorf("opening %s: %w", key, err)
	}
	defer body.Close()
	buf := make([]byte, localChunkReadBuffer)
	for {
		n, readErr := body.Read(buf)
		pending = append(pending, buf[:n]...)
		if err := drain(false); err != nil {
			return err
		}
		if readErr == io.EOF {
			return drain(true)
		}
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", key, readErr)
		}
	}
}
