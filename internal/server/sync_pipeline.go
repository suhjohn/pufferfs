package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	syncStageChunk  = "chunk"
	syncStageEmbed  = "embed"
	syncStageIndex  = "index"
	syncStageCommit = "commit"

	defaultSyncShardMaxFiles     = 128
	defaultSyncShardMaxBytes     = 32 * 1024 * 1024
	defaultSyncShardMaxChunks    = 8192
	defaultSyncMaxInFlightShards = 32
	activeRowsQueryLimit         = 10000
)

type syncPipeline struct {
	server     *Server
	orgID      string
	rootID     string
	generation *SyncGeneration
	job        *models.SyncJob
	userID     string
	req        *models.SyncRequest
	broker     *objectQueueBroker
	resp       *models.SyncResponse

	indexNamespaces       []models.RootIndexNamespace
	indexNamespacesLoaded bool
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

type syncInputShard struct {
	Ref       string
	FileCount int
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
			RootID:        req.RootID,
			SyncJobID:     syncJobIdentifier(job),
			GenerationID:  generation.ID,
			GenerationSeq: generation.Seq,
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

func (s *Server) enqueueSync(ctx context.Context, orgID, userID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	p := &syncPipeline{
		server:     s,
		orgID:      orgID,
		rootID:     req.RootID,
		generation: generation,
		job:        job,
		userID:     userID,
		req:        req,
		resp: &models.SyncResponse{
			RootID:        req.RootID,
			SyncJobID:     syncJobIdentifier(job),
			GenerationID:  generation.ID,
			GenerationSeq: generation.Seq,
		},
	}
	if err := p.writeRequest(ctx); err != nil {
		return nil, err
	}
	msgs, err := p.prepareQueueJobs(ctx)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		if err := p.enqueueCommit(ctx, 0); err != nil {
			return nil, err
		}
		return p.resp, nil
	}
	if err := s.queue.Enqueue(ctx, syncStageChunk, initialChunkShardMessages(msgs)...); err != nil {
		return nil, err
	}
	if job != nil {
		_ = s.db.UpdateSyncJobStatus(ctx, job.ID, "queued")
	}
	return p.resp, nil
}

func (p *syncPipeline) writeRequest(ctx context.Context) error {
	data, err := json.Marshal(p.req)
	if err != nil {
		return err
	}
	return p.server.s3.Upload(ctx, syncRequestKey(p.generation.ID), data, "application/json")
}

func (p *syncPipeline) prepareQueueJobs(ctx context.Context) ([]queue.JobMessage, error) {
	if _, err := p.loadIndexNamespaces(ctx); err != nil {
		return nil, err
	}
	shards, err := p.inputShards(ctx)
	if err != nil {
		return nil, err
	}
	msgs := make([]queue.JobMessage, 0, len(shards))
	for i, shard := range shards {
		msgs = append(msgs, p.jobMessage(syncStageChunk, chunkShardJobID(p.generation.ID, i), shard.Ref, i, len(shards), shard.FileCount))
	}
	return msgs, nil
}

func (p *syncPipeline) enqueueCommit(ctx context.Context, totalShards int) error {
	msg := p.jobMessage(syncStageCommit, uuid.NewString(), syncRequestKey(p.generation.ID), 0, totalShards, 0)
	return p.server.queue.Enqueue(ctx, syncStageCommit, msg)
}

func (p *syncPipeline) jobMessage(stage, jobID, payloadRef string, shardIndex, totalShards, filesInShard int) queue.JobMessage {
	return queue.JobMessage{
		JobID:             jobID,
		SyncJobID:         syncJobIdentifier(p.job),
		UserID:            p.userID,
		OrgID:             p.orgID,
		RootID:            p.rootID,
		GenerationID:      p.generation.ID,
		GenerationSeq:     p.generation.Seq,
		BaseGenerationID:  p.generation.BaseGenerationID,
		BaseGenerationSeq: p.generation.BaseGenerationSeq,
		Stage:             stage,
		PayloadRef:        payloadRef,
		IndexNamespaces:   queueIndexNamespaces(p.indexNamespaces),
		ShardIndex:        shardIndex,
		TotalShards:       totalShards,
		FilesInShard:      filesInShard,
		DisableVector:     p.req != nil && p.req.DisableVector,
		EnqueuedAt:        time.Now().UTC(),
	}
}

func (p *syncPipeline) loadIndexNamespaces(ctx context.Context) ([]models.RootIndexNamespace, error) {
	if p.indexNamespacesLoaded {
		return p.indexNamespaces, nil
	}
	if len(p.indexNamespaces) == 0 {
		namespaces, err := p.server.db.ListRootIndexNamespaces(ctx, p.orgID, p.rootID)
		if err != nil {
			return nil, err
		}
		p.indexNamespaces = namespaces
	}
	p.indexNamespacesLoaded = true
	return p.indexNamespaces, nil
}

func shardChanges(changes []models.FileChange, maxFiles int, maxBytes int64) [][]models.FileChange {
	return shardChangesByWork(changes, maxFiles, maxBytes, defaultSyncShardMaxChunks)
}

func shardChangesByWork(changes []models.FileChange, maxFiles int, maxBytes, maxChunks int64) [][]models.FileChange {
	var shards [][]models.FileChange
	var current []models.FileChange
	var currentBytes, currentChunks int64
	for _, change := range changes {
		if change.Status == models.StatusUnchanged {
			continue
		}
		size := change.SourceLength
		if size <= 0 {
			size = change.Size
		}
		if size < 0 {
			size = 0
		}
		chunks := estimatedServerChangeChunks(change, size)
		if len(current) > 0 && (len(current) >= maxFiles || currentBytes+size > maxBytes || currentChunks+chunks > maxChunks) {
			shards = append(shards, current)
			current = nil
			currentBytes = 0
			currentChunks = 0
		}
		current = append(current, change)
		currentBytes += size
		currentChunks += chunks
	}
	if len(current) > 0 {
		shards = append(shards, current)
	}
	return shards
}

func estimatedServerChangeChunks(change models.FileChange, size int64) int64 {
	if change.Status != models.StatusAdded && change.Status != models.StatusModified {
		return 1
	}
	if size <= 0 {
		return 1
	}
	return (size + 1999) / 2000
}

func syncRequestKey(generationID string) string {
	return fmt.Sprintf("syncs/%s/request.json", generationID)
}

func syncInputShardKey(generationID string, shardIndex int) string {
	return fmt.Sprintf("syncs/%s/inputs/shard-%06d.jsonl", generationID, shardIndex)
}

func syncManifestShardKey(generationID string, shardIndex int) string {
	return fmt.Sprintf("syncs/%s/manifests/%06d.jsonl", generationID, shardIndex)
}

func chunkShardJobID(generationID string, shardIndex int) string {
	return fmt.Sprintf("%s-chunk-%06d", generationID, shardIndex)
}

func syncMaxInFlightShards() int {
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_SYNC_MAX_IN_FLIGHT_SHARDS"))
	if raw == "" {
		return defaultSyncMaxInFlightShards
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultSyncMaxInFlightShards
	}
	if n > 1024 {
		return 1024
	}
	return n
}

func initialChunkShardMessages(msgs []queue.JobMessage) []queue.JobMessage {
	limit := syncMaxInFlightShards()
	if len(msgs) <= limit {
		return msgs
	}
	return msgs[:limit]
}

func nextChunkShardMessage(msg queue.JobMessage) (queue.JobMessage, bool) {
	nextIndex := msg.ShardIndex + syncMaxInFlightShards()
	if msg.TotalShards <= 0 || nextIndex >= msg.TotalShards {
		return queue.JobMessage{}, false
	}
	next := msg
	next.JobID = chunkShardJobID(msg.GenerationID, nextIndex)
	next.Stage = syncStageChunk
	if strings.Contains(msg.PayloadRef, "/inputs/") {
		next.PayloadRef = syncInputShardKey(msg.GenerationID, nextIndex)
	} else {
		next.PayloadRef = syncManifestShardKey(msg.GenerationID, nextIndex)
	}
	next.CleanupKeys = nil
	next.ShardIndex = nextIndex
	next.FilesInShard = 0
	next.EnqueuedAt = time.Now().UTC()
	return next, true
}

func (p *syncPipeline) prepareInputJobs(ctx context.Context) error {
	shards, err := p.inputShards(ctx)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return nil
	}
	jobs := make([]objectQueueJob, 0, len(shards))
	for i, shard := range shards {
		job := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageChunk, shard.Ref, i, len(shards), shard.FileCount)
		job.JobID = chunkShardJobID(p.generation.ID, i)
		jobs = append(jobs, job)
	}
	return p.broker.Push(ctx, p.generation.ID, syncStageChunk, jobs...)
}

