// Package storage provides S3-compatible object storage operations.
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

// Client wraps an S3 client with the configured bucket.
type Client struct {
	s3     *s3.Client
	bucket string
}

// NewClient creates a new S3-compatible storage client.
func NewClient(cfg appconfig.StorageConfig) (*Client, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion("auto"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			),
		),
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

	return &Client{s3: client, bucket: cfg.Bucket}, nil
}

// Upload puts an object into S3.
func (c *Client) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &c.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	return err
}

// UploadFile uploads a local file to S3.
func (c *Client) UploadFile(ctx context.Context, key string, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
		Body:   f,
	})
	return err
}

// Download gets an object from S3.
func (c *Client) Download(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Delete removes an object from S3.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	return err
}

// Rename copies an object to a new key and deletes the old one.
func (c *Client) Rename(ctx context.Context, oldKey, newKey string) error {
	copySource := fmt.Sprintf("%s/%s", c.bucket, oldKey)
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     &c.bucket,
		CopySource: &copySource,
		Key:        &newKey,
	})
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", oldKey, newKey, err)
	}
	if err := c.Delete(ctx, oldKey); err != nil {
		return fmt.Errorf("rename delete %s after copy to %s: %w", oldKey, newKey, err)
	}
	return nil
}
