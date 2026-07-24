package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/pascalinthecloud/terrastrata/internal/config"
)

// s3API is the subset of the AWS S3 client terrastrata uses. Defining it as an
// interface keeps the S3 backend unit-testable with a fake.
type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3 is an S3-compatible Cache backend: the durable layer that survives pod
// restarts and is shared across replicas. Works with AWS S3, OVH, MinIO, etc.
type S3 struct {
	client s3API
	bucket string
	prefix string
}

// NewS3 builds an S3 cache from configuration. Static credentials are used when
// configured; otherwise the AWS default credential chain applies (environment,
// shared config, IRSA, instance profile). An optional custom endpoint enables
// path-style addressing, which MinIO and OVH require.
func NewS3(ctx context.Context, cfg config.S3Config) (*S3, error) {
	opts := s3.Options{Region: cfg.Region}
	if cfg.AccessKey != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	} else {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
		if err != nil {
			return nil, fmt.Errorf("cache: load AWS default credential chain: %w", err)
		}
		opts.Credentials = awsCfg.Credentials
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
		opts.UsePathStyle = true
	}
	return &S3{
		client: s3.New(opts),
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// objectKey prepends the configured prefix to a cache key.
func (s *S3) objectKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// Get implements Cache. A missing object yields hit == false and a nil error.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, bool, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache: s3 get %q: %w", key, err)
	}
	return out.Body, true, nil
}

// Put implements Cache, uploading the object under the prefixed key. When r is
// an io.ReadSeeker (e.g. an *os.File), the SDK derives the content length and
// avoids buffering; Layered passes the warmed local file for exactly this reason.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("cache: s3 put %q: %w", key, err)
	}
	return nil
}

// isNotFound reports whether err is an S3 "object absent" error. Different
// S3-compatible backends surface this as NoSuchKey or NotFound, so we match on
// the smithy API error code rather than a concrete type.
//
// NoSuchBucket is deliberately NOT treated as not-found: a missing/misconfigured
// bucket is an operator fault, and swallowing it as a cache miss would silently
// disable the durable cache forever. It must propagate as an error.
func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
