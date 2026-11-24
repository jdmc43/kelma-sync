// Package storage abstracts blob storage for media files. The production
// implementation targets Cloudflare R2 via its S3-compatible API.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Storage is the blob store interface used by media handlers.
type ObjectInfo struct {
	SizeBytes   int64
	ContentType string
}

type Storage interface {
	// Put stores an object and returns its storage key.
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	// Stat verifies an object without downloading its body.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Get retrieves an object as a stream. Caller must close it.
	Get(ctx context.Context, key string) (io.ReadCloser, string, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object whose key starts with prefix. It is used
	// for complete account erasure and must also remove orphaned objects that no
	// longer have database metadata.
	DeletePrefix(ctx context.Context, prefix string) error
}

// R2 implements Storage against Cloudflare R2 (S3-compatible).
type R2 struct {
	client *s3.Client
	bucket string
}

// NewR2 builds an R2 client from environment variables:
//
//	R2_ACCOUNT_ID, R2_ACCESS_KEY, R2_SECRET_KEY, R2_BUCKET
func NewR2(ctx context.Context) (*R2, error) {
	return NewR2WithHTTPClient(ctx, nil)
}

// NewR2WithHTTPClient pins every SDK request to an explicitly reviewed client.
// The sandbox uses this instead of relying on process-global transport defaults.
func NewR2WithHTTPClient(_ context.Context, httpClient s3.HTTPClient) (*R2, error) {
	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKey := os.Getenv("R2_ACCESS_KEY")
	secretKey := os.Getenv("R2_SECRET_KEY")
	bucket := os.Getenv("R2_BUCKET")
	if accountID == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, fmt.Errorf("R2_ACCOUNT_ID, R2_ACCESS_KEY, R2_SECRET_KEY, R2_BUCKET must be set")
	}

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		HTTPClient:   httpClient,
	})
	return &R2{client: client, bucket: bucket}, nil
}

func (r *R2) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	return err
}

func (r *R2) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var responseError *smithyhttp.ResponseError
		if errors.As(err, &responseError) && responseError.HTTPStatusCode() == 404 {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	info := ObjectInfo{}
	if out.ContentLength != nil {
		info.SizeBytes = *out.ContentLength
	}
	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}
	return info, nil
}

func (r *R2) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Map a missing object to the shared ErrNotFound so handlers return a
		// 404 (letting clients self-heal by re-uploading) instead of a 500.
		var nsk *types.NoSuchKey
		var nf *types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return out.Body, ct, nil
}

func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (r *R2) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return errors.New("storage: refusing to delete an empty prefix")
	}
	for {
		listed, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(r.bucket),
			Prefix: aws.String(prefix),
		})
		if err != nil {
			return err
		}
		if len(listed.Contents) == 0 {
			return nil
		}
		objects := make([]types.ObjectIdentifier, 0, len(listed.Contents))
		for _, object := range listed.Contents {
			if object.Key != nil {
				objects = append(objects, types.ObjectIdentifier{Key: object.Key})
			}
		}
		if len(objects) == 0 {
			return errors.New("storage: R2 returned objects without keys")
		}
		deleted, err := r.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(r.bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		if len(deleted.Errors) > 0 {
			return fmt.Errorf("storage: R2 failed to delete %d objects", len(deleted.Errors))
		}
	}
}
