// Package storage abstracts where file bytes live: local disk or Cloudflare R2.
// Uploads are chunked; keys are file slugs. Chunk indexes are 0-based and all
// chunks except the last must be exactly chunkSize bytes (required by R2).
package storage

import (
	"context"
	"io"
)

type Storage interface {
	// Init starts a chunked upload for key. The returned providerID must be
	// passed back to subsequent calls (empty for backends that don't need it).
	Init(ctx context.Context, key string, size int64) (providerID string, err error)
	// PutChunk stores one chunk. Returns an etag for backends that need it to
	// complete the upload.
	PutChunk(ctx context.Context, key, providerID string, idx int, chunkSize int64, r io.Reader, size int64) (etag string, err error)
	// Complete finalizes the upload. etags are ordered by chunk index.
	Complete(ctx context.Context, key, providerID string, etags []string) error
	// Abort discards a partial upload.
	Abort(ctx context.Context, key, providerID string) error
	// Open returns a reader over the finished file and its size.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
	// Delete removes a finished file.
	Delete(ctx context.Context, key string) error
}

// SpaceChecker is implemented by backends that can report free capacity, so
// callers can refuse an upload that would not fit before starting it.
type SpaceChecker interface {
	// FreeSpace returns the bytes available to store new files.
	FreeSpace() (int64, error)
}

// Presigner is implemented by backends that let browsers transfer bytes
// directly (bypassing this server) via presigned URLs.
type Presigner interface {
	// PresignPart returns a URL the client can PUT chunk idx to.
	PresignPart(ctx context.Context, key, providerID string, idx int) (string, error)
	// PresignGet returns a URL downloads can be redirected to. disposition
	// and contentType override the response headers the store sends.
	PresignGet(ctx context.Context, key, disposition, contentType string) (string, error)
}

// ObjectLister is implemented by backends that can enumerate finished object
// keys (used by orphan reconciliation during cleanup).
type ObjectLister interface {
	ListKeys(ctx context.Context) ([]string, error)
}
