// Package storage provides S3-compatible object storage operations.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

// Client wraps an S3 client with the configured bucket.
type Client struct {
	s3       *s3.Client
	uploader streamUploader
	aborter  multipartAborter
	bucket   string
}

type streamUploader interface {
	Upload(context.Context, *s3.PutObjectInput, ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

type multipartAborter interface {
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

const multipartAbortTimeout = 30 * time.Second

// NewClient creates a new S3-compatible storage client.
func NewClient(cfg appconfig.StorageConfig) (*Client, error) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "auto"
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
			o.UsePathStyle = true
		}
	})
	uploader := newStreamUploader(client)

	return &Client{s3: client, uploader: uploader, aborter: client, bucket: cfg.Bucket}, nil
}

func newStreamUploader(client manager.UploadAPIClient, options ...func(*manager.Uploader)) *manager.Uploader {
	baseOptions := []func(*manager.Uploader){func(u *manager.Uploader) {
		// Keep the checksum behavior consistent with the S3 client. This also
		// avoids requiring optional checksum support from S3-compatible stores.
		u.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		// Bound per-request buffering and let UploadStream abort explicitly so
		// cleanup can use a context that survives client disconnection.
		u.Concurrency = 2
		u.LeavePartsOnError = true
	}}
	return manager.NewUploader(client, append(baseOptions, options...)...)
}

// Upload puts an object into S3.
func (c *Client) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	return c.UploadStream(ctx, key, bytes.NewReader(data), contentType)
}

func (c *Client) UploadStream(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      &c.bucket,
		Key:         &key,
		Body:        body,
		ContentType: &contentType,
	})
	if err == nil {
		return nil
	}

	var multipartFailure manager.MultiUploadFailure
	if !errors.As(err, &multipartFailure) || multipartFailure.UploadID() == "" || c.aborter == nil {
		return err
	}

	// Request cancellation is a common upload failure mode, but using the
	// canceled request context for cleanup would leave orphaned multipart
	// parts. Preserve context values while giving the abort a short deadline.
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), multipartAbortTimeout)
	defer cancel()
	_, abortErr := c.aborter.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   &c.bucket,
		Key:      &key,
		UploadId: aws.String(multipartFailure.UploadID()),
	})
	if abortErr != nil {
		return errors.Join(err, fmt.Errorf("aborting failed multipart upload: %w", abortErr))
	}
	return err
}

// Download gets an object from S3.
func (c *Client) Download(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Open streams an object, optionally limited to a byte range.
func (c *Client) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("invalid object range offset=%d length=%d", offset, length)
	}
	input := &s3.GetObjectInput{Bucket: &c.bucket, Key: &key}
	if length > 0 {
		if offset > math.MaxInt64-length+1 {
			return nil, fmt.Errorf("invalid object range offset=%d length=%d", offset, length)
		}
		rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		input.Range = &rangeHeader
	}
	resp, err := c.s3.GetObject(ctx, input)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DeleteMany removes objects from S3 in one batch.
func (c *Client) DeleteMany(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += 1000 {
		end := min(start+1000, len(keys))
		objects := make([]types.ObjectIdentifier, 0, end-start)
		for _, key := range keys[start:end] {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
		}
		output, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &c.bucket,
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		if len(output.Errors) > 0 {
			failure := output.Errors[0]
			return fmt.Errorf("deleting %d objects: %s (%s): %s", len(output.Errors), aws.ToString(failure.Key), aws.ToString(failure.Code), aws.ToString(failure.Message))
		}
	}
	return nil
}

func (c *Client) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, nil
	}
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: &c.bucket,
		Prefix: &prefix,
	})
	deleted := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, err
		}
		keys := make([]string, 0, len(page.Contents))
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			keys = append(keys, *obj.Key)
		}
		if err := c.DeleteMany(ctx, keys); err != nil {
			return deleted, err
		}
		deleted += len(keys)
	}
	return deleted, nil
}
