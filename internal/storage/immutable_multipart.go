package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const sourceUploadIdentity = "pufferfs-upload-identity"

var ErrMultipartUploadExpired = errors.New("multipart session and completed object are absent")

// CheckImmutableMultipartUpload checks a persisted session before resuming.
// False/nil means active; true/nil means completed despite a lost response.
// Only NoSuchUpload plus an absent object proves that new upload bytes are
// needed. Access errors, throttling and transport failures never mean expired.
func (c *Client) CheckImmutableMultipartUpload(ctx context.Context, key, uploadID, identity string, size int64) (bool, error) {
	_, err := c.s3.ListParts(ctx, &s3.ListPartsInput{
		Bucket: &c.bucket, Key: &key, UploadId: &uploadID, MaxParts: aws.Int32(1),
	})
	if err == nil {
		return false, nil
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchUpload" {
		return false, err
	}
	err = c.verifyImmutableMultipartObject(ctx, key, identity, size)
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
		return false, ErrMultipartUploadExpired
	}
	return err == nil, err
}

// CreateImmutableMultipartUpload binds the object to a durable caller-generated
// identity. The caller must persist key/identity before creation and the returned
// upload ID before issuing part URLs. Generated/converted media must not use it.
func (c *Client) CreateImmutableMultipartUpload(ctx context.Context, key, identity string) (string, error) {
	if key == "" || identity == "" {
		return "", errors.New("immutable multipart key and identity are required")
	}
	out, err := c.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &c.bucket, Key: &key, ContentType: aws.String("application/octet-stream"),
		Metadata: map[string]string{sourceUploadIdentity: identity},
	})
	if err != nil {
		return "", err
	}
	if aws.ToString(out.UploadId) == "" {
		return "", errors.New("object storage returned an empty multipart upload id")
	}
	return *out.UploadId, nil
}

// CompleteImmutableMultipartUpload never overwrites or deletes an object.
// HEAD identity+size recognizes completion after a lost response, without
// accepting a different upload merely because its byte count happens to match.
// This checks upload identity, not content integrity: extraction verifies hashes.
func (c *Client) CompleteImmutableMultipartUpload(ctx context.Context, key, uploadID, identity string, size int64, parts []CompletedPart) error {
	if key == "" || uploadID == "" || identity == "" || size < 1 || len(parts) < 1 || len(parts) > 10000 {
		return errors.New("invalid immutable multipart completion")
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, part := range parts {
		if part.PartNumber != int32(i+1) || part.ETag == "" {
			return errors.New("multipart parts must be contiguous with nonempty ETags")
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(part.PartNumber), ETag: aws.String(part.ETag)}
	}
	_, completeErr := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &c.bucket, Key: &key, UploadId: &uploadID, IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err := c.verifyImmutableMultipartObject(ctx, key, identity, size); err != nil {
		return errors.Join(completeErr, err)
	}
	return nil
}

func (c *Client) verifyImmutableMultipartObject(ctx context.Context, key, identity string, size int64) error {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		return err
	}
	if out.ContentLength == nil || *out.ContentLength != size || out.Metadata[sourceUploadIdentity] != identity {
		return fmt.Errorf("immutable multipart object identity or size mismatch")
	}
	return nil
}
