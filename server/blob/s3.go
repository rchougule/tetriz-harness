package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config addresses an S3-compatible bucket.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// Prefix is prepended to every object key.
	Prefix string
}

// S3 stores bodies as objects in an S3-compatible bucket.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 connects to the bucket, creating it when absent.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3 endpoint and bucket are required")
	}
	var creds *credentials.Credentials
	if cfg.AccessKey != "" {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	} else {
		creds = credentials.NewIAM("")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{Creds: creds, Secure: cfg.UseSSL, Region: cfg.Region})
	if err != nil {
		return nil, fmt.Errorf("creating s3 client: %w", err)
	}
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("checking bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("creating bucket: %w", err)
		}
	}
	return &S3{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *S3) key(hash string) string {
	return s.prefix + "blobs/" + hash[:2] + "/" + hash
}

func isNoSuchKey(err error) bool {
	return minio.ToErrorResponse(err).Code == minio.NoSuchKey
}

// Put uploads body; an object already at the key is left as is.
func (s *S3) Put(ctx context.Context, hash string, body []byte) error {
	if err := checkPut(hash, body); err != nil {
		return err
	}
	if ok, err := s.Has(ctx, hash); err != nil {
		return err
	} else if ok {
		return nil
	}
	if _, err := s.client.PutObject(ctx, s.bucket, s.key(hash), bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("uploading blob: %w", err)
	}
	return nil
}

// Get downloads a body.
func (s *S3) Get(ctx context.Context, hash string) ([]byte, error) {
	if err := ValidateHash(hash); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(hash), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetching blob: %w", err)
	}
	defer func() { _ = obj.Close() }()
	b, err := io.ReadAll(obj)
	if isNoSuchKey(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading blob: %w", err)
	}
	return b, nil
}

// Has reports whether a body is stored.
func (s *S3) Has(ctx context.Context, hash string) (bool, error) {
	if err := ValidateHash(hash); err != nil {
		return false, err
	}
	_, err := s.client.StatObject(ctx, s.bucket, s.key(hash), minio.StatObjectOptions{})
	if isNoSuchKey(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking blob: %w", err)
	}
	return true, nil
}
