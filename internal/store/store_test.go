package store_test

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"melodyshare/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestTryIncrementDownloadsRespectsMax(t *testing.T) {
	st := openTestStore(t)
	f := &store.File{
		Slug: "once", Name: "a.txt", Size: 1, Storage: "local",
		ContentType: "text/plain", CreatedAt: time.Now().Unix(), MaxDownloads: 1,
	}
	if err := st.CreateFile(f); err != nil {
		t.Fatal(err)
	}
	ok, err := st.TryIncrementDownloads(f.ID)
	if err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	ok, err = st.TryIncrementDownloads(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second increment should fail when max_downloads=1")
	}
	got, err := st.GetFileByID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Downloads != 1 {
		t.Fatalf("downloads = %d, want 1", got.Downloads)
	}
}

func TestTryIncrementDownloadsConcurrent(t *testing.T) {
	st := openTestStore(t)
	f := &store.File{
		Slug: "race", Name: "a.txt", Size: 1, Storage: "local",
		ContentType: "text/plain", CreatedAt: time.Now().Unix(), MaxDownloads: 1,
	}
	if err := st.CreateFile(f); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.TryIncrementDownloads(f.ID)
			if err != nil {
				t.Errorf("TryIncrementDownloads: %v", err)
				return
			}
			if ok {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successes = %d, want 1", successes.Load())
	}
}

func TestFinalizeUploadAtomic(t *testing.T) {
	st := openTestStore(t)
	up := &store.Upload{
		ID: "up1", Slug: "fin", Name: "a.txt", Size: 10, ChunkSize: 10,
		TotalChunks: 1, Storage: "local", CreatedAt: time.Now().Unix(),
	}
	if err := st.CreateUpload(up); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPart(up.ID, 0, "etag"); err != nil {
		t.Fatal(err)
	}
	f := &store.File{
		Slug: up.Slug, Name: up.Name, Size: up.Size, Storage: up.Storage,
		ContentType: "text/plain", CreatedAt: time.Now().Unix(),
	}
	if err := st.FinalizeUpload(f, up.ID); err != nil {
		t.Fatal(err)
	}
	if f.ID == 0 {
		t.Fatal("file id not set")
	}
	if _, err := st.GetUpload(up.ID); err != store.ErrNotFound {
		t.Fatalf("upload should be gone, err=%v", err)
	}
	got, err := st.GetFileBySlug(up.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a.txt" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestListExhaustedFiles(t *testing.T) {
	st := openTestStore(t)
	live := &store.File{
		Slug: "live", Name: "a.txt", Size: 1, Storage: "local",
		ContentType: "text/plain", CreatedAt: 1, MaxDownloads: 2, Downloads: 1,
	}
	done := &store.File{
		Slug: "done", Name: "b.txt", Size: 1, Storage: "local",
		ContentType: "text/plain", CreatedAt: 1, MaxDownloads: 1, Downloads: 1,
	}
	unlimited := &store.File{
		Slug: "free", Name: "c.txt", Size: 1, Storage: "local",
		ContentType: "text/plain", CreatedAt: 1, Downloads: 99,
	}
	for _, f := range []*store.File{live, done, unlimited} {
		if err := st.CreateFile(f); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.ListExhaustedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Slug != "done" {
		t.Fatalf("exhausted = %+v", list)
	}
}

func TestKnownObjectSlugs(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateFile(&store.File{
		Slug: "f1", Name: "a", Size: 1, Storage: "local", ContentType: "text/plain", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUpload(&store.Upload{
		ID: "u1", Slug: "u1", Name: "b", Size: 1, ChunkSize: 1, TotalChunks: 1,
		Storage: "local", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	known, err := st.KnownObjectSlugs()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["f1"]; !ok {
		t.Fatal("missing file slug")
	}
	if _, ok := known["u1"]; !ok {
		t.Fatal("missing upload slug")
	}
}
