package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadline  time.Time
	writeDeadline time.Time
	readCalls     int
	writeCalls    int
}

func (r *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	r.readDeadline = deadline
	r.readCalls++
	return nil
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.writeDeadline = deadline
	r.writeCalls++
	return nil
}

func TestPrepareStreamingUploadUsesSlidingDeadlineAndCapsBody(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("12345"))
	r.ContentLength = -1

	if ok := prepareStreamingUpload(w, r, 4); !ok {
		t.Fatal("prepareStreamingUpload rejected an unknown-length body")
	}

	if w.readCalls != 1 {
		t.Fatalf("SetReadDeadline calls = %d, want 1", w.readCalls)
	}
	if !w.readDeadline.IsZero() {
		t.Fatalf("initial read deadline = %v, want zero", w.readDeadline)
	}
	if w.writeCalls != 1 {
		t.Fatalf("SetWriteDeadline calls = %d, want 1", w.writeCalls)
	}
	if !w.writeDeadline.IsZero() {
		t.Fatalf("write deadline = %v, want zero", w.writeDeadline)
	}
	_, err := io.ReadAll(r.Body)
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("reading oversized body error = %v, want *http.MaxBytesError", err)
	}
	if w.readCalls < 3 {
		t.Fatalf("SetReadDeadline calls after reading = %d, want deadline refresh", w.readCalls)
	}
	if !w.readDeadline.IsZero() {
		t.Fatalf("read deadline after reading = %v, want zero", w.readDeadline)
	}
}

func TestPrepareStreamingUploadRejectsKnownOversizedBodyBeforeReading(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("12345"))

	if ok := prepareStreamingUpload(w, r, 4); ok {
		t.Fatal("prepareStreamingUpload accepted a known oversized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if w.readCalls != 0 || w.writeCalls != 0 {
		t.Fatalf("deadline calls for rejected body: read=%d write=%d, want 0", w.readCalls, w.writeCalls)
	}
}

func TestStreamingUploadBodyClassifiesIdleTimeout(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/upload", timeoutReadCloser{})
	r.ContentLength = -1
	if ok := prepareStreamingUpload(w, r, 4); !ok {
		t.Fatal("prepareStreamingUpload rejected body")
	}

	_, err := r.Body.Read(make([]byte, 1))
	if !errors.Is(err, errUploadBodyTimeout) {
		t.Fatalf("Read error = %v, want upload body timeout", err)
	}
}

func TestWriteUploadFailureReturnsContentTooLarge(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/upload", nil)
	err := errors.Join(errors.New("multipart upload failed"), &http.MaxBytesError{Limit: 4})

	writeUploadFailure(w, r, err)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWriteUploadFailureReturnsRequestTimeoutForIdleBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/upload", nil)

	writeUploadFailure(w, r, errUploadBodyTimeout)

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestTimeout)
	}
}

type timeoutReadCloser struct{}

func (timeoutReadCloser) Read([]byte) (int, error) { return 0, timeoutError{} }
func (timeoutReadCloser) Close() error             { return nil }

type timeoutError struct{}

func (timeoutError) Error() string { return "timeout" }
func (timeoutError) Timeout() bool { return true }
