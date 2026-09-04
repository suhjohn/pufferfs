package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pufferfs/pufferfs/internal/sourcecapture"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	defaultMultipartMinBytes = int64(64 << 20)
	memoryMultipartPartBytes = int64(32 << 20)
	maxMultipartFileLanes    = 4
)

func multipartUploadMinBytes() int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("PUFFERFS_MULTIPART_MIN_BYTES")), 10, 64)
	if value < 1 {
		return defaultMultipartMinBytes
	}
	return value
}

func multipartFileConcurrency() int {
	outer := uploadConcurrency()
	return min(maxMultipartFileLanes, max(1, maxUploadConcurrency/outer))
}

func captureMultipartSource(client *apiClient, rootID, generationID, relPath, localPath string, file *os.File, before os.FileInfo) (capturedSource, bool, error) {
	initBody, err := client.post(fmt.Sprintf("/roots/%s/upload/multipart/init", rootID), models.MultipartSourceInitRequest{
		GenerationID: generationID,
		Path:         relPath,
		Size:         before.Size(),
	})
	if multipartUploadUnsupported(err) {
		return capturedSource{}, false, nil
	}
	if err != nil {
		return capturedSource{}, true, fmt.Errorf("starting direct multipart upload: %w", err)
	}
	var init models.MultipartSourceInitResponse
	if err := json.Unmarshal(initBody, &init); err != nil {
		if init.Key != "" && init.UploadID != "" {
			abortMultipartSource(client, rootID, generationID, init.Key, init.UploadID)
		}
		return capturedSource{}, true, fmt.Errorf("parsing multipart upload response: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			abortMultipartSource(client, rootID, generationID, init.Key, init.UploadID)
		}
	}()
	expectedParts := 0
	if init.PartSize > 0 {
		expectedParts = int((before.Size() + init.PartSize - 1) / init.PartSize)
	}
	if init.Key == "" || init.UploadID == "" || init.PartSize <= 0 || init.PartCount < 1 || init.PartCount != expectedParts || init.RangeBytes < 0 {
		return capturedSource{}, true, fmt.Errorf("multipart upload response contains invalid limits")
	}

	parts, contentHash, sourceRanges, err := captureAndUploadParts(
		client, rootID, generationID, relPath, file, before.Size(), init,
	)
	if err != nil {
		return capturedSource{}, true, err
	}
	dirty := sourceChangedAfterCapture(localPath, file, before)
	completeBody, err := completeMultipartSource(client, rootID, models.MultipartSourceCompleteRequest{
		GenerationID: generationID,
		Path:         relPath,
		Key:          init.Key,
		UploadID:     init.UploadID,
		Size:         before.Size(),
		PartSize:     init.PartSize,
		ContentHash:  contentHash,
		SourceRanges: sourceRanges,
		Parts:        parts,
	})
	if err != nil {
		return capturedSource{}, true, fmt.Errorf("completing direct multipart upload: %w", err)
	}
	var resp models.SourceUploadResponse
	if err := json.Unmarshal(completeBody, &resp); err != nil {
		return capturedSource{}, true, fmt.Errorf("parsing multipart completion response: %w", err)
	}
	if resp.Key != init.Key || resp.ContentHash != contentHash || resp.Size != before.Size() {
		return capturedSource{}, true, fmt.Errorf("multipart completion response does not match captured source")
	}
	completed = true
	dirty = dirty || sourceChangedAfterCapture(localPath, file, before)
	return capturedSource{
		Path:         relPath,
		ContentHash:  resp.ContentHash,
		Size:         resp.Size,
		Mtime:        before.ModTime().UnixNano(),
		SourceKey:    resp.Key,
		SourceLength: resp.Size,
		SourceRanges: resp.SourceRanges,
		Multipart:    true,
		Dirty:        dirty,
	}, true, nil
}

func completeMultipartSource(client *apiClient, rootID string, req models.MultipartSourceCompleteRequest) ([]byte, error) {
	const maxAttempts = 3
	const baseDelay = 250 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<(attempt-1))
			if client.sleep != nil {
				client.sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}
		body, err := client.post(fmt.Sprintf("/roots/%s/upload/multipart/complete", rootID), req)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !isRetryableUploadError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func multipartUploadUnsupported(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusNotImplemented)
}

func abortMultipartSource(client *apiClient, rootID, generationID, key, uploadID string) {
	if client == nil || key == "" || uploadID == "" {
		return
	}
	_, _ = client.post(fmt.Sprintf("/roots/%s/upload/multipart/abort", rootID), models.MultipartSourceAbortRequest{
		GenerationID: generationID,
		Key:          key,
		UploadID:     uploadID,
	})
}

type multipartPartPayload struct {
	data []byte
	path string
	size int64
}

