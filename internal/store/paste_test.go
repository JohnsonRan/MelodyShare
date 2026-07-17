package store_test

import (
	"errors"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"melodyshare/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPasteCRUD(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	p := &store.Paste{Slug: store.NewPasteSlug(), Content: "hello 剪切板", CreatedAt: now, ExpiresAt: now + 7200}
	if err := st.CreatePaste(p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 {
		t.Fatal("CreatePaste did not set ID")
	}

	got, err := st.GetPasteBySlug(p.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != p.Content || got.ExpiresAt != p.ExpiresAt {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	byID, err := st.GetPasteByID(p.ID)
	if err != nil || byID.Slug != p.Slug {
		t.Fatalf("by id: err=%v got=%+v", err, byID)
	}

	list, err := st.ListPastes()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}

	if err := st.DeletePaste(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPasteBySlug(p.Slug); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestPasteSlugUnique(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	if err := st.CreatePaste(&store.Paste{Slug: "aaaaa", Content: "1", CreatedAt: now, ExpiresAt: now + 60}); err != nil {
		t.Fatal(err)
	}
	err := st.CreatePaste(&store.Paste{Slug: "aaaaa", Content: "2", CreatedAt: now, ExpiresAt: now + 60})
	if !store.IsUniqueViolation(err) {
		t.Fatalf("want unique violation, got %v", err)
	}
}

func TestDeleteExpiredPastes(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	live := &store.Paste{Slug: "live1", Content: "x", CreatedAt: now, ExpiresAt: now + 3600}
	dead := &store.Paste{Slug: "dead1", Content: "x", CreatedAt: now - 7200, ExpiresAt: now - 10}
	for _, p := range []*store.Paste{live, dead} {
		if err := st.CreatePaste(p); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.DeleteExpiredPastes(now)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredPastes: n=%d err=%v", n, err)
	}
	if _, err := st.GetPasteBySlug("live1"); err != nil {
		t.Fatalf("live paste was deleted: %v", err)
	}
	if _, err := st.GetPasteBySlug("dead1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dead paste still there: %v", err)
	}
}

func TestNewPasteSlugFormat(t *testing.T) {
	re := regexp.MustCompile(`^[abcdefghjkmnpqrstuvwxyz23456789]{5}$`)
	for i := 0; i < 100; i++ {
		if s := store.NewPasteSlug(); !re.MatchString(s) {
			t.Fatalf("bad slug %q", s)
		}
	}
}
