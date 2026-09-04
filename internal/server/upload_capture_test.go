package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestUploadHashedStreamReturnsStoredDigestAndSize(t *testing.T) {
	store := newMemoryObjectStore()
	body := []byte("captured bytes")
	hash, size, err := uploadHashedStream(context.Background(), store, "capture", bytes.NewReader(body), "application/octet-stream", int64(len(body)))
	if err != nil {
		t.Fatalf("uploadHashedStream: %v", err)
	}
	if hash != "sha256:31a98f8b885ad1f171b243b6c1402bf5299e88e6732b9a4e0d0584709ddcbb58" {
		t.Fatalf("hash = %q", hash)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
	stored, err := store.Download(context.Background(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, body) {
		t.Fatalf("stored = %q, want %q", stored, body)
	}
}

func TestUploadHashedStreamRejectsAndDeletesShortBody(t *testing.T) {
	store := newMemoryObjectStore()
	_, size, err := uploadHashedStream(context.Background(), store, "capture", bytes.NewReader([]byte("short")), "application/octet-stream", 10)
	if !errors.Is(err, errUploadLengthMismatch) {
		t.Fatalf("err = %v, want errUploadLengthMismatch", err)
	}
	if size != 5 {
		t.Fatalf("size = %d, want 5", size)
	}
	if _, err := store.Download(context.Background(), "capture"); err == nil {
		t.Fatal("short capture object was not deleted")
	}
}

func TestSyncSourceFilePathAcceptsCaptureAndLegacyKeys(t *testing.T) {
	legacy := syncSourceFileKey("gen-1", "docs/a.txt")
	if path, ok := syncSourceFilePath("gen-1", legacy); !ok || path != "docs/a.txt" {
		t.Fatalf("legacy key resolved to path=%q ok=%v", path, ok)
	}
	capture := syncSourceCaptureFileKey("gen-1", "94e45e1e-7979-4718-bc01-2d46c424d93c", "docs/a.txt")
	if path, ok := syncSourceFilePath("gen-1", capture); !ok || path != "docs/a.txt" {
		t.Fatalf("capture key resolved to path=%q ok=%v", path, ok)
	}
}
