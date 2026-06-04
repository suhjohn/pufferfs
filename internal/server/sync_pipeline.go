package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	syncStageChunk = "chunk"
	syncStageEmbed = "embed"
	syncStageIndex = "index"
)

type syncPipeline struct {
	server     *Server
	orgID      string
	rootID     string
	generation *SyncGeneration
	job        *models.SyncJob
	req        *models.SyncRequest
	broker     *objectQueueBroker
	resp       *models.SyncResponse
}

type syncChunkArtifact struct {
	Op     string            `json:"op"`
	Change models.FileChange `json:"change"`
	Chunk  map[string]any    `json:"chunk,omitempty"`
	Row    map[string]any    `json:"row,omitempty"`
}

type syncIndexArtifact struct {
	Op        string         `json:"op"`
	Row       map[string]any `json:"row,omitempty"`
	ClosePath string         `json:"close_path,omitempty"`
}

func (s *Server) processSync(ctx context.Context, orgID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	p := &syncPipeline{
		server:     s,
		orgID:      orgID,
		rootID:     req.RootID,
		generation: generation,
		job:        job,
		req:        req,
		broker:     newObjectQueueBroker(s.s3),
		resp: &models.SyncResponse{
			RootID:    req.RootID,
			SyncJobID: syncJobIdentifier(job),
		},
	}
	return p.run(ctx)
}

func (p *syncPipeline) run(ctx context.Context) (*models.SyncResponse, error) {
	if err := p.prepareInputJobs(ctx); err != nil {
		return nil, err
	}
	if err := p.runChunkStage(ctx); err != nil {
		return nil, err
	}
	if err := p.runEmbedStage(ctx); err != nil {
		return nil, err
	}
	if err := p.runIndexStage(ctx); err != nil {
		return nil, err
	}
	return p.resp, nil
}

func (p *syncPipeline) prepareInputJobs(ctx context.Context) error {
	var batch []models.FileChange
	for _, change := range p.req.Changes {
		if change.Status == models.StatusUnchanged {
			continue
		}
		batch = append(batch, change)
	}
	if len(batch) == 0 {
		return nil
	}
	ref, err := p.writeJSONL(ctx, "inputs", "batch-000001", batch)
	if err != nil {
		return err
	}
	job := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageChunk, ref)
	return p.broker.Push(ctx, p.generation.ID, syncStageChunk, job)
}

func (p *syncPipeline) runChunkStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "chunking", p.resp.FilesProcessed)
	}
	sourceCache := newSyncSourceCache(p.server.s3)
	for {
		jobs, err := p.broker.Claim(ctx, p.generation.ID, syncStageChunk, "chunk-worker", syncWorkerCount(), 5*time.Minute)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return p.ensureStageComplete(ctx, syncStageChunk)
		}
		for _, job := range jobs {
			resultRef, err := p.processChunkJob(ctx, job, sourceCache)
			if err != nil {
				_ = p.broker.Fail(ctx, p.generation.ID, syncStageChunk, job.JobID, err.Error(), 3)
				return err
			}
			next := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageEmbed, resultRef)
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageChunk, job.JobID, resultRef, next); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processChunkJob(ctx context.Context, job objectQueueJob, sourceCache *syncSourceCache) (string, error) {
	var changes []models.FileChange
	if err := p.readJSONL(ctx, job.PayloadRef, &changes); err != nil {
		return "", err
	}
	workers := syncWorkerCount()
	sem := make(chan struct{}, workers)
	results := make([][]syncChunkArtifact, len(changes))
	errs := make([]error, len(changes))
	var wg sync.WaitGroup
	for i, change := range changes {
		i, change := i, change
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows, err := p.chunkChange(ctx, change, sourceCache)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = rows
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return "", fmt.Errorf("chunking %s: %w", changes[i].Path, err)
		}
	}
	var artifact []syncChunkArtifact
	for _, rows := range results {
		artifact = append(artifact, rows...)
	}
	if len(artifact) == 0 {
		artifact = append(artifact, syncChunkArtifact{Op: "noop"})
	}
	return p.writeJSONL(ctx, "chunks", job.JobID, artifact)
}

