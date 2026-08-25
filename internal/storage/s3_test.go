package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type stubStreamUploader struct {
	upload func(context.Context, *s3.PutObjectInput) (*manager.UploadOutput, error)
}

func (s stubStreamUploader) Upload(ctx context.Context, input *s3.PutObjectInput, _ ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	return s.upload(ctx, input)
}

func TestUploadStreamPassesBodyDirectlyToUploader(t *testing.T) {
	body := strings.NewReader("streamed content")
	uploader := stubStreamUploader{upload: func(_ context.Context, input *s3.PutObjectInput) (*manager.UploadOutput, error) {
		if input.Body != body {
			t.Fatal("UploadStream replaced the original body")
		}
		if got := *input.Bucket; got != "test-bucket" {
			t.Fatalf("bucket = %q, want test-bucket", got)
		}
		if got := *input.Key; got != "files/root/file.txt" {
			t.Fatalf("key = %q, want files/root/file.txt", got)
		}
		if got := *input.ContentType; got != "text/plain" {
			t.Fatalf("content type = %q, want text/plain", got)
		}
		data, err := io.ReadAll(input.Body)
		if err != nil {
			t.Fatalf("reading upload body: %v", err)
		}
		if got := string(data); got != "streamed content" {
			t.Fatalf("body = %q, want streamed content", got)
		}
		return &manager.UploadOutput{}, nil
	}}

	client := &Client{uploader: uploader, bucket: "test-bucket"}
	if err := client.UploadStream(context.Background(), "files/root/file.txt", body, "text/plain"); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
}

func TestUploadStreamDoesNotReadBeforeUploader(t *testing.T) {
	readErr := errors.New("body was read")
	body := readerFunc(func([]byte) (int, error) { return 0, readErr })
	uploadErr := errors.New("upload failed")
	uploader := stubStreamUploader{upload: func(_ context.Context, input *s3.PutObjectInput) (*manager.UploadOutput, error) {
		if input.Body == nil {
			t.Fatal("uploader received a nil body")
		}
		return nil, uploadErr
	}}

	client := &Client{uploader: uploader, bucket: "test-bucket"}
	err := client.UploadStream(context.Background(), "key", body, "application/octet-stream")
	if !errors.Is(err, uploadErr) {
		t.Fatalf("UploadStream error = %v, want uploader error", err)
	}
	if errors.Is(err, readErr) {
		t.Fatalf("UploadStream read the body before handing it to the uploader: %v", err)
	}
}

func TestUploadStreamUsesMultipartForLargeNonSeekableBody(t *testing.T) {
	store := &multipartUploadStore{}
	uploader := newStreamUploader(store, func(u *manager.Uploader) {
		u.Concurrency = 1
	})
	client := &Client{uploader: uploader, aborter: store, bucket: "test-bucket"}
	bodySize := manager.MinUploadPartSize + 1024
	body := io.LimitReader(zeroReader{}, bodySize)

	if err := client.UploadStream(context.Background(), "large.bin", body, "application/octet-stream"); err != nil {
		t.Fatalf("UploadStream: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.putCalls != 0 {
		t.Fatalf("PutObject calls = %d, want multipart upload", store.putCalls)
	}
	if store.createCalls != 1 || store.completeCalls != 1 {
		t.Fatalf("multipart lifecycle calls: create=%d complete=%d, want 1 each", store.createCalls, store.completeCalls)
	}
	if store.partCalls != 2 {
		t.Fatalf("UploadPart calls = %d, want 2", store.partCalls)
	}
	if store.uploadedBytes != bodySize {
		t.Fatalf("uploaded bytes = %d, want %d", store.uploadedBytes, bodySize)
	}
}

func TestNewStreamUploaderBoundsBuffersAndDefersCleanup(t *testing.T) {
	store := &multipartUploadStore{}
	uploader := newStreamUploader(store)

	if uploader.Concurrency != 2 {
		t.Fatalf("uploader concurrency = %d, want 2", uploader.Concurrency)
	}
	if !uploader.LeavePartsOnError {
		t.Fatal("uploader must leave SDK cleanup disabled so UploadStream can abort with a live context")
	}
	if uploader.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatalf("checksum mode = %v, want required-only", uploader.RequestChecksumCalculation)
	}
}

