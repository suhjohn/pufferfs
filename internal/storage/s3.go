// Package storage provides S3-compatible object storage operations.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

// Client wraps an S3 client with the configured bucket.
type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

type CompletedPart struct {
	PartNumber int32
	ETag       string
}

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
				cfg.SessionToken,
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

	return &Client{s3: client, presign: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

// PresignImmutablePut signs a create-only upload. Reusing the URL cannot
// overwrite a source pack after a version has started referencing it.
func (c *Client) PresignImmutablePut(ctx context.Context, key string, size int64) (string, map[string][]string, error) {
	result, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key),
		ContentLength: aws.Int64(size), ContentType: aws.String("application/octet-stream"),
		IfNoneMatch: aws.String("*"),
	}, func(options *s3.PresignOptions) { options.Expires = 15 * time.Minute })
	if err != nil {
		return "", nil, err
	}
	return result.URL, result.SignedHeader, nil
}

func (c *Client) ObjectSize(ctx context.Context, key string) (int64, error) {
	result, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(result.ContentLength), nil
}

func (c *Client) PresignMultipartPart(ctx context.Context, key, uploadID string, partNumber int32, contentLength int64, expires time.Duration) (string, map[string][]string, error) {
	if c.presign == nil {
		return "", nil, errors.New("object storage presigner is unavailable")
	}
	out, err := c.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:        &c.bucket,
		Key:           &key,
		UploadId:      &uploadID,
		PartNumber:    &partNumber,
		ContentLength: &contentLength,
	}, func(options *s3.PresignOptions) {
		options.Expires = expires
	})
	if err != nil {
		return "", nil, err
	}
	return out.URL, out.SignedHeader, nil
}

func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	_, err := c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   &c.bucket,
		Key:      &key,
		UploadId: &uploadID,
	})
	return err
}

// Upload puts an object into S3.
func (c *Client) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket, Key: &key, Body: bytes.NewReader(data), ContentType: &contentType,
	})
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
