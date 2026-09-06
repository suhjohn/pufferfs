package sourcecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/pufferfs/pufferfs/pkg/models"
)

// ValidateManifest checks representation only. Authorization of object keys is
// a separate check at registration; a content hash is never an access token.
func ValidateManifest(m models.SourceManifest) error {
	if m.Format != 1 || m.Size < 0 {
		return fmt.Errorf("unsupported or invalid source manifest")
	}
	digest := strings.TrimPrefix(m.ContentHash, "sha256:")
	if len(digest) != 64 || m.ContentHash != "sha256:"+strings.ToLower(digest) {
		return fmt.Errorf("source manifest requires a canonical SHA-256")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("invalid source digest: %w", err)
	}
	remaining := m.Size
	for _, extent := range m.Extents {
		if extent.ObjectKey == "" || extent.Offset < 0 || extent.Length <= 0 ||
			extent.Length > remaining || extent.Offset > (1<<63-1)-extent.Length {
			return fmt.Errorf("invalid source extent")
		}
		remaining -= extent.Length
	}
	if remaining != 0 {
		return fmt.Errorf("source extents do not cover declared size")
	}
	return nil
}

type RangeOpener func(context.Context, string, int64, int64) (io.ReadCloser, error)

// OpenManifest opens extents lazily and verifies the complete captured byte
// sequence at EOF. Closing early does not imply successful verification.
func OpenManifest(ctx context.Context, manifest models.SourceManifest, open RangeOpener) (io.ReadCloser, error) {
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	if open == nil {
		return nil, fmt.Errorf("source range opener is required")
	}
	return &manifestReader{ctx: ctx, manifest: manifest, open: open, digest: sha256.New()}, nil
}

type manifestReader struct {
	ctx       context.Context
	manifest  models.SourceManifest
	open      RangeOpener
	digest    hash.Hash
	index     int
	current   io.ReadCloser
	remaining int64
	closed    bool
	terminal  error
}

func (r *manifestReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fmt.Errorf("source reader is closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	for {
		if err := r.ctx.Err(); err != nil {
			r.terminal = err
			return 0, err
		}
		if r.current == nil {
			if r.index == len(r.manifest.Extents) {
				r.terminal = io.EOF
				if "sha256:"+hex.EncodeToString(r.digest.Sum(nil)) != r.manifest.ContentHash {
					r.terminal = fmt.Errorf("captured source hash mismatch")
				}
				return 0, r.terminal
			}
			e := r.manifest.Extents[r.index]
			var err error
			r.current, err = r.open(r.ctx, e.ObjectKey, e.Offset, e.Length)
			if err != nil {
				r.terminal = err
				return 0, err
			}
			r.index++
			r.remaining = e.Length
		}
		n, err := r.current.Read(p[:min(int64(len(p)), r.remaining)])
		r.remaining -= int64(n)
		_, _ = r.digest.Write(p[:n])
		if err == io.EOF && r.remaining > 0 {
			err = io.ErrUnexpectedEOF
		}
		if r.remaining == 0 {
			closeErr := r.current.Close()
			r.current = nil
			if err == io.EOF {
				err = nil
			}
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			r.terminal = err
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (r *manifestReader) Close() error {
	r.closed = true
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}