func (p *syncPipeline) inputShards(ctx context.Context) ([]syncInputShard, error) {
	if len(p.req.ChangeRefs) > 0 {
		var shards []syncInputShard
		var current []models.FileChange
		var currentBytes, currentChunks int64
		flush := func() error {
			if len(current) == 0 {
				return nil
			}
			ref, err := p.writeJSONL(ctx, "inputs", fmt.Sprintf("shard-%06d", len(shards)), current)
			if err != nil {
				return err
			}
			shards = append(shards, syncInputShard{Ref: ref, FileCount: len(current)})
			current = nil
			currentBytes = 0
			currentChunks = 0
			return nil
		}
		for _, ref := range p.req.ChangeRefs {
			if ref == "" {
				continue
			}
			if err := p.forEachJSONL(ctx, ref, func(raw json.RawMessage) error {
				var change models.FileChange
				if err := json.Unmarshal(raw, &change); err != nil {
					return err
				}
				if change.Status != models.StatusUnchanged {
					size := change.SourceLength
					if size <= 0 {
						size = change.Size
					}
					if size < 0 {
						size = 0
					}
					work := estimatedServerChangeChunks(change, size)
					if len(current) > 0 && (len(current) >= defaultSyncShardMaxFiles || currentBytes+size > defaultSyncShardMaxBytes || currentChunks+work > defaultSyncShardMaxChunks) {
						if err := flush(); err != nil {
							return err
						}
					}
					current = append(current, change)
					currentBytes += size
					currentChunks += work
				}
				return nil
			}); err != nil {
				return nil, err
			}
			// Preserve a client's smaller boundary while still splitting legacy
			// oversized refs above. Combining refs here would undo deliberate
			// client-side work/byte batching.
			if err := flush(); err != nil {
				return nil, err
			}
		}
		return shards, nil
	}
	changesByShard := shardChanges(p.req.Changes, defaultSyncShardMaxFiles, defaultSyncShardMaxBytes)
	return p.writeInputShards(ctx, changesByShard)
}

