package sourcecapture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
