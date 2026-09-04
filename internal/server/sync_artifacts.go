package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	defaultSyncArtifactPartRecords = 512
	defaultSyncArtifactPartBytes   = 8 << 20
)

type syncArtifactManifest struct {
	Version int      `json:"version"`
	Refs    []string `json:"refs"`
	Records int      `json:"records"`
	Bytes   int64    `json:"bytes"`
}

type syncArtifactWriter struct {
	pipeline *syncPipeline
	ctx      context.Context
	dir      string
	name     string
	buf      bytes.Buffer
	refs     []string
	records  int
	bytes    int64
	partRows int
}

func newSyncArtifactWriter(ctx context.Context, p *syncPipeline, dir, name string) *syncArtifactWriter {
	return &syncArtifactWriter{pipeline: p, ctx: ctx, dir: dir, name: safeObjectName(name)}
}

func (w *syncArtifactWriter) Append(value any) error {
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if w.partRows > 0 && (w.partRows >= syncArtifactPartRecords() || w.buf.Len()+len(line) > syncArtifactPartBytes()) {
		if err := w.flush(); err != nil {
			return err
		}
	}
	_, _ = w.buf.Write(line)
	w.partRows++
	w.records++
	w.bytes += int64(len(line))
	return nil
}

func (w *syncArtifactWriter) Close(ctx context.Context) (string, error) {
	if err := w.flush(); err != nil {
		return "", err
	}
	if len(w.refs) == 1 {
		return w.refs[0], nil
	}
	manifest := syncArtifactManifest{Version: 1, Refs: w.refs, Records: w.records, Bytes: w.bytes}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("syncs/%s/%s/%s.manifest.json", w.pipeline.generation.ID, w.dir, w.name)
	if err := w.pipeline.server.s3.Upload(ctx, key, data, "application/json"); err != nil {
		return "", fmt.Errorf("uploading artifact manifest %s: %w", key, err)
	}
	return key, nil
}

func (w *syncArtifactWriter) flush() error {
	if w.partRows == 0 {
		return nil
	}
	key := fmt.Sprintf("syncs/%s/%s/%s.part-%06d.jsonl", w.pipeline.generation.ID, w.dir, w.name, len(w.refs))
	if err := w.pipeline.server.s3.UploadStream(w.ctx, key, bytes.NewReader(w.buf.Bytes()), "application/x-ndjson"); err != nil {
		return fmt.Errorf("uploading artifact part %s: %w", key, err)
	}
	w.refs = append(w.refs, key)
	w.buf.Reset()
	w.partRows = 0
	return nil
}

func (p *syncPipeline) artifactRefs(ctx context.Context, key string) ([]string, error) {
	if !strings.HasSuffix(key, ".manifest.json") {
		return []string{key}, nil
	}
	data, err := p.server.s3.Download(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("downloading artifact manifest %s: %w", key, err)
	}
	var manifest syncArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parsing artifact manifest %s: %w", key, err)
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("unsupported artifact manifest version %d in %s", manifest.Version, key)
	}
	return manifest.Refs, nil
}

func (p *syncPipeline) forEachJSONL(ctx context.Context, key string, visit func(json.RawMessage) error) error {
	refs, err := p.artifactRefs(ctx, key)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		data, err := p.server.s3.Download(ctx, ref)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", ref, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("decoding %s: %w", ref, err)
			}
			if err := visit(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncArtifactPartRecords() int {
	return boundedArtifactSetting("PUFFERFS_SYNC_ARTIFACT_PART_RECORDS", defaultSyncArtifactPartRecords, 1, 10000)
}

func syncArtifactPartBytes() int {
	return boundedArtifactSetting("PUFFERFS_SYNC_ARTIFACT_PART_BYTES", defaultSyncArtifactPartBytes, 64<<10, 64<<20)
}

func boundedArtifactSetting(name string, fallback, minimum, maximum int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
