package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/diff"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/internal/merkle"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// A captureCandidate needs authoritative bytes before it can be compared with
// the committed root. The discovery metadata is only a cache hint; it never
// becomes the file's committed content identity.
type captureCandidate struct {
	Path string
	Size int64
}

type capturePlan struct {
	State      map[string]models.FileState
	Candidates []captureCandidate
	CacheHits  int
	Present    map[string]bool
}

type capturedSource struct {
	Path         string
	ContentHash  string
	Size         int64
	Mtime        int64
	SourceKey    string
	SourceOffset int64
	SourceLength int64
	SourceRanges []models.SourceRange
	Multipart    bool
	Dirty        bool
}

type captureBatch struct {
	Files    map[string]capturedSource
	Deferred map[string]error
}

type captureSyncInput struct {
	Config            *appconfig.Config
	Client            *apiClient
	Dir               string
	Name              string
	RootID            string
	BaseGenerationID  string
	BaseGenerationSeq int64
	BaseState         map[string]models.FileState
	HashCache         map[string]models.FileState
	Policy            ignore.PolicyPatternSet
	Select            func(string) bool
	Force             bool
	WaitForCompletion bool
	Log               io.Writer
}

func runCapturedSyncWithConflictRetry(input captureSyncInput) (*syncCommandResult, error) {
	result, err := runCapturedSyncOnce(input)
	var conflict *syncConflictError
	if !errors.As(err, &conflict) {
		return result, err
	}

	fmt.Fprintln(input.Log, "Remote generation changed during capture; rebuilding against latest remote state.")
	latestRoot, loadErr := loadRemoteRoot(input.Client, input.RootID)
	if loadErr != nil {
		return nil, fmt.Errorf("loading remote root metadata after sync conflict: %w", loadErr)
	}
	baseState, loadErr := loadRemoteState(input.Client, input.RootID)
	if loadErr != nil {
		return nil, fmt.Errorf("loading remote state after sync conflict: %w", loadErr)
	}
	input.BaseGenerationID = latestRoot.VisibleGenerationID
	input.BaseGenerationSeq = latestRoot.VisibleGenerationSeq
	input.BaseState = baseState
	return runCapturedSyncOnce(input)
}

