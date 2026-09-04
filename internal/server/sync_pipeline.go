package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sync/errgroup"
)

const (
	syncStageChunk  = "chunk"
	syncStageIndex  = "index"
	syncStageCommit = "commit"

	defaultSyncShardMaxFiles  = 128
	defaultSyncShardMaxChunks = 8192
	activeRowsQueryLimit      = 10000
	rangedSourceOffsetFence   = int64(-1)
)

type syncPipeline struct {
	server     *Server
	orgID      string
	rootID     string
	generation *SyncGeneration
	jobID      string
	userID     string
	req        *models.SyncRequest
	resp       *models.SyncResponse

	indexNamespaces       []models.RootIndexNamespace
	indexNamespacesLoaded bool
}

type syncArtifact struct {
	Op        string         `json:"op"`
	Row       map[string]any `json:"row,omitempty"`
	ClosePath string         `json:"close_path,omitempty"`
}

type syncInputShard struct {
	Ref       string
	FileCount int
}

// syncChunkWork is the execution record stored in input shards. Unranged
// records remain wire-compatible with the old FileChange-only JSON shape.
// Ranged records let independent workers read disjoint regions of one source.
type syncChunkWork struct {
	models.FileChange
	RangeOffset    int64 `json:"work_range_offset,omitempty"`
	RangeLength    int64 `json:"work_range_length,omitempty"`
	RangeLineStart int64 `json:"work_range_line_start,omitempty"`
	RangeIndex     int   `json:"work_range_index,omitempty"`
	RangeCount     int   `json:"work_range_count,omitempty"`
}

func chunkWorkForChange(change models.FileChange) []syncChunkWork {
	if len(change.SourceRanges) == 0 {
		return []syncChunkWork{{FileChange: change}}
	}
	work := make([]syncChunkWork, len(change.SourceRanges))
	ranges := change.SourceRanges
	change.SourceRanges = nil
	// Older workers do not understand the work_range fields. Make them fail
	// the storage read instead of indexing the entire file once per range
	// during a rolling deployment. Current workers use RangeOffset directly.
	change.SourceOffset = rangedSourceOffsetFence
	for i, sourceRange := range ranges {
		work[i] = syncChunkWork{
			FileChange:     change,
			RangeOffset:    sourceRange.Offset,
			RangeLength:    sourceRange.Length,
			RangeLineStart: sourceRange.LineStart,
			RangeIndex:     i,
			RangeCount:     len(ranges),
		}
	}
	return work
}

func (work syncChunkWork) fileCredit() int {
	if work.RangeCount == 0 || work.RangeIndex == work.RangeCount-1 {
		return 1
	}
	return 0
}

func (work syncChunkWork) closesPreviousRows() bool {
	return work.RangeCount == 0 || work.RangeIndex == 0
}

func validateChunkWork(work syncChunkWork) error {
	if work.RangeCount == 0 {
		return nil
	}
	if work.RangeIndex < 0 || work.RangeIndex >= work.RangeCount || work.RangeOffset < 0 || work.RangeLength <= 0 || work.RangeLineStart < 1 {
		return fmt.Errorf("invalid ranged chunk work for %s", work.Path)
	}
	if work.SourceOffset != rangedSourceOffsetFence || work.SourceKey == "" || !localChunkable(work.Path) {
		return fmt.Errorf("unsupported ranged chunk work for %s", work.Path)
	}
	return nil
}

func (s *Server) processSync(ctx context.Context, orgID string, generation *SyncGeneration, req *models.SyncRequest, jobID string) (*models.SyncResponse, error) {
	p := &syncPipeline{
		server:     s,
		orgID:      orgID,
		rootID:     req.RootID,
		generation: generation,
		jobID:      jobID,
		req:        req,
		resp: &models.SyncResponse{
			RootID:        req.RootID,
			SyncJobID:     jobID,
			GenerationID:  generation.ID,
			GenerationSeq: generation.Seq,
		},
	}
	return p.run(ctx)
}

