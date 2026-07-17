package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type R2 struct {
	core   *minio.Core
	bucket string
}

func NewR2(endpoint, accessKey, secretKey, bucket string) (*R2, error) {
	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: true,
		Region: "auto",
	})
	if err != nil {
		return nil, err
	}
	return &R2{core: core, bucket: bucket}, nil
}

func (r2 *R2) Init(ctx context.Context, key string, size int64) (string, error) {
	return r2.core.NewMultipartUpload(ctx, r2.bucket, key, minio.PutObjectOptions{})
}

func (r2 *R2) PutChunk(ctx context.Context, key, providerID string, idx int, chunkSize int64, r io.Reader, size int64) (string, error) {
	part, err := r2.core.PutObjectPart(ctx, r2.bucket, key, providerID, idx+1, r, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (r2 *R2) Complete(ctx context.Context, key, providerID string, etags []string) error {
	parts := make([]minio.CompletePart, len(etags))
	for i, etag := range etags {
		parts[i] = minio.CompletePart{PartNumber: i + 1, ETag: etag}
	}
	_, err := r2.core.CompleteMultipartUpload(ctx, r2.bucket, key, providerID, parts, minio.PutObjectOptions{})
	return err
}

func (r2 *R2) Abort(ctx context.Context, key, providerID string) error {
	return r2.core.AbortMultipartUpload(ctx, r2.bucket, key, providerID)
}

func (r2 *R2) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	stat, err := r2.core.Client.StatObject(ctx, r2.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	obj, err := r2.core.Client.GetObject(ctx, r2.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	return obj, stat.Size, nil
}

// CheckBucket verifies the credentials can reach the bucket.
func (r2 *R2) CheckBucket(ctx context.Context) error {
	exists, err := r2.core.Client.BucketExists(ctx, r2.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("bucket %q 不存在或凭据无权访问", r2.bucket)
	}
	return nil
}

// PresignPart returns a presigned UploadPart URL so the browser can PUT the
// chunk straight to R2.
func (r2 *R2) PresignPart(ctx context.Context, key, providerID string, idx int) (string, error) {
	params := url.Values{}
	params.Set("partNumber", strconv.Itoa(idx+1))
	params.Set("uploadId", providerID)
	u, err := r2.core.Client.Presign(ctx, http.MethodPut, r2.bucket, key, time.Hour, params)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// PresignGet returns a presigned GET URL so downloads stream from R2 instead
// of through this server. R2 egress is free; presigned URLs honor Range.
func (r2 *R2) PresignGet(ctx context.Context, key, disposition, contentType string) (string, error) {
	params := url.Values{}
	params.Set("response-content-disposition", disposition)
	params.Set("response-content-type", contentType)
	u, err := r2.core.Client.PresignedGetObject(ctx, r2.bucket, key, 15*time.Minute, params)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (r2 *R2) Delete(ctx context.Context, key string) error {
	return r2.core.Client.RemoveObject(ctx, r2.bucket, key, minio.RemoveObjectOptions{})
}