func runCapturedSyncOnce(input captureSyncInput) (*syncCommandResult, error) {
	if input.Log == nil {
		input.Log = os.Stdout
	}
	matcher := ignore.NewMatcherWithPolicy(input.Dir, input.Policy)
	dirtyBefore, err := loadLocalDirtyPaths(input.RootID)
	if err != nil {
		fmt.Fprintf(input.Log, "Warning: could not load dirty-path cache; recapturing uncached files: %v\n", err)
		dirtyBefore = make(map[string]bool)
		input.HashCache = nil
	}
	plan, err := discoverCapturePlan(input.Dir, matcher, input.BaseState, input.HashCache, dirtyBefore, input.Select, input.Force)
	if err != nil {
		return nil, fmt.Errorf("discovering files: %w", err)
	}

	if len(plan.Candidates) == 0 {
		finalDirty := reconcileDirtyPaths(dirtyBefore, input.Select, nil)
		return finishCapturedNoUpload(input, plan.State, finalDirty)
	}

	plannedFiles := len(plan.Candidates) + removedPathCount(input.BaseState, plan.Present, input.Select)
	fmt.Fprintf(input.Log, "Capturing and uploading %d files to root %s...\n", len(plan.Candidates), input.RootID)
	syncInit, err := initSyncSession(input.Client, input.RootID, input.BaseGenerationID, input.BaseGenerationSeq, plannedFiles)
	if err != nil {
		if conflict, ok := syncConflictFromError(err); ok {
			return nil, conflict
		}
		return nil, fmt.Errorf("initializing sync session: %w", err)
	}
	syncSubmitted := false
	defer func() {
		if !syncSubmitted {
			_ = abortSyncSession(input.Client, input.RootID, syncInit.GenerationID)
		}
	}()
	heartbeat := startSyncSessionHeartbeat(input.Client, input.RootID, syncInit.GenerationID, input.Log)
	defer heartbeat.Stop()

	progressStep := len(plan.Candidates) / 20
	if progressStep < 1 {
		progressStep = 1
	}
	batch, err := captureFiles(input.Client, input.RootID, syncInit.GenerationID, input.Dir, plan.Candidates, func(files int, bytes int64) {
		if files == len(plan.Candidates) || files%progressStep == 0 {
			fmt.Fprintf(input.Log, "Upload progress: %d/%d files (%.1f MiB)\n", files, len(plan.Candidates), float64(bytes)/(1<<20))
		}
	})
	if err != nil {
		return nil, err
	}
	if count := multipartCaptureCount(batch); count > 0 {
		label := "files"
		if count == 1 {
			label = "file"
		}
		fmt.Fprintf(input.Log, "Direct multipart upload completed for %d large %s.\n", count, label)
	}
	for path, capture := range batch.Files {
		plan.State[path] = models.FileState{
			Size:        capture.Size,
			ContentHash: capture.ContentHash,
			Mtime:       capture.Mtime,
		}
	}
	for path := range batch.Deferred {
		if previous, ok := input.BaseState[path]; ok {
			plan.State[path] = previous
		}
	}

	newDirty := make(map[string]bool)
	for path, capture := range batch.Files {
		if capture.Dirty {
			newDirty[path] = true
		}
	}
	for path := range batch.Deferred {
		newDirty[path] = true
	}
	finalDirty := reconcileDirtyPaths(dirtyBefore, input.Select, newDirty)
	if len(batch.Deferred) > 0 {
		fmt.Fprintf(input.Log, "Warning: deferred %d files that changed before their capture completed; their previous versions remain visible.\n", len(batch.Deferred))
	}
	if dirtyCaptureCount(batch) > 0 {
		fmt.Fprintf(input.Log, "Notice: %d files changed during capture and remain scheduled for reconciliation.\n", dirtyCaptureCount(batch))
	}

	result := capturedDiff(input.BaseState, plan.State, input.Select, input.Force, batch.Deferred)
	currentTree, err := merkle.BuildTreeFromState(input.Dir, plan.State)
	if err != nil {
		return nil, fmt.Errorf("building captured Merkle tree: %w", err)
	}
	if countChanges(result) == 0 {
		printCapturedNoChanges(input)
		if err := saveLocalDirtyPaths(input.RootID, finalDirty); err != nil {
			return nil, fmt.Errorf("saving dirty paths: %w", err)
		}
		if err := saveLocalSyncCache(input.RootID, input.Name, input.Dir, plan.State, currentTree, input.BaseGenerationID, input.BaseGenerationSeq); err != nil {
			return nil, err
		}
		unchanged := unchangedSyncResult(input.RootID, input.Name, input.Dir, input.BaseGenerationID, input.BaseGenerationSeq)
		unchanged.dirtyPaths = sortedDirtyPaths(finalDirty)
		return unchanged, nil
	}

	changes, err := changesWithCapturedSources(input.Dir, result, batch.Files)
	if err != nil {
		return nil, err
	}
	changeCount := countChanges(result)
	fmt.Fprintf(input.Log, "Merkle diff found %d changed files (captured bytes are authoritative)\n", changeCount)
	fmt.Fprintf(input.Log, "Syncing %d changes to root %s...\n", changeCount, input.RootID)
	proof := currentTree.BuildContentProof()
	contentProof := &models.ContentProofData{
		FileHashes: proof.FileHashes,
		DirHashes:  proof.DirHashes,
		RootHash:   proof.RootHash,
	}
	metadataRefs, err := uploadSyncMetadata(input.Client, input.RootID, syncInit.GenerationID, changes, contentProof, plan.State)
	if err != nil {
		return nil, err
	}
	syncReq := models.SyncRequest{
		ProtocolVersion:   models.SyncProtocolVersion,
		RootID:            input.RootID,
		GenerationID:      syncInit.GenerationID,
		BaseGenerationID:  syncInit.BaseGenerationID,
		BaseGenerationSeq: syncInit.BaseGenerationSeq,
		ChangeRefs:        metadataRefs.ChangeRefs,
		StateRef:          metadataRefs.StateRef,
		ContentProofRef:   metadataRefs.ContentProofRef,
	}

	respBody, err := input.Client.post(fmt.Sprintf("/roots/%s/sync?async=true", input.RootID), syncReq)
	heartbeat.Stop()
	if err != nil {
		if conflict, ok := syncConflictFromError(err); ok {
			return nil, conflict
		}
		return nil, fmt.Errorf("sync request: %w", err)
	}
	syncSubmitted = true

	var syncResp models.SyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return nil, fmt.Errorf("parsing sync response: %w", err)
	}
	if syncResp.RootID == "" {
		syncResp.RootID = input.RootID
	}
	var completedJob *models.SyncJob
	if syncResp.SyncJobID != "" && input.WaitForCompletion {
		completedJob, err = pollSyncJob(input.Client, input.RootID, syncResp.SyncJobID, input.Log)
		if err != nil {
			return nil, err
		}
		syncResp.FilesProcessed = completedJob.Processed
	}

	if err := saveLocalDirtyPaths(input.RootID, finalDirty); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save dirty paths: %v\n", err)
	} else {
		saveLocalSyncCacheWarnings(input.RootID, input.Name, input.Dir, plan.State, currentTree, syncResp.GenerationID, syncResp.GenerationSeq)
	}

	var commandResult *syncCommandResult
	if !input.WaitForCompletion {
		fmt.Fprintf(input.Log, "Sync job %s started for root %s. Check status with: pufferfs sync status --root %s --job-id %s\n",
			syncResp.SyncJobID, input.RootID, input.RootID, syncResp.SyncJobID)
		commandResult = backgroundSyncResult(input.Name, input.Dir, changeCount, syncResp)
	} else {
		if completedJob != nil {
			fmt.Fprintf(input.Log, "Sync complete: %d/%d files processed\n", completedJob.Processed, completedJob.TotalFiles)
		} else {
			fmt.Fprintf(input.Log, "Sync complete: %d files processed, %d chunks added, %d removed, %d moved\n",
				syncResp.FilesProcessed, syncResp.ChunksAdded, syncResp.ChunksRemoved, syncResp.ChunksMoved)
		}
		commandResult = completedSyncResult(input.Name, input.Dir, changeCount, syncResp)
	}
	commandResult.FileChanges = filterChanges(result)
	commandResult.dirtyPaths = sortedDirtyPaths(finalDirty)
	return commandResult, nil
}