func (p *syncPipeline) chunkChange(ctx context.Context, change models.FileChange, sourceCache *syncSourceCache) ([]syncChunkArtifact, error) {
	switch change.Status {
	case models.StatusAdded, models.StatusModified:
		s3Key := change.SourceKey
		if s3Key == "" {
			s3Key = fmt.Sprintf("files/%s/%s", p.rootID, change.Path)
		}
		fileData, err := sourceCache.read(ctx, s3Key, change.SourceOffset, change.SourceLength)
		if err != nil {
			return nil, fmt.Errorf("downloading %s: %w", s3Key, err)
		}
		chunkResp, err := p.server.modal.ChunkFile(ChunkFileRequest{
			S3Key:      s3Key,
			FilePath:   change.Path,
			FileType:   detectFileType(change.Path),
			RootID:     p.rootID,
			ContentB64: base64.StdEncoding.EncodeToString(fileData),
		})
		if err != nil {
			return nil, err
		}
		rows := make([]syncChunkArtifact, 0, len(chunkResp.Chunks)+1)
		if change.Status == models.StatusModified {
			rows = append(rows, syncChunkArtifact{Op: "close", Change: change})
		}
		for _, chunk := range chunkResp.Chunks {
			rows = append(rows, syncChunkArtifact{Op: "chunk", Change: change, Chunk: chunk})
		}
		return rows, nil
	case models.StatusRemoved:
		return []syncChunkArtifact{{Op: "close", Change: change}}, nil
	case models.StatusMoved, models.StatusRenamed:
		rows, err := p.queryActiveRows(ctx, change.OldPath, []string{"content", "file_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path"})
		if err != nil {
			return nil, err
		}
		out := []syncChunkArtifact{{Op: "close", Change: models.FileChange{Path: change.OldPath, Status: models.StatusRemoved}}}
		for i, row := range rows {
			chunk := indexedChunkFromExisting(p.rootID, p.generation.ID, p.generation.Seq, change.Path, change.ContentHash, intFromAny(row["chunk_index"], i), row)
			out = append(out, syncChunkArtifact{Op: "row", Change: change, Row: chunk.mapRow()})
		}
		return out, nil
	default:
		return nil, nil
	}
}

func (p *syncPipeline) runEmbedStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "embedding", p.resp.FilesProcessed)
	}
	for {
		jobs, err := p.broker.Claim(ctx, p.generation.ID, syncStageEmbed, "embed-worker", syncWorkerCount(), 10*time.Minute)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return p.ensureStageComplete(ctx, syncStageEmbed)
		}
		for _, job := range jobs {
			resultRef, err := p.processEmbedJob(ctx, job)
			if err != nil {
				_ = p.broker.Fail(ctx, p.generation.ID, syncStageEmbed, job.JobID, err.Error(), 3)
				return err
			}
			next := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageIndex, resultRef)
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageEmbed, job.JobID, resultRef, next); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processEmbedJob(ctx context.Context, job objectQueueJob) (string, error) {
	var chunks []syncChunkArtifact
	if err := p.readJSONL(ctx, job.PayloadRef, &chunks); err != nil {
		return "", err
	}
	var indexRows []syncIndexArtifact
	var pending []pendingEmbedding
	var contentHashes []string
	for _, item := range chunks {
		switch item.Op {
		case "close":
			indexRows = append(indexRows, syncIndexArtifact{Op: "close", ClosePath: item.Change.Path})
		case "row":
			hash := strVal(item.Row, "content_hash")
			contentHashes = append(contentHashes, hash)
			indexRows = append(indexRows, syncIndexArtifact{Op: "upsert", Row: item.Row})
		case "chunk":
			row := indexedChunkFromModal(p.rootID, p.generation.ID, p.generation.Seq, item.Change.ContentHash, item.Chunk).mapRow()
			hash := strVal(row, "content_hash")
			contentHashes = append(contentHashes, hash)
			indexRows = append(indexRows, syncIndexArtifact{Op: "upsert", Row: row})
		}
	}
	cached, err := p.server.db.GetCachedEmbeddings(ctx, p.orgID, contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup failed: %v", err)
		cached = map[string][]float64{}
	}
	for i := range indexRows {
		if indexRows[i].Op != "upsert" {
			continue
		}
		hash := strVal(indexRows[i].Row, "content_hash")
		if emb, ok := cached[hash]; ok {
			indexRows[i].Row["vector"] = emb
			continue
		}
		pending = append(pending, pendingEmbedding{
			chunk:       modalChunkPayload(indexRows[i].Row),
			row:         indexRows[i].Row,
			contentHash: hash,
		})
	}
	if len(pending) > 0 {
		if err := p.server.resolvePendingEmbeddings(ctx, p.orgID, pending); err != nil {
			return "", err
		}
	}
	return p.writeJSONL(ctx, "index_rows", job.JobID, indexRows)
}

