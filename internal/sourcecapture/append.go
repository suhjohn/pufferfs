package sourcecapture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/pufferfs/pufferfs/pkg/models"
)

var ErrPrefixChanged = errors.New("source prefix changed; replacement capture required")

// HashVerifiedPrefix consumes and hashes the old extent, leaving source at the
// suffix. The returned digest can continue hashing new bytes without rereading
// the prefix. A changed prefix requires rewinding for a replacement capture.
func HashVerifiedPrefix(source io.Reader, capturedSize int64, previous models.SourceManifest) (hash.Hash, error) {
	if err := ValidateManifest(previous); err != nil {
		return nil, err
	}
	if capturedSize < previous.Size {
		return nil, ErrPrefixChanged
	}
	digest := sha256.New()
	if _, err := io.CopyN(digest, source, previous.Size); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrPrefixChanged
		}
		return nil, err
	}
	if "sha256:"+hex.EncodeToString(digest.Sum(nil)) != previous.ContentHash {
		return nil, ErrPrefixChanged
	}
	return digest, nil
}

// CaptureAppend verifies the previous bytes before writing any suffix. The
// caller supplies a fixed-extent reader of capturedSize bytes and an immutable
// upload/spool destination. It must discard a partial destination on error.
// This saves transfer, not the local reads needed to prove prefix identity.
func CaptureAppend(source io.Reader, capturedSize int64, previous models.SourceManifest, suffix io.Writer, destination models.SourceExtent) (models.SourceManifest, error) {
	if err := ValidateManifest(previous); err != nil {
		return models.SourceManifest{}, err
	}
	if capturedSize < previous.Size {
		return models.SourceManifest{}, ErrPrefixChanged
	}
	newBytes := capturedSize - previous.Size
	if newBytes > 0 && (suffix == nil || destination.ObjectKey == "" || destination.Offset < 0 || destination.Offset > (1<<63-1)-newBytes) {
		return models.SourceManifest{}, fmt.Errorf("invalid append destination")
	}
	digest, err := HashVerifiedPrefix(source, capturedSize, previous)
	if err != nil {
		return models.SourceManifest{}, err
	}
	manifest := previous
	manifest.Extents = append([]models.SourceExtent(nil), previous.Extents...)
	if newBytes == 0 {
		return manifest, nil
	}
	if _, err := io.CopyN(io.MultiWriter(suffix, digest), source, newBytes); err != nil {
		return models.SourceManifest{}, fmt.Errorf("capturing append: %w", err)
	}
	destination.Length = newBytes
	manifest.Extents = append(manifest.Extents, destination)
	manifest.Size = capturedSize
	manifest.ContentHash = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	return manifest, nil
}