func TestUploadStreamAbortsMultipartWithLiveContextAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &multipartUploadStore{}
	uploader := newStreamUploader(store, func(u *manager.Uploader) { u.Concurrency = 1 })
	client := &Client{uploader: uploader, aborter: store, bucket: "test-bucket"}
	body := &cancelingReader{
		remaining: manager.MinUploadPartSize,
		cancel:    cancel,
	}

	err := client.UploadStream(ctx, "interrupted.bin", body, "application/octet-stream")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("UploadStream error = %v, want context cancellation", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.abortCalls != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", store.abortCalls)
	}
	if store.abortContextErr != nil {
		t.Fatalf("abort context was already canceled: %v", store.abortContextErr)
	}
	if store.completeCalls != 0 {
		t.Fatalf("CompleteMultipartUpload calls = %d, want 0", store.completeCalls)
	}
}

func TestUploadStreamReportsAbortFailure(t *testing.T) {
	uploadErr := multipartFailure{err: errors.New("part failed"), uploadID: "upload-1"}
	abortErr := errors.New("abort failed")
	aborter := &stubMultipartAborter{err: abortErr}
	uploader := stubStreamUploader{upload: func(context.Context, *s3.PutObjectInput) (*manager.UploadOutput, error) {
		return nil, uploadErr
	}}
	client := &Client{uploader: uploader, aborter: aborter, bucket: "test-bucket"}

	err := client.UploadStream(context.Background(), "key", strings.NewReader("body"), "application/octet-stream")
	if !errors.Is(err, uploadErr.err) {
		t.Fatalf("UploadStream error = %v, want upload error", err)
	}
	if !errors.Is(err, abortErr) {
		t.Fatalf("UploadStream error = %v, want abort error", err)
	}
}

func TestUploadStreamAbortsAfterPartFailure(t *testing.T) {
	partErr := errors.New("part failed")
	store := &multipartUploadStore{partErr: partErr}
	uploader := newStreamUploader(store, func(u *manager.Uploader) { u.Concurrency = 1 })
	client := &Client{uploader: uploader, aborter: store, bucket: "test-bucket"}
	body := io.LimitReader(zeroReader{}, manager.MinUploadPartSize+1)

	err := client.UploadStream(context.Background(), "failed.bin", body, "application/octet-stream")
	if !errors.Is(err, partErr) {
		t.Fatalf("UploadStream error = %v, want part failure", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.abortCalls != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", store.abortCalls)
	}
	if store.completeCalls != 0 {
		t.Fatalf("CompleteMultipartUpload calls = %d, want 0", store.completeCalls)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) {
	return f(p)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type cancelingReader struct {
	remaining int64
	cancel    context.CancelFunc
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		r.cancel()
		return 0, context.Canceled
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	clear(p[:n])
	r.remaining -= n
	return int(n), nil
}

type multipartFailure struct {
	err      error
	uploadID string
}

func (e multipartFailure) Error() string    { return e.err.Error() }
func (e multipartFailure) Unwrap() error    { return e.err }
func (e multipartFailure) UploadID() string { return e.uploadID }

type stubMultipartAborter struct {
	err error
}

func (s *stubMultipartAborter) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return nil, s.err
}

type multipartUploadStore struct {
	mu              sync.Mutex
	putCalls        int
	createCalls     int
	partCalls       int
	completeCalls   int
	abortCalls      int
	uploadedBytes   int64
	abortContextErr error
	partErr         error
}

func (s *multipartUploadStore) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	return &s3.PutObjectOutput{}, nil
}

func (s *multipartUploadStore) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (s *multipartUploadStore) UploadPart(_ context.Context, input *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	n, err := io.Copy(io.Discard, input.Body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partCalls++
	s.uploadedBytes += n
	if s.partErr != nil {
		return nil, s.partErr
	}
	return &s3.UploadPartOutput{ETag: aws.String("part")}, nil
}

func (s *multipartUploadStore) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (s *multipartUploadStore) AbortMultipartUpload(ctx context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abortCalls++
	s.abortContextErr = ctx.Err()
	return &s3.AbortMultipartUploadOutput{}, nil
}

var _ manager.UploadAPIClient = (*multipartUploadStore)(nil)