func captureMultipartPart(file *os.File, size int64, contentHash hash.Hash, ranges *sourcecapture.RangePlanner) (multipartPartPayload, error) {
	if size <= memoryMultipartPartBytes {
		data := make([]byte, int(size))
		if _, err := io.ReadFull(file, data); err != nil {
			return multipartPartPayload{}, err
		}
		if _, err := contentHash.Write(data); err != nil {
			return multipartPartPayload{}, err
		}
		if _, err := ranges.Write(data); err != nil {
			return multipartPartPayload{}, err
		}
		return multipartPartPayload{data: data, size: size}, nil
	}

	temp, err := os.CreateTemp("", "pufferfs-multipart-part-*")
	if err != nil {
		return multipartPartPayload{}, err
	}
	tempPath := temp.Name()
	written, copyErr := io.CopyN(io.MultiWriter(temp, contentHash, ranges), file, size)
	closeErr := temp.Close()
	if copyErr != nil || written != size || closeErr != nil {
		_ = os.Remove(tempPath)
		if copyErr != nil {
			return multipartPartPayload{}, copyErr
		}
		if closeErr != nil {
			return multipartPartPayload{}, closeErr
		}
		return multipartPartPayload{}, io.ErrUnexpectedEOF
	}
	return multipartPartPayload{path: tempPath, size: size}, nil
}

func (p multipartPartPayload) open() (io.ReadCloser, error) {
	if p.path != "" {
		return os.Open(p.path)
	}
	return io.NopCloser(bytes.NewReader(p.data)), nil
}

func (p multipartPartPayload) cleanup() {
	if p.path != "" {
		_ = os.Remove(p.path)
	}
}

func captureAndUploadParts(client *apiClient, rootID, generationID, relPath string, file *os.File, totalSize int64, init models.MultipartSourceInitResponse) ([]models.MultipartSourceCompletedPart, string, []models.SourceRange, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", nil, err
	}
	contentHash := sha256.New()
	ranges := sourcecapture.NewRangePlanner(init.RangeBytes > 0, init.RangeBytes)
	parts := make([]models.MultipartSourceCompletedPart, init.PartCount)
	slots := make(chan struct{}, multipartFileConcurrency())
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	loadErr := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr
	}

	var captured int64
	for partIndex := 0; partIndex < init.PartCount; partIndex++ {
		if err := loadErr(); err != nil {
			break
		}
		slots <- struct{}{}
		if err := loadErr(); err != nil {
			<-slots
			break
		}
		partSize := min(init.PartSize, totalSize-captured)
		payload, err := captureMultipartPart(file, partSize, contentHash, ranges)
		if err != nil {
			<-slots
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				recordErr(&sourceChangedError{path: relPath, err: err})
			} else {
				recordErr(err)
			}
			break
		}
		captured += partSize
		partNumber := partIndex + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer payload.cleanup()
			etag, err := uploadMultipartPart(client, rootID, generationID, init.Key, init.UploadID, partNumber, payload)
			if err != nil {
				recordErr(fmt.Errorf("uploading multipart part %d/%d: %w", partNumber, init.PartCount, err))
				return
			}
			parts[partIndex] = models.MultipartSourceCompletedPart{PartNumber: partNumber, ETag: etag}
		}()
	}
	wg.Wait()
	if err := loadErr(); err != nil {
		return nil, "", nil, err
	}
	if captured != totalSize {
		return nil, "", nil, &sourceChangedError{path: relPath, err: fmt.Errorf("captured %d of %d bytes", captured, totalSize)}
	}
	return parts, "sha256:" + hex.EncodeToString(contentHash.Sum(nil)), ranges.Finish(), nil
}

func uploadMultipartPart(client *apiClient, rootID, generationID, key, uploadID string, partNumber int, payload multipartPartPayload) (string, error) {
	const maxAttempts = 3
	const baseDelay = 250 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<(attempt-1))
			if client.sleep != nil {
				client.sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}
		responseBody, err := client.post(fmt.Sprintf("/roots/%s/upload/multipart/part", rootID), models.MultipartSourcePartRequest{
			GenerationID: generationID,
			Key:          key,
			UploadID:     uploadID,
			PartNumber:   partNumber,
			Size:         payload.size,
		})
		if err != nil {
			lastErr = err
			if !isRetryableUploadError(err) {
				return "", err
			}
			continue
		}
		var signed models.MultipartSourcePartResponse
		if err := json.Unmarshal(responseBody, &signed); err != nil {
			return "", err
		}
		if signed.URL == "" {
			return "", fmt.Errorf("multipart part response is missing URL")
		}
		etag, err := putMultipartPart(client, signed, payload)
		if err == nil {
			return etag, nil
		}
		lastErr = err
		if !isRetryableUploadError(err) {
			return "", err
		}
	}
	return "", lastErr
}

func putMultipartPart(client *apiClient, signed models.MultipartSourcePartResponse, payload multipartPartPayload) (string, error) {
	body, err := payload.open()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPut, signed.URL, body)
	if err != nil {
		_ = body.Close()
		return "", err
	}
	req.ContentLength = payload.size
	for name, values := range signed.Headers {
		if strings.EqualFold(name, "host") {
			if len(values) > 0 {
				req.Host = values[0]
			}
			continue
		}
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := client.httpClient.Do(req)
	if err != nil {
		_ = body.Close()
		return "", fmt.Errorf("multipart PUT failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &apiError{StatusCode: resp.StatusCode, Body: responseBody}
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		return "", fmt.Errorf("multipart PUT response is missing ETag")
	}
	return etag, nil
}