func (p *syncPipeline) writeInputShards(ctx context.Context, changesByShard [][]models.FileChange) ([]syncInputShard, error) {
	shards := make([]syncInputShard, 0, len(changesByShard))
	for i, shard := range changesByShard {
		ref, err := p.writeJSONL(ctx, "inputs", fmt.Sprintf("shard-%06d", i), shard)
		if err != nil {
			return nil, err
		}
		shards = append(shards, syncInputShard{Ref: ref, FileCount: len(shard)})
	}
	return shards, nil
}

func (p *syncPipeline) runChunkStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "chunking")
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
			if job.SyncID != "" {
				if err := p.server.db.RecordSyncJobShard(ctx, job.SyncID, syncStageChunk, job.ShardIndex, job.FilesInShard); err != nil {
					return err
				}
			}
			next := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageEmbed, resultRef, job.ShardIndex, job.TotalShards, job.FilesInShard)
			next.JobID = job.JobID + "-embed"
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageChunk, job.JobID, resultRef, next); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processChunkJob(ctx context.Context, job objectQueueJob, sourceCache *syncSourceCache) (string, error) {
	w := newSyncArtifactWriter(ctx, p, "chunks", job.JobID)
	wrote := false
	err := p.forEachJSONL(ctx, job.PayloadRef, func(raw json.RawMessage) error {
		var change models.FileChange
		if err := json.Unmarshal(raw, &change); err != nil {
			return err
		}
		return p.chunkChangeEach(ctx, change, sourceCache, func(item syncChunkArtifact) error {
			wrote = true
			return w.Append(item)
		})
	})
	if err != nil {
		return "", err
	}
	if !wrote {
		if err := w.Append(syncChunkArtifact{Op: "noop"}); err != nil {
			return "", err
		}
	}
	return w.Close(ctx)
}