func finishCapturedNoUpload(input captureSyncInput, state map[string]models.FileState, dirty map[string]bool) (*syncCommandResult, error) {
	result := capturedDiff(input.BaseState, state, input.Select, input.Force, nil)
	if countChanges(result) != 0 {
		// This path contains removals only. Re-enter the regular session flow with
		// no capture candidates by using the existing precomputed submission path.
		tree, err := merkle.BuildTreeFromState(input.Dir, state)
		if err != nil {
			return nil, fmt.Errorf("building captured Merkle tree: %w", err)
		}
		if err := saveLocalDirtyPaths(input.RootID, dirty); err != nil {
			return nil, fmt.Errorf("saving dirty paths: %w", err)
		}
		commandResult, err := runSyncWithResult(input.Config, input.Dir, input.Name, input.RootID, "", false, false, input.WaitForCompletion, result, state, tree, input.BaseGenerationID, input.BaseGenerationSeq, input.Policy, input.Log)
		if err != nil {
			return commandResult, err
		}
		if commandResult != nil {
			commandResult.FileChanges = filterChanges(result)
			commandResult.dirtyPaths = sortedDirtyPaths(dirty)
		}
		return commandResult, nil
	}

	tree, err := merkle.BuildTreeFromState(input.Dir, state)
	if err != nil {
		return nil, fmt.Errorf("building captured Merkle tree: %w", err)
	}
	printCapturedNoChanges(input)
	if err := saveLocalDirtyPaths(input.RootID, dirty); err != nil {
		return nil, fmt.Errorf("saving dirty paths: %w", err)
	}
	if err := saveLocalSyncCache(input.RootID, input.Name, input.Dir, state, tree, input.BaseGenerationID, input.BaseGenerationSeq); err != nil {
		return nil, err
	}
	unchanged := unchangedSyncResult(input.RootID, input.Name, input.Dir, input.BaseGenerationID, input.BaseGenerationSeq)
	unchanged.dirtyPaths = sortedDirtyPaths(dirty)
	return unchanged, nil
}

