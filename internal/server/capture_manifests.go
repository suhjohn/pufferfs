package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const maxManifestPackBytes = 16 << 20

type sourceManifestPack struct {
	key  string
	body []byte
	refs []string
}

// One immutable object per capture. Each locator binds a bounded byte range to
// its own checksum; readers need not download the other files' manifests.
func packSourceManifests(orgID, rootID string, files []models.CaptureFile) (sourceManifestPack, error) {
	pack := sourceManifestPack{refs: make([]string, len(files))}
	digests := make([]string, len(files))
	records := make(map[string][]byte)
	for i, file := range files {
		if file.Deleted {
			continue
		}
		raw, err := json.Marshal(file.Source)
		if err != nil {
			return pack, err
		}
		digests[i] = fmt.Sprintf("%x", sha256.Sum256(raw))
		records[digests[i]] = raw
	}
	if len(records) == 0 {
		return pack, nil
	}
	var body bytes.Buffer
	suffixes := make(map[string]string, len(records))
	// Sorting makes identical batches stable across request order and servers.
	// Deduplication also stores identical manifests (including empty files) once.
	for _, digest := range slices.Sorted(maps.Keys(records)) {
		raw := records[digest]
		if len(raw)+1 > maxManifestPackBytes-body.Len() {
			return pack, fmt.Errorf("source manifest pack exceeds 16 MiB")
		}
		suffixes[digest] = fmt.Sprintf("#%d:%d:%s", body.Len(), len(raw), digest)
		body.Write(raw)
		body.WriteByte('\n')
	}
	pack.body = body.Bytes()
	pack.key = fmt.Sprintf("sources/%s/%s/manifests/%x.jsonl", orgID, rootID, sha256.Sum256(pack.body))
	for i, digest := range digests {
		if digest != "" {
			pack.refs[i] = pack.key + suffixes[digest]
		}
	}
	return pack, nil
}

var packedManifestIdentity = regexp.MustCompile(`^(sources/[^/]+/[^/]+/manifests/)[0-9a-f]{64}\.jsonl#(?:0|[1-9][0-9]*):[1-9][0-9]*:([0-9a-f]{64})$`)

// Replay identity is independent of pack membership and byte position.
// This is equality, not read authorization or byte validation; source_io
// validates ownership, range bounds and checksums.
func sourceManifestIdentity(ref string) (string, error) {
	if ref == "" {
		return "", nil // Tombstone.
	}
	match := packedManifestIdentity.FindStringSubmatch(ref)
	if match == nil {
		return "", fmt.Errorf("source manifest requires a pack range and checksum")
	}
	return match[1] + match[2], nil
}
