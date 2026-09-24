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
	"slices"
	"sync"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sync/errgroup"
)

func uploadJournalMultipart(ctx context.Context, client *apiClient, rootID string, pack *journalPack, source *os.File, save func() error, slots chan struct{}) error {
	if pack.Multipart == nil {
		pack.Multipart = &journalMultipart{RequestID: uuid.NewString()}
		if err := save(); err != nil {
			return err
		}
	}
	m := pack.Multipart
	if _, err := uuid.Parse(m.RequestID); err != nil {
		return errors.New("invalid multipart journal request ID")
	}
	endpoint := "/roots/" + url.PathEscape(rootID) + "/sources/multipart/"
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
		if pack.Complete {
			return errors.New("cannot restart an accepted multipart capture")
		}
		pack.ObjectKey = ""
		pack.Multipart = &journalMultipart{RequestID: uuid.NewString()}
		m = pack.Multipart
		if err = save(); err != nil {
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
	acknowledged := make(map[int32]bool, len(m.Parts))
	for _, part := range m.Parts {
		if part.PartNumber < 1 || part.PartNumber > int32(init.PartCount) || part.ETag == "" || acknowledged[part.PartNumber] {
			return errors.New("invalid multipart journal acknowledgement")
		}
		acknowledged[part.PartNumber] = true
	}
	pack.ObjectKey, m.UploadID, m.PartSize = init.ObjectKey, init.UploadID, init.PartSize
	pack.Complete = init.Complete
	if err = save(); err != nil {
		return err
	}
	if pack.Complete {
		return nil
	}
	var acknowledgements sync.Mutex
	group, transferContext := errgroup.WithContext(ctx)
	group.SetLimit(cap(slots))
	for number := int32(1); number <= int32(init.PartCount); number++ {
		if acknowledged[number] {
			continue
		}
		group.Go(func() error {
			var etag string
			err := captureTransfer(transferContext, slots, func() error {
				offset := int64(number-1) * m.PartSize
				size := min(m.PartSize, pack.Size-offset)
				body, err := client.postContext(transferContext, endpoint+"part", models.SourceMultipartPartRequest{ObjectKey: pack.ObjectKey, PartNumber: number})
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
				etag, err = client.putCapturedPart(transferContext, part, source, offset)
				return err
			})
			if err != nil {
				return err
			}
			acknowledgements.Lock()
			defer acknowledgements.Unlock()
			m.Parts = append(m.Parts, models.SourceMultipartPart{PartNumber: number, ETag: etag})
			slices.SortFunc(m.Parts, func(a, b models.SourceMultipartPart) int { return int(a.PartNumber - b.PartNumber) })
			// Persist every confirmed part, including a noncontiguous set, before
			// returning. Only an unacknowledged part needs replay after a crash.
			return save()
		})
	}
	if err = group.Wait(); err != nil {
		return err
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
	return save()
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