func printCapturedNoChanges(input captureSyncInput) {
	if input.Select != nil {
		fmt.Fprintln(input.Log, "No changes detected for selected files.")
		return
	}
	fmt.Fprintln(input.Log, "No changes detected.")
}

func discoverCapturePlan(root string, matcher *ignore.Matcher, baseState, hashCache map[string]models.FileState, dirty map[string]bool, selectPath func(string) bool, force bool) (capturePlan, error) {
	plan := capturePlan{
		State:   make(map[string]models.FileState, len(baseState)),
		Present: make(map[string]bool),
	}
	if selectPath != nil {
		for path, state := range baseState {
			plan.State[path] = state
			if selectPath(path) {
				delete(plan.State, path)
			}
		}
	}

	root = filepath.Clean(root)
	err := filepath.WalkDir(root, func(absPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && absPath != root {
				return nil
			}
			return walkErr
		}
		relPath, err := filepath.Rel(root, absPath)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if matcher != nil && matcher.ShouldIgnore(relPath, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || (selectPath != nil && !selectPath(relPath)) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("stat %s: %w", relPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(absPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("stat symlink target %s: %w", relPath, err)
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		plan.Present[relPath] = true

		mtime := info.ModTime().UnixNano()
		cached, cacheHit := hashCache[relPath]
		base, existsInBase := baseState[relPath]
		if !force && !dirty[relPath] && cacheHit && existsInBase &&
			cached.ContentHash != "" && cached.ContentHash == base.ContentHash &&
			cached.Size == info.Size() && cached.Mtime == mtime {
			plan.State[relPath] = cached
			plan.CacheHits++
			return nil
		}
		plan.Candidates = append(plan.Candidates, captureCandidate{Path: relPath, Size: info.Size()})
		return nil
	})
	if err != nil {
		return capturePlan{}, err
	}
	sort.Slice(plan.Candidates, func(i, j int) bool { return plan.Candidates[i].Path < plan.Candidates[j].Path })
	return plan, nil
}

func captureFiles(client *apiClient, rootID, generationID, dir string, candidates []captureCandidate, progress ...func(int, int64)) (captureBatch, error) {
	smallLimit := uploadBundleSmallFileLimit()
	maxBundleBytes := uploadBundleMaxBytes()
	uploads := newBoundedUploadGroup(uploadConcurrency())
	results := make([]captureFileResult, len(candidates))
	var bundle bytes.Buffer
	var bundleCandidates []int
	bundleID := fmt.Sprintf("%d", time.Now().UnixNano())
	bundleIndex := 0
	var progressMu sync.Mutex
	completedFiles := 0
	var completedBytes int64
	reportProgress := func(files int, bytes int64) {
		if len(progress) == 0 || progress[0] == nil {
			return
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		completedFiles += files
		completedBytes += bytes
		progress[0](completedFiles, completedBytes)
	}

	flushBundle := func() bool {
		if bundle.Len() == 0 {
			return true
		}
		bundleName := fmt.Sprintf("%s-%06d", bundleID, bundleIndex)
		bundleData := bundle.Bytes()
		candidateIndexes := bundleCandidates
		bundleBytes := int64(len(bundleData))
		if !uploads.Go(func() error {
			key, err := uploadBundle(client, rootID, generationID, bundleName, bundleData, "application/octet-stream")
			if err != nil {
				return fmt.Errorf("uploading source bundle %s: %w", bundleName, err)
			}
			for _, candidateIndex := range candidateIndexes {
				results[candidateIndex].capture.SourceKey = key
			}
			reportProgress(len(candidateIndexes), bundleBytes)
			return nil
		}) {
			return false
		}
		bundle = bytes.Buffer{}
		bundleCandidates = nil
		bundleIndex++
		return true
	}

	for i, candidate := range candidates {
		localPath := filepath.Join(dir, filepath.FromSlash(candidate.Path))
		startStandalone := func() bool {
			candidateIndex := i
			if !uploads.Go(func() error {
				capture, err := captureStandaloneFile(client, rootID, generationID, candidate.Path, localPath)
				if err != nil {
					var changed *sourceChangedError
					if errors.As(err, &changed) {
						results[candidateIndex].deferred = err
						reportProgress(1, 0)
						return nil
					}
					return fmt.Errorf("capturing %s: %w", candidate.Path, err)
				}
				results[candidateIndex] = captureFileResult{ready: true, capture: capture}
				reportProgress(1, capture.Size)
				return nil
			}) {
				return false
			}
			return true
		}
		if candidate.Size > min(smallLimit, maxBundleBytes) {
			if !startStandalone() {
				break
			}
			continue
		}

		data, capture, err := captureSmallFile(candidate.Path, localPath, smallLimit)
		if err != nil {
			if errors.Is(err, errCaptureNeedsStream) {
				if !startStandalone() {
					break
				}
				continue
			}
			var changed *sourceChangedError
			if errors.As(err, &changed) {
				results[i].deferred = err
				reportProgress(1, 0)
				continue
			}
			uploads.Fail(fmt.Errorf("capturing %s: %w", candidate.Path, err))
			break
		}
		if len(data) == 0 {
			results[i] = captureFileResult{ready: true, capture: capture}
			reportProgress(1, 0)
			continue
		}
		if bundle.Len() > 0 && (len(bundleCandidates) == 128 || int64(bundle.Len()+len(data)) > maxBundleBytes) {
			if !flushBundle() {
				break
			}
		}
		capture.SourceOffset = int64(bundle.Len())
		capture.SourceLength = int64(len(data))
		if _, err := bundle.Write(data); err != nil {
			uploads.Fail(err)
			break
		}
		results[i] = captureFileResult{ready: true, capture: capture}
		bundleCandidates = append(bundleCandidates, i)
	}
	if uploads.Err() == nil {
		flushBundle()
	}
	if err := uploads.Wait(); err != nil {
		return captureBatch{}, err
	}

	batch := captureBatch{
		Files:    make(map[string]capturedSource),
		Deferred: make(map[string]error),
	}
	for i, result := range results {
		if result.deferred != nil {
			batch.Deferred[candidates[i].Path] = result.deferred
			continue
		}
		if !result.ready {
			continue
		}
		capture := result.capture
		batch.Files[capture.Path] = capture
	}
	return batch, nil
}

type captureFileResult struct {
	ready    bool
	capture  capturedSource
	deferred error
}

type sourceChangedError struct {
	path string
	err  error
}

func (e *sourceChangedError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("source %s changed during capture: %v", e.path, e.err)
	}
	return fmt.Sprintf("source %s changed during capture", e.path)
}

func (e *sourceChangedError) Unwrap() error { return e.err }

var errCaptureNeedsStream = errors.New("capture exceeds small-file buffer")

func captureStandaloneFile(client *apiClient, rootID, generationID, relPath, localPath string) (capturedSource, error) {
	file, err := os.Open(localPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return capturedSource{}, &sourceChangedError{path: relPath, err: err}
		}
		return capturedSource{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return capturedSource{}, err
	}
	if !before.Mode().IsRegular() {
		return capturedSource{}, &sourceChangedError{path: relPath, err: fmt.Errorf("source is no longer a regular file")}
	}
	captureSize := before.Size()
	if captureSize >= multipartUploadMinBytes() && captureSize > 0 {
		capture, supported, err := captureMultipartSource(client, rootID, generationID, relPath, localPath, file, before)
		if supported || err != nil {
			return capture, err
		}
	}
	body := io.NewSectionReader(file, 0, captureSize)
	path := fmt.Sprintf("/roots/%s/upload?generation_id=%s&path=%s", rootID, url.QueryEscape(generationID), url.QueryEscape(relPath))
	respBody, uploadErr := client.postStream(path, body, "application/octet-stream")
	dirty := sourceChangedAfterCapture(localPath, file, before)
	if uploadErr != nil {
		// A short fixed-extent read means the source version disappeared while
		// being captured and can be deferred independently. A server or network
		// error must still fail the sync even when the source also changed.
		afterDescriptor, statErr := file.Stat()
		if statErr != nil || afterDescriptor.Size() < captureSize {
			return capturedSource{}, &sourceChangedError{path: relPath, err: uploadErr}
		}
		return capturedSource{}, uploadErr
	}
	var resp models.SourceUploadResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return capturedSource{}, err
	}
	if resp.Key == "" || !validContentHash(resp.ContentHash) {
		return capturedSource{}, fmt.Errorf("upload response missing authoritative key or SHA-256")
	}
	if resp.Size != captureSize {
		return capturedSource{}, fmt.Errorf("upload response size %d does not match captured size %d", resp.Size, captureSize)
	}
	return capturedSource{
		Path:         relPath,
		ContentHash:  resp.ContentHash,
		Size:         resp.Size,
		Mtime:        before.ModTime().UnixNano(),
		SourceKey:    resp.Key,
		SourceLength: resp.Size,
		SourceRanges: resp.SourceRanges,
		Dirty:        dirty,
	}, nil
}

func captureSmallFile(relPath, localPath string, maxBytes int64) ([]byte, capturedSource, error) {
	file, err := os.Open(localPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, capturedSource{}, &sourceChangedError{path: relPath, err: err}
		}
		return nil, capturedSource{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, capturedSource{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, capturedSource{}, &sourceChangedError{path: relPath, err: fmt.Errorf("source is no longer a regular file")}
	}
	if before.Size() > maxBytes {
		return nil, capturedSource{}, errCaptureNeedsStream
	}
	if before.Size() < 0 || before.Size() > int64(int(^uint(0)>>1)) {
		return nil, capturedSource{}, fmt.Errorf("file is too large to buffer")
	}
	data := make([]byte, int(before.Size()))
	if _, err := io.ReadFull(file, data); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, capturedSource{}, &sourceChangedError{path: relPath, err: err}
		}
		return nil, capturedSource{}, err
	}
	sum := sha256.Sum256(data)
	return data, capturedSource{
		Path:         relPath,
		ContentHash:  "sha256:" + hex.EncodeToString(sum[:]),
		Size:         int64(len(data)),
		Mtime:        before.ModTime().UnixNano(),
		SourceLength: int64(len(data)),
		Dirty:        sourceChangedAfterCapture(localPath, file, before),
	}, nil
}

