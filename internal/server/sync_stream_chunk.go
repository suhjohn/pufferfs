package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	defaultLocalChunkStreamThreshold = 8 << 20
	localChunkReadBlock              = 4 << 20
)

func localChunkStreamThreshold() int64 {
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_SYNC_LOCAL_STREAM_THRESHOLD_BYTES"))
	if raw == "" {
		return defaultLocalChunkStreamThreshold
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 64<<10 {
		return defaultLocalChunkStreamThreshold
	}
	return value
}

func (p *syncPipeline) chunkLocalSourceEach(ctx context.Context, key string, change models.FileChange, emit func(map[string]any) error) error {
	length := change.SourceLength
	if length <= 0 {
		length = change.Size
	}
	if length <= localChunkStreamThreshold() {
		return nil
	}
	fileType := detectLocalFileType(change.Path)
	pending := make([]byte, 0, localChunkReadBlock+textChunkChars)
	lineStart := 1
	chunkIndex := 0
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
		for len(pending) > textChunkChars || (final && len(pending) > 0) {
			end := len(pending)
			if end > textChunkChars {
				end = bestTextBoundary(string(pending[:textChunkChars]), 0, textChunkChars, textChunkChars/2)
			}
			if err := emitPiece(pending[:end]); err != nil {
				return err
			}
			if end == len(pending) {
				pending = pending[:0]
				break
			}
			advance := end - textOverlapChars
			if advance <= 0 {
				advance = end
			}
			for advance < len(pending) && !utf8.RuneStart(pending[advance]) {
				advance++
			}
			lineStart += bytes.Count(pending[:advance], []byte{'\n'})
			pending = append([]byte(nil), pending[advance:]...)
		}
		return nil
	}
	for read := int64(0); read < length; {
		block := int64(localChunkReadBlock)
		if remaining := length - read; remaining < block {
			block = remaining
		}
		data, err := p.server.s3.DownloadRange(ctx, key, change.SourceOffset+read, block)
		if err != nil {
			return fmt.Errorf("downloading %s range offset=%d length=%d: %w", key, change.SourceOffset+read, block, err)
		}
		if int64(len(data)) != block {
			return fmt.Errorf("downloading %s range offset=%d: got %d bytes, want %d", key, change.SourceOffset+read, len(data), block)
		}
		pending = append(pending, data...)
		if err := drain(false); err != nil {
			return err
		}
		read += block
	}
	return drain(true)
}
