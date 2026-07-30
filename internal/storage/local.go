package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Local struct {
	filesDir string
	tmpDir   string
}

func NewLocal(dataDir string) (*Local, error) {
	l := &Local{
		filesDir: filepath.Join(dataDir, "files"),
		tmpDir:   filepath.Join(dataDir, "tmp"),
	}
	for _, dir := range []string{l.filesDir, l.tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Local) Init(ctx context.Context, key string, size int64) (string, error) {
	f, err := os.Create(filepath.Join(l.tmpDir, key))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return "", f.Truncate(size)
}

func (l *Local) PutChunk(ctx context.Context, key, providerID string, idx int, chunkSize int64, r io.Reader, size int64) (string, error) {
	f, err := os.OpenFile(filepath.Join(l.tmpDir, key), os.O_WRONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	n, err := io.Copy(io.NewOffsetWriter(f, int64(idx)*chunkSize), r)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("chunk %d: wrote %d bytes, expected %d", idx, n, size)
	}
	return "", nil
}

func (l *Local) Complete(ctx context.Context, key, providerID string, etags []string) error {
	return os.Rename(filepath.Join(l.tmpDir, key), filepath.Join(l.filesDir, key))
}

func (l *Local) Abort(ctx context.Context, key, providerID string) error {
	err := os.Remove(filepath.Join(l.tmpDir, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *Local) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	f, err := os.Open(filepath.Join(l.filesDir, key))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	err := os.Remove(filepath.Join(l.filesDir, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ListKeys returns finished object keys under the files directory.
func (l *Local) ListKeys(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(l.filesDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}