func sourceChangedAfterCapture(path string, file *os.File, before os.FileInfo) bool {
	afterDescriptor, err := file.Stat()
	if err != nil || afterDescriptor.Size() != before.Size() || !afterDescriptor.ModTime().Equal(before.ModTime()) {
		return true
	}
	afterPath, err := os.Stat(path)
	if err != nil {
		return true
	}
	return !os.SameFile(before, afterPath) || afterPath.Size() != before.Size() || !afterPath.ModTime().Equal(before.ModTime())
}

func validContentHash(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return strings.HasPrefix(value, "sha256:") && err == nil && len(decoded) == sha256.Size
}

func capturedDiff(baseState, currentState map[string]models.FileState, selectPath func(string) bool, force bool, deferred map[string]error) models.DiffResult {
	if !force {
		return limitCapturedMoveReuse(diff.Compute(baseState, currentState), baseState, currentState)
	}
	result := models.DiffResult{}
	seen := make(map[string]bool, len(baseState)+len(currentState))
	paths := make([]string, 0, len(baseState)+len(currentState))
	for path := range baseState {
		if selectPath != nil && !selectPath(path) {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	for path := range currentState {
		if (selectPath != nil && !selectPath(path)) || seen[path] {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if deferred[path] != nil {
			continue
		}
		current, hasCurrent := currentState[path]
		previous, hadPrevious := baseState[path]
		switch {
		case hasCurrent && hadPrevious:
			result.Changes = append(result.Changes, models.FileChange{Path: path, Status: models.StatusModified, ContentHash: current.ContentHash, Size: current.Size})
			result.Stats.Modified++
		case hasCurrent:
			result.Changes = append(result.Changes, models.FileChange{Path: path, Status: models.StatusAdded, ContentHash: current.ContentHash, Size: current.Size})
			result.Stats.Added++
		case hadPrevious:
			result.Changes = append(result.Changes, models.FileChange{Path: path, Status: models.StatusRemoved, ContentHash: previous.ContentHash, Size: previous.Size})
			result.Stats.Removed++
		}
	}
	return result
}

func limitCapturedMoveReuse(result models.DiffResult, baseState, currentState map[string]models.FileState) models.DiffResult {
	limited := models.DiffResult{Stats: result.Stats}
	maxMoveBytes := moveReuseMaxBytes()
	for _, change := range result.Changes {
		if (change.Status != models.StatusMoved && change.Status != models.StatusRenamed) || change.Size <= maxMoveBytes {
			limited.Changes = append(limited.Changes, change)
			continue
		}
		previous := baseState[change.OldPath]
		current := currentState[change.Path]
		limited.Changes = append(limited.Changes,
			models.FileChange{Path: change.OldPath, Status: models.StatusRemoved, ContentHash: previous.ContentHash, Size: previous.Size},
			models.FileChange{Path: change.Path, Status: models.StatusAdded, ContentHash: current.ContentHash, Size: current.Size},
		)
		limited.Stats.Removed++
		limited.Stats.Added++
		switch change.Status {
		case models.StatusMoved:
			limited.Stats.Moved--
		case models.StatusRenamed:
			limited.Stats.Renamed--
		}
	}
	return limited
}

func changesWithCapturedSources(root string, result models.DiffResult, captures map[string]capturedSource) ([]models.FileChange, error) {
	changes := withAbsolutePaths(root, filterChanges(result))
	for i := range changes {
		if changes[i].Status != models.StatusAdded && changes[i].Status != models.StatusModified {
			continue
		}
		capture, ok := captures[changes[i].Path]
		if !ok {
			return nil, fmt.Errorf("captured source missing for %s change %s", changes[i].Status, changes[i].Path)
		}
		changes[i].ContentHash = capture.ContentHash
		changes[i].Size = capture.Size
		changes[i].SourceKey = capture.SourceKey
		changes[i].SourceOffset = capture.SourceOffset
		changes[i].SourceLength = capture.SourceLength
		changes[i].SourceRanges = capture.SourceRanges
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].SourceKey != changes[j].SourceKey {
			return changes[i].SourceKey < changes[j].SourceKey
		}
		if changes[i].SourceOffset != changes[j].SourceOffset {
			return changes[i].SourceOffset < changes[j].SourceOffset
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func removedPathCount(baseState map[string]models.FileState, present map[string]bool, selectPath func(string) bool) int {
	count := 0
	for path := range baseState {
		if selectPath != nil && !selectPath(path) {
			continue
		}
		if !present[path] {
			count++
		}
	}
	return count
}

func dirtyCaptureCount(batch captureBatch) int {
	count := 0
	for _, capture := range batch.Files {
		if capture.Dirty {
			count++
		}
	}
	return count
}

func multipartCaptureCount(batch captureBatch) int {
	count := 0
	for _, capture := range batch.Files {
		if capture.Multipart {
			count++
		}
	}
	return count
}

func reconcileDirtyPaths(previous map[string]bool, selectPath func(string) bool, current map[string]bool) map[string]bool {
	result := make(map[string]bool)
	if selectPath != nil {
		for path := range previous {
			if !selectPath(path) {
				result[path] = true
			}
		}
	}
	for path := range current {
		result[path] = true
	}
	return result
}

func sortedDirtyPaths(paths map[string]bool) []string {
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func loadLocalDirtyPaths(rootID string) (map[string]bool, error) {
	result := make(map[string]bool)
	if rootID == "" {
		return result, nil
	}
	data, err := os.ReadFile(filepath.Join(appconfig.RootDir(rootID), "dirty.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, err
	}
	for _, path := range paths {
		if path != "" {
			result[path] = true
		}
	}
	return result, nil
}

func saveLocalDirtyPaths(rootID string, paths map[string]bool) error {
	if rootID == "" {
		return nil
	}
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(sortedDirtyPaths(paths))
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".dirty-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, filepath.Join(dir, "dirty.json"))
}