func (p *syncPipeline) run(ctx context.Context) (*models.SyncResponse, error) {
	jobs, err := p.prepareJobs(ctx)
	if err != nil {
		return nil, err
	}
	if p.jobID != "" {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.jobID, "chunking")
	}
	sourceCache := &syncSourceCache{s3: p.server.s3}
	for i := range jobs {
		ref, err := p.processChunkJob(ctx, jobs[i], sourceCache)
		if err != nil {
			return nil, err
		}
		if p.jobID != "" {
			if _, _, err := p.server.db.RecordSyncJobShard(ctx, p.jobID, syncStageChunk, jobs[i].ShardIndex, jobs[i].FilesInShard); err != nil {
				return nil, err
			}
		}
		jobs[i].PayloadRef = ref
	}
	if p.jobID != "" {
		_ = p.server.db.UpdateSyncJobStatus(ctx, p.jobID, "indexing")
	}
	for i := range jobs {
		jobs[i].JobID += "-index"
		jobs[i].Stage = syncStageIndex
		if err := p.processIndexJob(ctx, jobs[i]); err != nil {
			return nil, err
		}
		if p.jobID != "" {
			if _, _, err := p.server.db.RecordSyncJobShard(ctx, p.jobID, syncStageIndex, jobs[i].ShardIndex, jobs[i].FilesInShard); err != nil {
				return nil, err
			}
		}
	}
	return p.resp, nil
}

func (s *Server) enqueueSync(ctx context.Context, orgID, userID string, generation *SyncGeneration, req *models.SyncRequest, jobID string) (*models.SyncResponse, error) {
	p := &syncPipeline{
		server:     s,
		orgID:      orgID,
		rootID:     req.RootID,
		generation: generation,
		jobID:      jobID,
		userID:     userID,
		req:        req,
		resp: &models.SyncResponse{
			RootID:        req.RootID,
			SyncJobID:     jobID,
			GenerationID:  generation.ID,
			GenerationSeq: generation.Seq,
		},
	}
	request, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := s.s3.Upload(ctx, syncRequestKey(generation.ID), request, "application/json"); err != nil {
		return nil, err
	}
	msgs, err := p.prepareJobs(ctx)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		commit := p.jobMessage(syncStageCommit, generation.ID+"-commit", syncRequestKey(generation.ID), 0, 0, 0)
		if err := s.queue.Enqueue(ctx, syncStageCommit, commit); err != nil {
			return nil, err
		}
		return p.resp, nil
	}
	if err := s.queue.Enqueue(ctx, syncStageChunk, msgs...); err != nil {
		return nil, err
	}
	if jobID != "" {
		_ = s.db.UpdateSyncJobStatus(ctx, jobID, "queued")
	}
	return p.resp, nil
}

func (p *syncPipeline) prepareJobs(ctx context.Context) ([]queue.JobMessage, error) {
	if _, err := p.loadIndexNamespaces(ctx); err != nil {
		return nil, err
	}
	shards, err := p.inputShards(ctx)
	if err != nil {
		return nil, err
	}
	if p.jobID != "" {
		totalFiles := 0
		for _, shard := range shards {
			totalFiles += shard.FileCount
		}
		if err := p.server.db.UpdateSyncJobTotalFiles(ctx, p.jobID, totalFiles); err != nil {
			return nil, err
		}
	}
	msgs := make([]queue.JobMessage, 0, len(shards))
	for i, shard := range shards {
		jobID := fmt.Sprintf("%s-chunk-%06d", p.generation.ID, i)
		msgs = append(msgs, p.jobMessage(syncStageChunk, jobID, shard.Ref, i, len(shards), shard.FileCount))
	}
	return msgs, nil
}

