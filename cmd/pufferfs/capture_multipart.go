package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func uploadJournalMultipart(ctx context.Context, client *apiClient, dir string, journal *captureJournal, pack *journalPack, source *os.File) error {
	if pack.Multipart == nil {
		pack.Multipart = &journalMultipart{RequestID: uuid.NewString()}
		if err := saveCaptureJournal(dir, *journal); err != nil {
			return err
		}
	}
	m := pack.Multipart
	if _, err := uuid.Parse(m.RequestID); err != nil {
		return errors.New("invalid multipart journal request ID")
	}
	endpoint := "/roots/" + url.PathEscape(journal.RootID) + "/sources/multipart/"
	var body []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		body, err = client.postContext(ctx, endpoint+"init", models.SourceMultipartInitRequest{RequestID: m.RequestID, Size: pack.Size})
		if err == nil {
			break
		}
		var apiErr *apiError
		var response struct {
			Code string `json:"code"`
		}
		if attempt != 0 || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict ||
			json.Unmarshal(apiErr.Body, &response) != nil || response.Code != models.SourceMultipartExpired {
			return err
		}
		// The server proved that no completed object exists. Retain the exact
		// pack and capture ID, but never reuse an expired S3 key, upload ID or
		// part acknowledgement. Persist the replacement before any network IO.
		if pack.Complete || journal.Accepted != nil {
			return errors.New("cannot restart an accepted multipart capture")
		}
		pack.ObjectKey = ""
		pack.Multipart = &journalMultipart{RequestID: uuid.NewString()}
		m = pack.Multipart
		if err = saveCaptureJournal(dir, *journal); err != nil {
			return err
		}
	}
	var init models.SourceMultipartInitResponse
	if err = json.Unmarshal(body, &init); err != nil {
		return err
	}
	if init.ObjectKey == "" || init.UploadID == "" || init.PartSize != 16<<20 || init.PartCount != int((pack.Size+init.PartSize-1)/init.PartSize) || init.PartCount < 1 || init.PartCount > 8 {
		return errors.New("invalid multipart initialization response")
	}
	if (pack.ObjectKey != "" && pack.ObjectKey != init.ObjectKey) || (m.UploadID != "" && m.UploadID != init.UploadID) || (m.PartSize != 0 && m.PartSize != init.PartSize) {
		return errors.New("multipart retry changed upload identity")
	}
	if len(m.Parts) > init.PartCount {
		return errors.New("invalid multipart journal parts")
	}
	for i, part := range m.Parts {
		if part.PartNumber != int32(i+1) || part.ETag == "" {
			return errors.New("invalid multipart journal acknowledgement")
		}
	}
	pack.ObjectKey, m.UploadID, m.PartSize = init.ObjectKey, init.UploadID, init.PartSize
	pack.Complete = init.Complete
	if err = saveCaptureJournal(dir, *journal); err != nil {
		return err
	}
	if pack.Complete {
		return nil
	}
	for len(m.Parts) < init.PartCount {
		number := int32(len(m.Parts) + 1)
		offset := int64(number-1) * m.PartSize
		size := min(m.PartSize, pack.Size-offset)
		body, err = client.postContext(ctx, endpoint+"part", models.SourceMultipartPartRequest{ObjectKey: pack.ObjectKey, PartNumber: number})
		if err != nil {
			return err
		}
		var part models.SourceMultipartPartResponse
		if err = json.Unmarshal(body, &part); err != nil {
			return err
		}
		if part.URL == "" || part.Size != size {
			return errors.New("multipart part authorization has wrong size")
		}
		etag, err := client.putCapturedPart(ctx, part, source, offset)
		if err != nil {
			return err
		}
		m.Parts = append(m.Parts, models.SourceMultipartPart{PartNumber: number, ETag: etag})
		// Persist each acknowledgement before advancing. A lost PUT response can
		// replay only that part from the same immutable local bytes.
		if err = saveCaptureJournal(dir, *journal); err != nil {
			return err
		}
	}
	body, err = client.postContext(ctx, endpoint+"complete", models.SourceMultipartCompleteRequest{ObjectKey: pack.ObjectKey, Parts: m.Parts})
	if err != nil {
		return err
	}
	var complete models.SourcePackCompleteResponse
	if err = json.Unmarshal(body, &complete); err != nil {
		return err
	}
	if complete.ObjectKey != pack.ObjectKey || complete.Status != "complete" {
		return errors.New("multipart completion response mismatch")
	}
	pack.Complete = true
	return saveCaptureJournal(dir, *journal)
}

func (c *apiClient) putCapturedPart(ctx context.Context, part models.SourceMultipartPartResponse, source io.ReaderAt, offset int64) (string, error) {
	if offset < 0 || part.Size < 1 || part.Size > 16<<20 {
		return "", errors.New("invalid multipart part range")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, part.URL, io.NewSectionReader(source, offset, part.Size))
	if err != nil {
		return "", errors.New("invalid multipart part URL")
	}
	req.ContentLength = part.Size
	req.Header = http.Header(part.Headers).Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Del("Authorization")
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("multipart part upload transport failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("multipart part upload returned HTTP %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || len(etag) > 1024 {
		return "", errors.New("multipart part upload returned invalid ETag")
	}
	return etag, nil
}
