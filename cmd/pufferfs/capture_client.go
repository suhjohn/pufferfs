package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func (c *apiClient) initSourcePack(ctx context.Context, rootID string, size int64, existingKey string) (models.SourcePackInitResponse, error) {
	var result models.SourcePackInitResponse
	if size < 1 || size > 128<<20 {
		return result, errors.New("source pack must contain 1..128 MiB")
	}
	input := models.SourcePackInitRequest{Size: size, ObjectKey: existingKey}
	body, err := c.postContext(ctx, "/roots/"+url.PathEscape(rootID)+"/sources/init", input)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if result.ObjectKey == "" || result.URL == "" || http.Header(result.Headers).Get("If-None-Match") != "*" {
		return result, errors.New("invalid immutable source upload authorization")
	}
	if input.ObjectKey != "" && result.ObjectKey != input.ObjectKey {
		return result, errors.New("source upload renewal changed object identity")
	}
	return result, nil
}

// Only captured immutable bytes belong here, never a live filesystem path.
// Caller owns the reader and persists the object key before attempting upload.
func (c *apiClient) putSourcePack(ctx context.Context, upload models.SourcePackInitResponse, source io.ReaderAt, size int64) error {
	if size < 1 || size > 128<<20 || http.Header(upload.Headers).Get("If-None-Match") != "*" {
		return errors.New("invalid immutable source pack")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.URL, io.NewSectionReader(source, 0, size))
	if err != nil {
		return errors.New("invalid source upload URL")
	}
	req.ContentLength = size
	req.Header = http.Header(upload.Headers).Clone()
	// Never forward the API bearer token or follow a redirect with signed headers.
	req.Header.Del("Authorization")
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// net/url errors include the signed URL; don't leak it into logs/journals.
		return errors.New("source pack upload transport failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusPreconditionFailed {
		// A lost PUT response can leave the immutable object already present.
		// This is not acceptance: completeSourcePack still must validate it.
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("source pack upload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *apiClient) completeSourcePack(ctx context.Context, rootID, key string) error {
	body, err := c.postContext(ctx, "/roots/"+url.PathEscape(rootID)+"/sources/complete", models.SourcePackCompleteRequest{ObjectKey: key})
	if err != nil {
		return err
	}
	var result models.SourcePackCompleteResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return err
	}
	if result.ObjectKey != key || result.Status != "complete" {
		return errors.New("source completion response does not match upload")
	}
	return nil
}

func (c *apiClient) registerCapturedVersions(ctx context.Context, rootID string, input models.CaptureVersionsRequest) (models.CaptureVersionsResponse, error) {
	var result models.CaptureVersionsResponse
	body, err := c.postContext(ctx, "/roots/"+url.PathEscape(rootID)+"/versions", input)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			var response struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(apiErr.Body, &response) == nil && response.Code == "capture_version_conflict" {
				return result, &captureVersionConflictError{CaptureID: input.CaptureID}
			}
		}
		return result, err
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if result.CaptureID != input.CaptureID || len(result.Versions) != len(input.Files) {
		return result, errors.New("capture acceptance response does not match request")
	}
	for _, version := range result.Versions {
		if version.FileID == "" || version.VersionID == "" || version.Sequence < 1 || version.ExtractionID == "" || version.WorkID == "" || (version.Stage != "transform" && version.Stage != "index") {
			return result, errors.New("invalid accepted file version")
		}
	}
	return result, nil
}

func (c *apiClient) walkCapturedFiles(ctx context.Context, rootID string, processing bool, visit func(models.CapturedFileHead) error) error {
	cursor := ""
	for {
		query := url.Values{"limit": {"500"}, "cursor": {cursor}}
		if processing {
			query.Set("processing", "true")
		}
		body, err := c.requestWithContext(ctx, http.MethodGet, "/roots/"+url.PathEscape(rootID)+"/captured-files?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		var page models.CapturedFilesResponse
		if err = json.Unmarshal(body, &page); err != nil {
			return err
		}
		last := cursor
		for _, file := range page.Files {
			if file.FileID <= last || file.Path == "" || file.VersionID == "" || file.Sequence < 1 {
				return errors.New("invalid or non-advancing captured catalog")
			}
			last = file.FileID
			if err = visit(file); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if page.NextCursor <= cursor || page.NextCursor < last {
			return errors.New("non-advancing captured catalog cursor")
		}
		cursor = page.NextCursor
	}
}