func (p *syncPipeline) runIndexStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "upserting", p.resp.FilesProcessed)
	}
	for {
		jobs, err := p.broker.Claim(ctx, p.generation.ID, syncStageIndex, "index-worker", 1, 10*time.Minute)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return p.ensureStageComplete(ctx, syncStageIndex)
		}
		for _, job := range jobs {
			if err := p.processIndexJob(ctx, job); err != nil {
				_ = p.broker.Fail(ctx, p.generation.ID, syncStageIndex, job.JobID, err.Error(), 3)
				return err
			}
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageIndex, job.JobID, job.PayloadRef); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processIndexJob(ctx context.Context, job objectQueueJob) error {
	var records []syncIndexArtifact
	if err := p.readJSONL(ctx, job.PayloadRef, &records); err != nil {
		return err
	}
	var upserts []map[string]any
	closePaths := make(map[string]bool)
	processedPaths := make(map[string]bool)
	for _, record := range records {
		switch record.Op {
		case "upsert":
			if record.Row != nil {
				upserts = append(upserts, record.Row)
				if path := strVal(record.Row, "file_path"); path != "" {
					processedPaths[path] = true
				}
			}
		case "close":
			if record.ClosePath != "" {
				closePaths[record.ClosePath] = true
				processedPaths[record.ClosePath] = true
			}
		}
	}
	ns := tpNamespace(p.orgID, p.rootID)
	if len(upserts) > 0 {
		if err := p.server.writeIndexRowsArtifact(ctx, p.generation.ID, "index-stage", upserts); err != nil {
			return err
		}
		if err := p.server.upsertRowsInBatches(ns, upserts); err != nil {
			return err
		}
		p.resp.ChunksAdded += len(upserts)
	}
	for path := range closePaths {
		closeRows, err := p.closeRowsForPath(ctx, path)
		if err != nil {
			return err
		}
		if len(closeRows) > 0 {
			if err := p.server.patchRowsInBatches(ns, closeRows); err != nil {
				return err
			}
			p.resp.ChunksRemoved += len(closeRows)
		}
	}
	p.resp.FilesProcessed += len(processedPaths)
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "upserting", p.resp.FilesProcessed)
	}
	return nil
}

func (p *syncPipeline) closeRowsForPath(ctx context.Context, path string) ([]map[string]any, error) {
	rows, err := p.queryActiveRows(ctx, path, []string{"id"})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		id := fmt.Sprintf("%v", row["id"])
		if id == "" || id == "<nil>" {
			continue
		}
		out = append(out, map[string]any{
			"id":                      id,
			"valid_to_generation":     p.generation.ID,
			"valid_to_generation_seq": p.generation.Seq,
		})
	}
	return out, nil
}

func (p *syncPipeline) queryActiveRows(ctx context.Context, path string, attrs []string) ([]map[string]any, error) {
	filters := []any{
		[]any{"file_path", "Eq", path},
	}
	if p.generation.BaseGenerationSeq > 0 {
		filters = append(filters, activeGenerationFilter(p.generation.BaseGenerationSeq))
	}
	return p.server.tp.Query(tpNamespace(p.orgID, p.rootID), []any{"file_path", "asc"}, 10000, tpAndFilter(filters), attrs)
}

func (p *syncPipeline) ensureStageComplete(ctx context.Context, stage string) error {
	summary, err := p.broker.Summary(ctx, p.generation.ID, stage)
	if err != nil {
		return err
	}
	if summary.Failed > 0 {
		return fmt.Errorf("%s stage has %d failed jobs", stage, summary.Failed)
	}
	if summary.Queued > 0 || summary.Running > 0 {
		return fmt.Errorf("%s stage incomplete: queued=%d running=%d", stage, summary.Queued, summary.Running)
	}
	return nil
}

func (p *syncPipeline) writeJSONL(ctx context.Context, dir, name string, value any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	switch items := value.(type) {
	case []models.FileChange:
		for _, item := range items {
			if err := enc.Encode(item); err != nil {
				return "", err
			}
		}
	case []syncChunkArtifact:
		for _, item := range items {
			if err := enc.Encode(item); err != nil {
				return "", err
			}
		}
	case []syncIndexArtifact:
		for _, item := range items {
			if err := enc.Encode(item); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("unsupported jsonl payload type %T", value)
	}
	key := fmt.Sprintf("syncs/%s/%s/%s.jsonl", p.generation.ID, dir, safeObjectName(name))
	if err := p.server.s3.Upload(ctx, key, buf.Bytes(), "application/x-ndjson"); err != nil {
		return "", fmt.Errorf("uploading %s: %w", key, err)
	}
	return key, nil
}

func (p *syncPipeline) readJSONL(ctx context.Context, key string, out any) error {
	data, err := p.server.s3.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", key, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	switch dest := out.(type) {
	case *[]models.FileChange:
		for {
			var item models.FileChange
			if err := dec.Decode(&item); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			*dest = append(*dest, item)
		}
	case *[]syncChunkArtifact:
		for {
			var item syncChunkArtifact
			if err := dec.Decode(&item); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			*dest = append(*dest, item)
		}
	case *[]syncIndexArtifact:
		for {
			var item syncIndexArtifact
			if err := dec.Decode(&item); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			*dest = append(*dest, item)
		}
	default:
		return fmt.Errorf("unsupported jsonl destination %T", out)
	}
	return nil
}

func activeGenerationFilter(seq int64) any {
	return []any{
		"And",
		[]any{
			[]any{"valid_from_generation_seq", "Lte", seq},
			[]any{"Or", []any{
				[]any{"valid_to_generation_seq", "Eq", 0},
				[]any{"valid_to_generation_seq", "Gt", seq},
			}},
		},
	}
}

func intFromAny(value any, fallback int) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, err := strconv.Atoi(v.String())
		if err == nil {
			return n
		}
	}
	return fallback
}

func syncJobIdentifier(job *models.SyncJob) string {
	if job == nil {
		return ""
	}
	return job.ID
}