func (p *syncPipeline) jobMessage(stage, jobID, payloadRef string, shardIndex, totalShards, filesInShard int) queue.JobMessage {
	return queue.JobMessage{
		JobID:             jobID,
		SyncJobID:         p.jobID,
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

func syncRequestKey(generationID string) string {
	return fmt.Sprintf("syncs/%s/request.json", generationID)
}

func (p *syncPipeline) inputShards(ctx context.Context) ([]syncInputShard, error) {
	var shards []syncInputShard
	var current []syncChunkWork
	var currentChunks int64
	var currentFiles int
	var currentBundle string
	var currentIsRange bool
	var uploads errgroup.Group
	uploads.SetLimit(4)
	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, work := range current {
			if err := enc.Encode(work); err != nil {
				return err
			}
		}
		ref := fmt.Sprintf("syncs/%s/inputs/shard-%06d.jsonl", p.generation.ID, len(shards))
		data := buf.Bytes()
		uploads.Go(func() error {
			if err := p.server.s3.Upload(ctx, ref, data, "application/x-ndjson"); err != nil {
				return fmt.Errorf("uploading %s: %w", ref, err)
			}
			return nil
		})
		shards = append(shards, syncInputShard{Ref: ref, FileCount: currentFiles})
		current = nil
		currentChunks = 0
		currentFiles = 0
		currentBundle = ""
		currentIsRange = false
		return nil
	}
	addWork := func(work syncChunkWork) error {
		change := work.FileChange
		// A source range is the unit of parallel execution, not merely another
		// record that may be packed behind work from the same large file.
		if len(current) > 0 && (currentIsRange || work.RangeCount > 0) {
			if err := flush(); err != nil {
				return err
			}
		}
		size := work.RangeLength
		if size == 0 {
			size = change.SourceLength
			if size == 0 {
				size = change.Size
			}
		}
		size = max(size, 0)
		bundle := ""
		if isSourceBundleKey(change.SourceKey) {
			bundle = change.SourceKey
		}
		if len(current) > 0 && bundle != currentBundle && (bundle != "" || currentBundle != "") {
			if err := flush(); err != nil {
				return err
			}
		}
		estimatedChunks := int64(1)
		if (change.Status == models.StatusAdded || change.Status == models.StatusModified || change.Status == models.StatusMoved || change.Status == models.StatusRenamed) && size > 0 {
			estimatedChunks = (size-1)/2000 + 1
		}
		if len(current) > 0 && (len(current) >= defaultSyncShardMaxFiles || currentChunks+estimatedChunks > defaultSyncShardMaxChunks) {
			if err := flush(); err != nil {
				return err
			}
		}
		current = append(current, work)
		currentChunks += estimatedChunks
		currentFiles += work.fileCredit()
		currentBundle = bundle
		currentIsRange = work.RangeCount > 0
		return nil
	}
	add := func(change models.FileChange) error {
		if err := normalizeSyncChange(p.rootID, p.generation.ID, &change); err != nil {
			return err
		}
		if change.Status == models.StatusUnchanged {
			return nil
		}
		for _, work := range chunkWorkForChange(change) {
			if err := addWork(work); err != nil {
				return err
			}
		}
		return nil
	}
	var inputErr error
	for _, change := range p.req.Changes {
		if inputErr = add(change); inputErr != nil {
			break
		}
	}
	if inputErr == nil {
		for _, ref := range p.req.ChangeRefs {
			if ref != "" {
				inputErr = eachJSONL(ctx, p.server.s3, ref, add)
			}
			if inputErr != nil {
				break
			}
		}
	}
	if inputErr == nil {
		inputErr = flush()
	}
	uploadErr := uploads.Wait()
	if inputErr != nil {
		return nil, inputErr
	}
	if uploadErr != nil {
		return nil, uploadErr
	}
	return shards, nil
}

func (p *syncPipeline) processChunkJob(ctx context.Context, job queue.JobMessage, sourceCache *syncSourceCache) (string, error) {
	return p.streamJSONL(ctx, "chunks", job.JobID, func(enc *json.Encoder) error {
		var workItems []syncChunkWork
		if err := eachJSONL(ctx, p.server.s3, job.PayloadRef, func(work syncChunkWork) error {
			if err := validateChunkWork(work); err != nil {
				return err
			}
			workItems = append(workItems, work)
			return nil
		}); err != nil {
			return err
		}
		var oldPaths []string
		for _, work := range workItems {
			change := work.FileChange
			path := ""
			switch change.Status {
			case models.StatusModified, models.StatusRemoved:
				if work.closesPreviousRows() {
					path = change.Path
				}
			case models.StatusMoved, models.StatusRenamed:
				path = change.OldPath
				oldPaths = append(oldPaths, path)
			}
			if path != "" {
				if err := enc.Encode(syncArtifact{Op: "close", ClosePath: path}); err != nil {
					return err
				}
			}
		}
		moveRows := make(map[string][]map[string]any, len(oldPaths))
		if len(oldPaths) > 0 {
			indexNamespaces, err := p.loadIndexNamespaces(ctx)
			if err != nil {
				return err
			}
			pathsByNamespace := make(map[string][]string)
			for _, path := range oldPaths {
				ns, err := rootIndexNamespaceForPath(indexNamespaces, path)
				if err != nil {
					return fmt.Errorf("routing move source %s: %w", path, err)
				}
				pathsByNamespace[ns.Namespace] = append(pathsByNamespace[ns.Namespace], path)
			}
			attrs := []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "line_start", "line_end", "vector"}
			for namespace, paths := range pathsByNamespace {
				filters := []any{[]any{"file_path", "In", paths}}
				if p.generation.BaseGenerationSeq > 0 {
					filters = append(filters, activeGenerationFilter(p.generation.BaseGenerationSeq))
				}
				rows, err := p.server.tp.Query(namespace, []any{"file_path", "asc"}, activeRowsQueryLimit, tpAndFilter(filters), attrs)
				if err != nil {
					return err
				}
				if len(rows) >= activeRowsQueryLimit {
					return fmt.Errorf("moves in %s have at least %d active chunks; re-sync them as remove+add", namespace, activeRowsQueryLimit)
				}
				for _, row := range rows {
					path := strVal(row, "file_path")
					moveRows[path] = append(moveRows[path], row)
				}
			}
		}
		for _, work := range workItems {
			change := work.FileChange
			switch change.Status {
			case models.StatusAdded, models.StatusModified:
				if err := p.chunkFileWorkEach(ctx, work, sourceCache, func(item syncArtifact) error { return enc.Encode(item) }); err != nil {
					return err
				}
			case models.StatusMoved, models.StatusRenamed:
				for i, row := range moveRows[change.OldPath] {
					chunk := indexedChunkFromExisting(p.rootID, p.generation.ID, p.generation.Seq, change.Path, change.AbsolutePath, change.ContentHash, intFromAny(row["chunk_index"], i), row)
					if err := enc.Encode(syncArtifact{Op: "upsert", Row: chunk.mapRow()}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func (p *syncPipeline) chunkFileWorkEach(ctx context.Context, work syncChunkWork, sourceCache *syncSourceCache, emit func(syncArtifact) error) error {
	change := work.FileChange
	if change.Size == 0 && change.SourceLength == 0 && change.SourceKey == "" {
		return nil
	}
	s3Key := change.SourceKey
	if s3Key == "" {
		s3Key = fmt.Sprintf("files/%s/%s", p.rootID, change.Path)
	}
	emitChunk := func(chunk map[string]any) error {
		if change.AbsolutePath != "" {
			chunk["absolute_path"] = change.AbsolutePath
		}
		row := indexedChunkFromModal(p.rootID, p.generation.ID, p.generation.Seq, change.ContentHash, chunk).mapRow()
		return emit(syncArtifact{Op: "upsert", Row: row})
	}
	if localChunkable(change.Path) {
		if work.RangeCount > 0 {
			chunkIndex := int(work.RangeOffset)
			lineStart := int(work.RangeLineStart)
			if int64(chunkIndex) != work.RangeOffset || int64(lineStart) != work.RangeLineStart {
				return fmt.Errorf("source range for %s exceeds local integer limits", change.Path)
			}
			return p.chunkLocalSourceRangeEach(
				ctx,
				s3Key,
				change,
				work.RangeOffset,
				work.RangeLength,
				chunkIndex,
				lineStart,
				emitChunk,
			)
		}
		sourceLength := change.SourceLength
		if sourceLength <= 0 {
			sourceLength = change.Size
		}
		if sourceLength > localChunkStreamThreshold {
			return p.chunkLocalSourceEach(ctx, s3Key, change, emitChunk)
		}
		fileData, err := sourceCache.read(ctx, s3Key, change.SourceOffset, change.SourceLength)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", s3Key, err)
		}
		return chunkLocallyEach(fileData, p.rootID, change.Path, emitChunk)
	}
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
	for _, chunk := range chunkResp.Chunks {
		if err := emitChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

func modalCanReadSourceDirectly(s3Key string, change models.FileChange) bool {
	return s3Key != "" && change.SourceOffset == 0 && !isSourceBundleKey(s3Key)
}

func isSourceBundleKey(key string) bool {
	return strings.HasPrefix(key, "bundles/") || strings.Contains(key, "/sources/bundles/")
}

func (p *syncPipeline) prepareIndexRows(ctx context.Context, rows []syncArtifact) error {
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
	pendingByHash := make(map[string]int, len(rows))
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
		if index, ok := pendingByHash[hash]; ok && hash != "" {
			pending[index].rows = append(pending[index].rows, rows[i].Row)
			continue
		}
		if hash != "" {
			pendingByHash[hash] = len(pending)
		}
		pending = append(pending, pendingEmbedding{chunk: modalChunkPayload(rows[i].Row), rows: []map[string]any{rows[i].Row}, contentHash: hash})
	}
	if len(pending) == 0 {
		return nil
	}
	return p.server.resolvePendingEmbeddings(ctx, p.orgID, pending)
}

func (p *syncPipeline) processIndexJob(ctx context.Context, job queue.JobMessage) error {
	indexNamespaces, err := p.loadIndexNamespaces(ctx)
	if err != nil {
		return err
	}
	distanceMetric := "cosine_distance"
	if p.req != nil && p.req.DisableVector {
		distanceMetric = ""
	}
	type routedRow struct {
		namespace string
		row       map[string]any
	}
	batchSize := tpWriteBatchSize()
	batchMaxBytes := max(1, tpWriteBatchMaxBytes()-(64<<10))
	writeBuffer := make([]routedRow, 0, batchSize)
	writeBytes := 0
	closePaths := make(map[string][]string)
	flushWrites := func() error {
		if len(writeBuffer) == 0 {
			return nil
		}
		byNamespace := make(map[string][]map[string]any)
		var namespaceOrder []string
		for _, item := range writeBuffer {
			if _, ok := byNamespace[item.namespace]; !ok {
				namespaceOrder = append(namespaceOrder, item.namespace)
			}
			byNamespace[item.namespace] = append(byNamespace[item.namespace], item.row)
		}
		for _, namespace := range namespaceOrder {
			rows := byNamespace[namespace]
			if err := p.server.tp.UpsertRows(namespace, rows, distanceMetric); err != nil {
				return err
			}
			p.resp.ChunksAdded += len(rows)
		}
		writeBuffer = writeBuffer[:0]
		writeBytes = 0
		return nil
	}
	appendWrite := func(namespace string, row map[string]any) error {
		encoded, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("measuring index row: %w", err)
		}
		rowBytes := len(encoded) + 1
		if rowBytes > batchMaxBytes {
			return fmt.Errorf("index row for %s is %d bytes; maximum batch bytes is %d", strVal(row, "file_path"), rowBytes, batchMaxBytes)
		}
		if len(writeBuffer) > 0 && (len(writeBuffer) >= batchSize || writeBytes+rowBytes > batchMaxBytes) {
			if err := flushWrites(); err != nil {
				return err
			}
		}
		writeBuffer = append(writeBuffer, routedRow{namespace: namespace, row: row})
		writeBytes += rowBytes
		if len(writeBuffer) >= batchSize || writeBytes >= batchMaxBytes {
			return flushWrites()
		}
		return nil
	}
	flushCloses := func() error {
		patch := map[string]any{
			"valid_to_generation":     p.generation.ID,
			"valid_to_generation_seq": p.generation.Seq,
		}
		for namespace, paths := range closePaths {
			filters := []any{
				[]any{"file_path", "In", paths},
				[]any{"Or", []any{
					[]any{"valid_to_generation_seq", "Eq", 0},
					[]any{"valid_to_generation_seq", "Lte", p.generation.Seq},
				}},
			}
			if p.generation.BaseGenerationSeq > 0 {
				filters = append(filters, activeGenerationFilter(p.generation.BaseGenerationSeq))
			}
			for pass := 0; pass < 100; pass++ {
				remaining, affected, err := p.server.tp.PatchByFilter(namespace, tpAndFilter(filters), patch, true)
				if err != nil {
					return err
				}
				p.resp.ChunksRemoved += affected
				if !remaining {
					break
				}
				if pass == 99 {
					return fmt.Errorf("closing rows in %s: rows remain after repeated patch passes", namespace)
				}
			}
		}
		clear(closePaths)
		return nil
	}
	applyPrepared := func(record syncArtifact) error {
		switch record.Op {
		case "upsert":
			if len(closePaths) > 0 {
				if err := flushCloses(); err != nil {
					return err
				}
			}
			if record.Row == nil {
				return nil
			}
			filePath := strVal(record.Row, "file_path")
			ns, err := rootIndexNamespaceForPath(indexNamespaces, filePath)
			if err != nil {
				return fmt.Errorf("routing index row for %s: %w", filePath, err)
			}
			return appendWrite(ns.Namespace, record.Row)
		case "close":
			if record.ClosePath == "" {
				return nil
			}
			if err := flushWrites(); err != nil {
				return err
			}
			ns, err := rootIndexNamespaceForPath(indexNamespaces, record.ClosePath)
			if err != nil {
				return fmt.Errorf("routing close for %s: %w", record.ClosePath, err)
			}
			closePaths[ns.Namespace] = append(closePaths[ns.Namespace], record.ClosePath)
		}
		return nil
	}
	embedBatch := make([]syncArtifact, 0, 128)
	flushEmbedBatch := func() error {
		if len(embedBatch) == 0 {
			return nil
		}
		if err := p.prepareIndexRows(ctx, embedBatch); err != nil {
			return err
		}
		for _, record := range embedBatch {
			if err := applyPrepared(record); err != nil {
				return err
			}
		}
		embedBatch = embedBatch[:0]
		return nil
	}
	err = eachJSONL(ctx, p.server.s3, job.PayloadRef, func(record syncArtifact) error {
		if record.Op != "upsert" && record.Op != "close" {
			return nil
		}
		embedBatch = append(embedBatch, record)
		if len(embedBatch) == cap(embedBatch) {
			return flushEmbedBatch()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flushEmbedBatch(); err != nil {
		return err
	}
	if len(closePaths) > 0 {
		if err := flushCloses(); err != nil {
			return err
		}
	}
	if err := flushWrites(); err != nil {
		return err
	}
	p.resp.FilesProcessed += job.FilesInShard
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
