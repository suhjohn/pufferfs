package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPostStreamRetriesSeekableBodyFromOriginalOffset(t *testing.T) {
	var attempts int
	var bodies []string
	var contentLengths []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		contentLengths = append(contentLengths, r.ContentLength)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		bodies = append(bodies, string(data))
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"key":"uploaded"}`))
	}))
	defer server.Close()

	body := &trackingReadSeeker{Reader: bytes.NewReader([]byte("skip:payload"))}
	if _, err := body.Seek(5, io.SeekStart); err != nil {
		t.Fatalf("seeking body: %v", err)
	}
	var delays []time.Duration
	client := &apiClient{
		baseURL:    server.URL,
		httpClient: server.Client(),
		sleep:      func(delay time.Duration) { delays = append(delays, delay) },
	}

	if _, err := client.postStream("/upload", body, "application/octet-stream"); err != nil {
		t.Fatalf("postStream: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	for i, got := range bodies {
		if got != "payload" {
			t.Fatalf("attempt %d body = %q, want payload", i+1, got)
		}
	}
	for i, got := range contentLengths {
		if got != int64(len("payload")) {
			t.Fatalf("attempt %d Content-Length = %d, want %d", i+1, got, len("payload"))
		}
	}
	if len(delays) != 1 || delays[0] != 250*time.Millisecond {
		t.Fatalf("retry delays = %v, want [250ms]", delays)
	}
	if body.closed {
		t.Fatal("postStream closed the caller-owned body")
	}
}

func TestPostStreamDoesNotRetryPermanentHTTPError(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()

	client := &apiClient{baseURL: server.URL, httpClient: server.Client(), sleep: func(time.Duration) {
		t.Fatal("unexpected retry")
	}}
	_, err := client.postRaw("/upload", []byte("payload"), "application/octet-stream")
	if err == nil {
		t.Fatal("postRaw succeeded, want HTTP error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestPostStreamStopsAfterThreeTransientFailures(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	var delays []time.Duration
	client := &apiClient{
		baseURL:    server.URL,
		httpClient: server.Client(),
		sleep:      func(delay time.Duration) { delays = append(delays, delay) },
	}
	_, err := client.postRaw("/upload", []byte("payload"), "application/octet-stream")
	if err == nil {
		t.Fatal("postRaw succeeded, want HTTP error")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	wantDelays := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}
	if len(delays) != len(wantDelays) || delays[0] != wantDelays[0] || delays[1] != wantDelays[1] {
		t.Fatalf("retry delays = %v, want %v", delays, wantDelays)
	}
}

func TestPostStreamDoesNotRetryNonSeekableBody(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "temporary", http.StatusInternalServerError)
	}))
	defer server.Close()

	body := struct{ io.Reader }{Reader: strings.NewReader("payload")}
	client := &apiClient{baseURL: server.URL, httpClient: server.Client(), sleep: func(time.Duration) {
		t.Fatal("unexpected retry")
	}}
	_, err := client.postStream("/upload", body, "application/octet-stream")
	if err == nil {
		t.Fatal("postStream succeeded, want HTTP error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

type trackingReadSeeker struct {
	*bytes.Reader
	closed bool
}

func (r *trackingReadSeeker) Close() error {
	r.closed = true
	return nil
}