func (p *syncPipeline) chunkChangeEach(ctx context.Context, change models.FileChange, sourceCache *syncSourceCache, emit func(syncChunkArtifact) error) error {
	switch change.Status {
	case models.StatusAdded, models.StatusModified:
		s3Key := change.SourceKey
		if s3Key == "" {
			s3Key = fmt.Sprintf("files/%s/%s", p.rootID, change.Path)
		}
		if change.Status == models.StatusModified {
			if err := emit(syncChunkArtifact{Op: "close", Change: change}); err != nil {
				return err
			}
		}
		if localChunkable(change.Path) {
			sourceLength := change.SourceLength
			if sourceLength <= 0 {
				sourceLength = change.Size
			}
			if sourceLength > localChunkStreamThreshold() {
				return p.chunkLocalSourceEach(ctx, s3Key, change, func(chunk map[string]any) error {
					attachAbsolutePath([]map[string]any{chunk}, change.AbsolutePath)
					return emit(syncChunkArtifact{Op: "chunk", Change: change, Chunk: chunk})
				})
			}
			fileData, err := sourceCache.read(ctx, s3Key, change.SourceOffset, change.SourceLength)
			if err != nil {
				return fmt.Errorf("downloading %s: %w", s3Key, err)
			}
			return chunkLocallyEach(fileData, p.rootID, change.Path, func(chunk map[string]any) error {
				attachAbsolutePath([]map[string]any{chunk}, change.AbsolutePath)
				return emit(syncChunkArtifact{Op: "chunk", Change: change, Chunk: chunk})
			})
		} else {
			var contentB64 string
			if !modalCanReadSourceDirectly(s3Key, change) {
				fileData, err := sourceCache.read(ctx, s3Key, change.SourceOffset, change.SourceLength)
				if err != nil {
					return fmt.Errorf("downloading %s: %w", s3Key, err)
				}
				contentB64 = base64.StdEncoding.EncodeToString(fileData)
			}
			chunkResp, err := p.server.modal.ChunkFile(ChunkFileRequest{
				S3Key:        s3Key,
				FilePath:     change.Path,
				AbsolutePath: change.AbsolutePath,
				FileType:     detectFileType(change.Path),
				RootID:       p.rootID,
				ContentB64:   contentB64,
			})
			if err != nil {
				return err
			}
			attachAbsolutePath(chunkResp.Chunks, change.AbsolutePath)
			for _, chunk := range chunkResp.Chunks {
				if err := emit(syncChunkArtifact{Op: "chunk", Change: change, Chunk: chunk}); err != nil {
					return err
				}
			}
			return nil
		}
	case models.StatusRemoved:
		return emit(syncChunkArtifact{Op: "close", Change: change})
	case models.StatusMoved, models.StatusRenamed:
		rows, err := p.queryActiveRows(ctx, change.OldPath, []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "line_start", "line_end", "vector"})
		if err != nil {
			return err
		}
		if len(rows) >= activeRowsQueryLimit {
			return fmt.Errorf("move/rename %s has at least %d active chunks; re-sync as remove+add to avoid partial metadata copy", change.OldPath, activeRowsQueryLimit)
		}
		if err := emit(syncChunkArtifact{Op: "close", Change: models.FileChange{Path: change.OldPath, Status: models.StatusRemoved}}); err != nil {
			return err
		}
		for i, row := range rows {
			chunk := indexedChunkFromExisting(p.rootID, p.generation.ID, p.generation.Seq, change.Path, change.AbsolutePath, change.ContentHash, intFromAny(row["chunk_index"], i), row)
			if err := emit(syncChunkArtifact{Op: "row", Change: change, Row: chunk.mapRow()}); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func modalCanReadSourceDirectly(s3Key string, change models.FileChange) bool {
	return s3Key != "" && !isSourceBundleKey(s3Key) && change.SourceOffset == 0
}

func isSourceBundleKey(s3Key string) bool {
	return strings.HasPrefix(s3Key, "bundles/") || strings.Contains(s3Key, "/sources/bundles/")
}

func attachAbsolutePath(chunks []map[string]any, absolutePath string) {
	if absolutePath == "" {
		return
	}
	for _, chunk := range chunks {
		chunk["absolute_path"] = absolutePath
	}
}

func (p *syncPipeline) runEmbedStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "embedding")
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
			if job.SyncID != "" {
				if err := p.server.db.RecordSyncJobShard(ctx, job.SyncID, syncStageEmbed, job.ShardIndex, job.FilesInShard); err != nil {
					return err
				}
			}
			next := newObjectQueueJob(syncJobIdentifier(p.job), p.generation.ID, p.generation.Seq, syncStageIndex, resultRef, job.ShardIndex, job.TotalShards, job.FilesInShard)
			next.JobID = job.JobID + "-index"
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageEmbed, job.JobID, resultRef, next); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processEmbedJob(ctx context.Context, job objectQueueJob) (string, error) {
	w := newSyncArtifactWriter(ctx, p, "index_rows", job.JobID)
	batch := make([]syncIndexArtifact, 0, syncEmbedWorkBatchRows())
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.prepareIndexRows(ctx, batch); err != nil {
			return err
		}
		for _, row := range batch {
			if err := w.Append(row); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	err := p.forEachJSONL(ctx, job.PayloadRef, func(raw json.RawMessage) error {
		var item syncChunkArtifact
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		var row syncIndexArtifact
		switch item.Op {
		case "close":
			row = syncIndexArtifact{Op: "close", ClosePath: item.Change.Path}
		case "row":
			row = syncIndexArtifact{Op: "upsert", Row: item.Row}
		case "chunk":
			row = syncIndexArtifact{Op: "upsert", Row: indexedChunkFromModal(p.rootID, p.generation.ID, p.generation.Seq, item.Change.ContentHash, item.Chunk).mapRow()}
		default:
			return nil
		}
		batch = append(batch, row)
		if len(batch) >= cap(batch) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := flush(); err != nil {
		return "", err
	}
	return w.Close(ctx)
}

func (p *syncPipeline) prepareIndexRows(ctx context.Context, rows []syncIndexArtifact) error {
	if p.req != nil && p.req.DisableVector {
		for i := range rows {
			if rows[i].Op == "upsert" {
				delete(rows[i].Row, "vector")
			}
		}
		return nil
	}
	contentHashes := make([]string, 0, len(rows))
	for _, item := range rows {
		if item.Op == "upsert" && item.Row != nil {
			contentHashes = append(contentHashes, strVal(item.Row, "content_hash"))
		}
	}
	cached, err := p.server.db.GetCachedEmbeddings(ctx, p.orgID, p.server.modal.EmbeddingModelVersion(), contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup failed: %v", err)
		cached = map[string][]float64{}
	}
	pending := make([]pendingEmbedding, 0, len(rows))
	for i := range rows {
		if rows[i].Op != "upsert" || rows[i].Row == nil {
			continue
		}
		if _, ok := rows[i].Row["vector"]; ok {
			continue
		}
		hash := strVal(rows[i].Row, "content_hash")
		if emb, ok := cached[hash]; ok {
			rows[i].Row["vector"] = emb
			continue
		}
		pending = append(pending, pendingEmbedding{chunk: modalChunkPayload(rows[i].Row), row: rows[i].Row, contentHash: hash})
	}
	if len(pending) == 0 {
		return nil
	}
	return p.server.resolvePendingEmbeddings(ctx, p.orgID, pending)
}

func syncEmbedWorkBatchRows() int {
	return boundedArtifactSetting("PUFFERFS_SYNC_EMBED_BATCH_ROWS", 128, 1, 512)
}

func (p *syncPipeline) runIndexStage(ctx context.Context) error {
	if p.job != nil {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.job.ID, "upserting")
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
			filesProcessed, err := p.processIndexJob(ctx, job)
			if err != nil {
				_ = p.broker.Fail(ctx, p.generation.ID, syncStageIndex, job.JobID, err.Error(), 3)
				return err
			}
			if job.SyncID != "" {
				progressFiles, err := p.progressFileCount(ctx, job, filesProcessed)
				if err != nil {
					return err
				}
				if err := p.server.db.RecordSyncJobShard(ctx, job.SyncID, syncStageIndex, job.ShardIndex, progressFiles); err != nil {
					return err
				}
			}
			if err := p.broker.Complete(ctx, p.generation.ID, syncStageIndex, job.JobID, job.PayloadRef); err != nil {
				return err
			}
		}
	}
}

func (p *syncPipeline) processIndexJob(ctx context.Context, job objectQueueJob) (int, error) {
	indexNamespaces, err := p.loadIndexNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	distanceMetric := "cosine_distance"
	if p.req != nil && p.req.DisableVector {
		distanceMetric = ""
	}
	buffers := make(map[string][]map[string]any)
	processedPaths := make(map[string]bool)
	flushNamespace := func(namespace string) error {
		rows := buffers[namespace]
		if len(rows) == 0 {
			return nil
		}
		if err := p.server.upsertRowsInBatches(namespace, rows, distanceMetric); err != nil {
			return err
		}
		p.resp.ChunksAdded += len(rows)
		buffers[namespace] = buffers[namespace][:0]
		return nil
	}
	flushAll := func() error {
		for namespace := range buffers {
			if err := flushNamespace(namespace); err != nil {
				return err
			}
		}
		return nil
	}
	err = p.forEachJSONL(ctx, job.PayloadRef, func(raw json.RawMessage) error {
		var record syncIndexArtifact
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		switch record.Op {
		case "upsert":
			if record.Row == nil {
				return nil
			}
			filePath := strVal(record.Row, "file_path")
			processedPaths[filePath] = true
			ns, err := rootIndexNamespaceForPath(indexNamespaces, filePath)
			if err != nil {
				return fmt.Errorf("routing index row for %s: %w", filePath, err)
			}
			buffers[ns.Namespace] = append(buffers[ns.Namespace], record.Row)
			if len(buffers[ns.Namespace]) >= tpWriteBatchSize() {
				return flushNamespace(ns.Namespace)
			}
		case "close":
			if record.ClosePath == "" {
				return nil
			}
			if err := flushAll(); err != nil {
				return err
			}
			processedPaths[record.ClosePath] = true
			closed, err := p.closeRowsForPath(ctx, record.ClosePath)
			if err != nil {
				return err
			}
			p.resp.ChunksRemoved += closed
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := flushAll(); err != nil {
		return 0, err
	}
	filesProcessed := len(processedPaths)
	p.resp.FilesProcessed += filesProcessed
	return filesProcessed, nil
}

func (p *syncPipeline) countIndexJobFiles(ctx context.Context, job objectQueueJob) (int, error) {
	processedPaths := make(map[string]bool)
	err := p.forEachJSONL(ctx, job.PayloadRef, func(raw json.RawMessage) error {
		var record syncIndexArtifact
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.Op == "upsert" && record.Row != nil {
			if path := strVal(record.Row, "file_path"); path != "" {
				processedPaths[path] = true
			}
		} else if record.Op == "close" && record.ClosePath != "" {
			processedPaths[record.ClosePath] = true
		}
		return nil
	})
	return len(processedPaths), err
}

func (p *syncPipeline) progressFileCount(ctx context.Context, job objectQueueJob, indexArtifactFiles int) (int, error) {
	if job.FilesInShard > 0 {
		return job.FilesInShard, nil
	}
	count, err := p.countOriginalShardFiles(ctx, job.ShardIndex)
	if err == nil {
		return count, nil
	}
	if indexArtifactFiles > 0 {
		return indexArtifactFiles, nil
	}
	return 0, err
}

func (p *syncPipeline) messageFileCount(ctx context.Context, msg queue.JobMessage) (int, error) {
	if msg.FilesInShard > 0 {
		return msg.FilesInShard, nil
	}
	if msg.Stage == syncStageChunk && msg.PayloadRef != "" {
		return p.countInputShardFiles(ctx, msg.PayloadRef)
	}
	return p.countOriginalShardFiles(ctx, msg.ShardIndex)
}

func (p *syncPipeline) countOriginalShardFiles(ctx context.Context, shardIndex int) (int, error) {
	keys := []string{
		syncManifestShardKey(p.generation.ID, shardIndex),
		syncInputShardKey(p.generation.ID, shardIndex),
	}
	var lastErr error
	for _, key := range keys {
		count, err := p.countInputShardFiles(ctx, key)
		if err == nil {
			return count, nil
		}
		lastErr = err
		if !isObjectNotFound(err) {
			return 0, err
		}
	}
	return 0, lastErr
}

func (p *syncPipeline) countInputShardFiles(ctx context.Context, ref string) (int, error) {
	count := 0
	err := p.forEachJSONL(ctx, ref, func(raw json.RawMessage) error {
		var change models.FileChange
		if err := json.Unmarshal(raw, &change); err != nil {
			return err
		}
		if change.Status != models.StatusUnchanged {
			count++
		}
		return nil
	})
	return count, err
}

func countIndexArtifactFiles(records []syncIndexArtifact) int {
	processedPaths := make(map[string]bool)
	for _, record := range records {
		switch record.Op {
		case "upsert":
			if record.Row != nil {
				if path := strVal(record.Row, "file_path"); path != "" {
					processedPaths[path] = true
				}
			}
		case "close":
			if record.ClosePath != "" {
				processedPaths[record.ClosePath] = true
			}
		}
	}
	return len(processedPaths)
}

func (p *syncPipeline) closeRowsForPath(ctx context.Context, path string) (int, error) {
	filters := []any{
		[]any{"file_path", "Eq", path},
	}
	if p.generation.BaseGenerationSeq > 0 {
		filters = append(filters, activeGenerationFilter(p.generation.BaseGenerationSeq))
	}
	patch := map[string]any{
		"valid_to_generation":     p.generation.ID,
		"valid_to_generation_seq": p.generation.Seq,
	}
	total := 0
	indexNamespaces, err := p.loadIndexNamespaces(ctx)
	if err != nil {
		return total, err
	}
	ns, err := rootIndexNamespaceForPath(indexNamespaces, path)
	if err != nil {
		return total, fmt.Errorf("routing close for %s: %w", path, err)
	}
	for pass := 0; pass < 100; pass++ {
		rowsRemaining, affected, err := p.server.tp.PatchByFilter(ns.Namespace, tpAndFilter(filters), patch, true)
		if err != nil {
			return total, err
		}
		total += affected
		if !rowsRemaining {
			return total, nil
		}
	}
	return total, fmt.Errorf("closing rows for %s: rows remain after repeated patch passes", path)
}

func (p *syncPipeline) queryActiveRows(ctx context.Context, path string, attrs []string) ([]map[string]any, error) {
	filters := []any{
		[]any{"file_path", "Eq", path},
	}
	if p.generation.BaseGenerationSeq > 0 {
		filters = append(filters, activeGenerationFilter(p.generation.BaseGenerationSeq))
	}
	indexNamespaces, err := p.loadIndexNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	ns, err := rootIndexNamespaceForPath(indexNamespaces, path)
	if err != nil {
		return nil, fmt.Errorf("routing active row query for %s: %w", path, err)
	}
	return p.server.tp.Query(ns.Namespace, []any{"file_path", "asc"}, activeRowsQueryLimit, tpAndFilter(filters), attrs)
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
