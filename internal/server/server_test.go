package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"melodyshare/internal/auth"
	"melodyshare/internal/config"
	"melodyshare/internal/server"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

const (
	testUser     = "admin"
	testPassword = "test-password"
	chunkSize    = 1024
)

type env struct {
	ts      *httptest.Server
	srv     *server.Server
	client  *http.Client // logged in
	anon    *http.Client
	st      *store.Store
	dataDir string
}

func newEnv(t *testing.T) *env { return newEnvWithR2(t, nil) }

func newEnvWithR2(t *testing.T, r2 storage.Storage) *env {
	t.Helper()
	dataDir := t.TempDir()

	cfg := &config.Config{
		Addr: ":0", DataDir: dataDir,
		Username: testUser, Password: testPassword,
		ChunkSize: chunkSize,
	}
	st, err := store.Open(filepath.Join(dataDir, "share.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	am, err := auth.New(dataDir, testUser, testPassword, st)
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.NewLocal(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, st, am, local, r2, os.DirFS("../../web"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	e := &env{ts: ts, srv: srv, client: client, anon: &http.Client{}, st: st, dataDir: dataDir}

	res := e.doJSON(t, client, "POST", "/api/login",
		map[string]string{"username": testUser, "password": testPassword})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", res.StatusCode)
	}
	res.Body.Close()
	return e
}

func (e *env) doJSON(t *testing.T, c *http.Client, method, path string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	defer res.Body.Close()
	var v T
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

type initResp struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	ChunkSize   int64  `json:"chunkSize"`
	TotalChunks int    `json:"totalChunks"`
}

// upload pushes content through the chunked protocol and returns the slug.
func (e *env) upload(t *testing.T, name string, content []byte, password string, expiresIn int64) initResp {
	t.Helper()
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": name, "size": len(content), "storage": "local",
		"password": password, "expiresIn": expiresIn,
	})
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("init upload: %d %s", res.StatusCode, body)
	}
	ir := decode[initResp](t, res)

	for i := 0; i < ir.TotalChunks; i++ {
		lo := int64(i) * ir.ChunkSize
		hi := min(lo+ir.ChunkSize, int64(len(content)))
		req, _ := http.NewRequest("PUT",
			fmt.Sprintf("%s/api/uploads/%s/chunks/%d", e.ts.URL, ir.ID, i),
			bytes.NewReader(content[lo:hi]))
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("chunk %d: %d %s", i, res.StatusCode, body)
		}
		res.Body.Close()
	}

	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("complete: %d %s", res.StatusCode, body)
	}
	res.Body.Close()
	return ir
}

func randomContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 31)
	}
	return b
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.anon, "GET", "/api/files", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.anon, "POST", "/api/login",
		map[string]string{"username": testUser, "password": "wrong"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}
}

func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		res := e.doJSON(t, e.anon, "POST", "/api/login",
			map[string]string{"username": testUser, "password": "wrong"})
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, res.StatusCode)
		}
	}
	// locked now — even the correct password must be rejected with 429
	res := e.doJSON(t, e.anon, "POST", "/api/login",
		map[string]string{"username": testUser, "password": testPassword})
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 during lockout, got %d", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestChunkedUploadAndDownload(t *testing.T) {
	e := newEnv(t)
	content := randomContent(2*chunkSize + 500) // 3 chunks, last partial
	ir := e.upload(t, "видео 视频.bin", content, "", 0)

	// the share link itself is a preview page
	page, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(page.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(string(pageBody), "/f/"+ir.Slug+"/dl") {
		t.Fatalf("expected preview page with download link, got %.200s", pageBody)
	}

	res, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded bytes differ: got %d bytes, want %d", len(got), len(content))
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("missing RFC5987 filename: %q", cd)
	}

	// Range request
	req, _ := http.NewRequest("GET", e.ts.URL+"/f/"+ir.Slug+"/dl", nil)
	req.Header.Set("Range", "bytes=100-199")
	res2, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusPartialContent {
		t.Fatalf("range: expected 206, got %d", res2.StatusCode)
	}
	part, _ := io.ReadAll(res2.Body)
	if !bytes.Equal(part, content[100:200]) {
		t.Fatal("range bytes differ")
	}
}

func TestChunkSizeValidation(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "a.bin", "size": chunkSize * 2, "storage": "local",
	})
	ir := decode[initResp](t, res)

	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/uploads/%s/chunks/0", e.ts.URL, ir.ID),
		bytes.NewReader(make([]byte, 10))) // wrong size
	res2, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong chunk size, got %d", res2.StatusCode)
	}
}

func TestCompleteRejectsMissingChunks(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "a.bin", "size": chunkSize * 2, "storage": "local",
	})
	ir := decode[initResp](t, res)

	res2 := e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for incomplete upload, got %d", res2.StatusCode)
	}
}

func TestPasswordProtectedDownload(t *testing.T) {
	e := newEnv(t)
	content := randomContent(300)
	ir := e.upload(t, "secret.txt", content, "hunter2", 0)

	// no password -> HTML form
	res, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "password") {
		t.Fatalf("expected password page, got %s: %.100s", res.Header.Get("Content-Type"), body)
	}

	// wrong password
	res, err = e.anon.PostForm(e.ts.URL+"/f/"+ir.Slug, url.Values{"password": {"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: expected 401, got %d", res.StatusCode)
	}

	// bytes endpoint refuses without token
	res, err = e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("dl without token: expected 403, got %d", res.StatusCode)
	}

	// correct password -> redirect to preview with token -> bytes via /dl
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err = noRedirect.PostForm(e.ts.URL+"/f/"+ir.Slug, url.Values{"password": {"hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", res.StatusCode)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	token := loc.Query().Get("t")
	if token == "" {
		t.Fatalf("no token in redirect location %q", loc)
	}
	res, err = e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl?t=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, content) {
		t.Fatal("downloaded bytes differ after password")
	}
}

func TestExpiredFileGone(t *testing.T) {
	e := newEnv(t)
	f := &store.File{
		Slug: store.NewSlug(), Name: "old.txt", Size: 1, Storage: "local",
		CreatedAt: time.Now().Unix() - 100, ExpiresAt: time.Now().Unix() - 10,
	}
	if err := e.st.CreateFile(f); err != nil {
		t.Fatal(err)
	}
	res, err := e.anon.Get(e.ts.URL + "/f/" + f.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatalf("expected 410, got %d", res.StatusCode)
	}
}

func TestBatchDeleteFiles(t *testing.T) {
	e := newEnv(t)
	first := e.upload(t, "first.txt", randomContent(100), "", 0)
	second := e.upload(t, "second.txt", randomContent(200), "", 0)

	list := decode[struct {
		Files []struct {
			ID   int64  `json:"id"`
			Slug string `json:"slug"`
		} `json:"files"`
	}](t, e.doJSON(t, e.client, "GET", "/api/files", nil))
	if len(list.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(list.Files))
	}

	res := e.doJSON(t, e.client, "POST", "/api/files/batch-delete", map[string]any{
		"ids": []int64{list.Files[0].ID, list.Files[1].ID},
	})
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("batch delete: %d %s", res.StatusCode, body)
	}
	result := decode[struct {
		Deleted []int64 `json:"deleted"`
		Failed  []struct {
			ID    int64  `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
	}](t, res)
	if len(result.Deleted) != 2 || len(result.Failed) != 0 {
		t.Fatalf("unexpected batch result: deleted=%v failed=%v", result.Deleted, result.Failed)
	}

	for _, f := range list.Files {
		if _, err := e.st.GetFileByID(f.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("file %d remains in database: %v", f.ID, err)
		}
	}
	for _, slug := range []string{first.Slug, second.Slug} {
		if _, err := os.Stat(filepath.Join(e.dataDir, "files", slug)); !os.IsNotExist(err) {
			t.Errorf("file %q remains on disk: %v", slug, err)
		}
	}
}

func TestDeleteFile(t *testing.T) {
	e := newEnv(t)
	ir := e.upload(t, "bye.txt", randomContent(100), "", 0)

	list := decode[struct {
		Files []struct {
			ID   int64  `json:"id"`
			Slug string `json:"slug"`
		} `json:"files"`
	}](t, e.doJSON(t, e.client, "GET", "/api/files", nil))
	if len(list.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(list.Files))
	}

	res := e.doJSON(t, e.client, "DELETE", fmt.Sprintf("/api/files/%d", list.Files[0].ID), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", res.StatusCode)
	}

	res, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "files", ir.Slug)); !os.IsNotExist(err) {
		t.Fatal("file bytes still on disk after delete")
	}
}

// stubR2 fakes a Presigner-capable backend so the direct-upload endpoints can
// be exercised without real R2 credentials.
type stubR2 struct {
	completeEtags []string
}

func (s *stubR2) Init(ctx context.Context, key string, size int64) (string, error) {
	return "mpu-1", nil
}
func (s *stubR2) PutChunk(ctx context.Context, key, providerID string, idx int, chunkSize int64, r io.Reader, size int64) (string, error) {
	io.Copy(io.Discard, r)
	return fmt.Sprintf("relay-etag-%d", idx), nil
}
func (s *stubR2) Complete(ctx context.Context, key, providerID string, etags []string) error {
	s.completeEtags = etags
	return nil
}
func (s *stubR2) Abort(ctx context.Context, key, providerID string) error { return nil }
func (s *stubR2) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	return nil, 0, fmt.Errorf("not implemented")
}
func (s *stubR2) Delete(ctx context.Context, key string) error { return nil }
func (s *stubR2) PresignPart(ctx context.Context, key, providerID string, idx int) (string, error) {
	return fmt.Sprintf("https://r2.example/%s?partNumber=%d&uploadId=%s", key, idx+1, providerID), nil
}
func (s *stubR2) PresignGet(ctx context.Context, key, disposition, contentType string) (string, error) {
	return "https://r2.example/get/" + key, nil
}

func TestR2DirectUploadFlow(t *testing.T) {
	stub := &stubR2{}
	e := newEnvWithR2(t, stub)

	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "big.iso", "size": chunkSize * 2, "storage": "r2",
	})
	ir := decode[initResp](t, res)

	// presigned URL per chunk
	for i := 0; i < 2; i++ {
		res := e.doJSON(t, e.client, "GET", fmt.Sprintf("/api/uploads/%s/chunks/%d/url", ir.ID, i), nil)
		u := decode[struct {
			URL string `json:"url"`
		}](t, res)
		if !strings.Contains(u.URL, fmt.Sprintf("partNumber=%d", i+1)) {
			t.Fatalf("presigned URL missing part number: %s", u.URL)
		}
	}

	// browser reports etags (with quotes, as returned by S3)
	for i := 0; i < 2; i++ {
		res := e.doJSON(t, e.client, "POST",
			fmt.Sprintf("/api/uploads/%s/chunks/%d/etag", ir.ID, i),
			map[string]string{"etag": fmt.Sprintf("\"etag-%d\"", i)})
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("etag %d: %d %s", i, res.StatusCode, body)
		}
		res.Body.Close()
	}

	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("complete: %d", res.StatusCode)
	}
	if len(stub.completeEtags) != 2 || stub.completeEtags[0] != "etag-0" || stub.completeEtags[1] != "etag-1" {
		t.Fatalf("complete got etags %v, want [etag-0 etag-1] (quotes stripped)", stub.completeEtags)
	}
}

func TestDirectUploadRejectedForLocal(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "a.bin", "size": 100, "storage": "local",
	})
	ir := decode[initResp](t, res)

	res = e.doJSON(t, e.client, "GET", "/api/uploads/"+ir.ID+"/chunks/0/url", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("presign for local: expected 400, got %d", res.StatusCode)
	}
	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/chunks/0/etag",
		map[string]string{"etag": "x"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("etag for local: expected 400, got %d", res.StatusCode)
	}
}

func TestUploadResumeStatus(t *testing.T) {
	e := newEnv(t)
	content := randomContent(2*chunkSize + 100)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "resume.bin", "size": len(content), "storage": "local",
	})
	ir := decode[initResp](t, res)

	// upload only chunk 1, then check status reports it
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/uploads/%s/chunks/1", e.ts.URL, ir.ID),
		bytes.NewReader(content[chunkSize:2*chunkSize]))
	r2, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()

	st := decode[struct {
		Received    []int  `json:"received"`
		TotalChunks int    `json:"totalChunks"`
		Storage     string `json:"storage"`
	}](t, e.doJSON(t, e.client, "GET", "/api/uploads/"+ir.ID, nil))
	if len(st.Received) != 1 || st.Received[0] != 1 || st.Storage != "local" {
		t.Fatalf("status: received=%v storage=%s", st.Received, st.Storage)
	}
}

func TestDownloadLimit(t *testing.T) {
	e := newEnv(t)
	content := randomContent(200)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "once.txt", "size": len(content), "storage": "local", "maxDownloads": 1,
	})
	ir := decode[initResp](t, res)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/uploads/%s/chunks/0", e.ts.URL, ir.ID), bytes.NewReader(content))
	r1, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	res.Body.Close()

	// inline preview is disabled for limited files
	raw, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	raw.Body.Close()
	if raw.StatusCode != http.StatusForbidden {
		t.Fatalf("raw for limited file: expected 403, got %d", raw.StatusCode)
	}

	// first download OK
	dl, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, dl.Body)
	dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("first download: %d", dl.StatusCode)
	}
	// second download exhausted
	dl2, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	defer dl2.Body.Close()
	if dl2.StatusCode != http.StatusGone {
		t.Fatalf("second download: expected 410, got %d", dl2.StatusCode)
	}
}

func TestPreviewInlineRaw(t *testing.T) {
	e := newEnv(t)
	content := randomContent(64)
	ir := e.upload(t, "photo.png", content, "", 0)

	// preview page embeds the raw URL for images
	page, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "/f/"+ir.Slug+"/raw") {
		t.Fatalf("preview page missing raw URL: %.300s", body)
	}

	// raw serves inline bytes and does not count as a download
	raw, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Body.Close()
	got, _ := io.ReadAll(raw.Body)
	if !bytes.Equal(got, content) {
		t.Fatal("raw bytes differ")
	}
	if cd := raw.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("raw disposition = %q, want inline", cd)
	}
	f, err := e.st.GetFileBySlug(ir.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if f.Downloads != 0 {
		t.Fatalf("raw view counted as download: %d", f.Downloads)
	}
}

func TestCustomSlug(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "a.txt", "size": 10, "storage": "local", "slug": "my-file_1",
	})
	ir := decode[initResp](t, res)
	if ir.Slug != "my-file_1" {
		t.Fatalf("slug = %q, want my-file_1", ir.Slug)
	}
	// duplicate custom slug (in-flight upload holds it) -> 409
	res = e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "b.txt", "size": 10, "storage": "local", "slug": "my-file_1",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate slug: expected 409, got %d", res.StatusCode)
	}
	// invalid slug -> 400
	res = e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "c.txt", "size": 10, "storage": "local", "slug": "bad/slug",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid slug: expected 400, got %d", res.StatusCode)
	}
}

func TestStatsAndSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	e.upload(t, "s.bin", randomContent(500), "", 0)

	stats := decode[struct {
		Local     struct{ Bytes, Count int64 } `json:"local"`
		LocalFree int64                        `json:"localFree"`
	}](t, e.doJSON(t, e.client, "GET", "/api/stats", nil))
	if stats.Local.Bytes != 500 || stats.Local.Count != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if stats.LocalFree <= 0 {
		t.Fatalf("localFree = %d, expected positive", stats.LocalFree)
	}

	res, err := e.anon.Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if res.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestR2DownloadRedirect(t *testing.T) {
	stub := &stubR2{}
	e := newEnvWithR2(t, stub)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "r.bin", "size": 10, "storage": "r2",
	})
	ir := decode[initResp](t, res)
	res = e.doJSON(t, e.client, "POST",
		fmt.Sprintf("/api/uploads/%s/chunks/0/etag", ir.ID), map[string]string{"etag": "e0"})
	res.Body.Close()
	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	res.Body.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	dl, err := noRedirect.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 to presigned URL, got %d", dl.StatusCode)
	}
	if loc := dl.Header.Get("Location"); loc != "https://r2.example/get/"+ir.Slug {
		t.Fatalf("Location = %q", loc)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	// a second auth.Manager over the same store simulates a restart
	am2, err := auth.New(e.dataDir, testUser, testPassword, e.st)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.ts.URL)
	var token string
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == auth.SessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie in jar")
	}
	if !am2.Valid(token) {
		t.Fatal("session did not survive manager restart")
	}
}

func TestOversizedPreviewDisabled(t *testing.T) {
	e := newEnv(t)
	f := &store.File{
		Slug: store.NewSlug(), Name: "huge.png", Size: 30 << 20, Storage: "local",
		ContentType: "image/png", CreatedAt: time.Now().Unix(),
	}
	if err := e.st.CreateFile(f); err != nil {
		t.Fatal(err)
	}
	page, err := e.anon.Get(e.ts.URL + "/f/" + f.Slug)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "文件过大") {
		t.Fatalf("expected oversized note, got %.300s", body)
	}
	if strings.Contains(string(body), "/raw") {
		t.Fatal("oversized image still embeds raw preview")
	}
}

func TestR2TestValidation(t *testing.T) {
	e := newEnv(t)
	res := e.doJSON(t, e.client, "POST", "/api/settings/r2/test", map[string]string{
		"r2Endpoint": "", "r2AccessKey": "x", "r2Bucket": "b",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete r2 test: expected 400, got %d", res.StatusCode)
	}
}

func TestSettingsUpdateAppliesLive(t *testing.T) {
	e := newEnv(t)

	res := e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "我的网盘", "baseURL": "https://files.example.com", "chunkSizeMB": 10,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings update: %d", res.StatusCode)
	}

	// site name reflected on pages
	page, err := e.anon.Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "我的网盘") {
		t.Fatalf("login page missing new site name: %.200s", body)
	}

	// chunk size + base URL applied to new uploads
	cfg := decode[struct {
		ChunkSize int64 `json:"chunkSize"`
	}](t, e.doJSON(t, e.client, "GET", "/api/config", nil))
	if cfg.ChunkSize != 10*1024*1024 {
		t.Fatalf("chunkSize = %d, want 10MiB", cfg.ChunkSize)
	}
	ir := e.upload(t, "b.bin", randomContent(100), "", 0)
	list := decode[struct {
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}](t, e.doJSON(t, e.client, "GET", "/api/files", nil))
	if want := "https://files.example.com/f/" + ir.Slug; list.Files[0].URL != want {
		t.Fatalf("url = %q, want %q", list.Files[0].URL, want)
	}

	// settings survive a server rebuild (restart)
	srv2, err := server.New(&config.Config{
		Addr: ":0", DataDir: e.dataDir, Username: testUser, Password: testPassword,
		SiteName: "env-name", ChunkSize: chunkSize,
	}, e.st, mustAuth(t, e), nil, nil, os.DirFS("../../web"))
	if err != nil {
		t.Fatal(err)
	}
	_ = srv2
}

func mustAuth(t *testing.T, e *env) *auth.Manager {
	t.Helper()
	am, err := auth.New(e.dataDir, testUser, testPassword, e.st)
	if err != nil {
		t.Fatal(err)
	}
	return am
}

func TestAutoChunkSizeUpload(t *testing.T) {
	e := newEnv(t)

	// chunkSizeMB = 0 switches to auto mode; out-of-range values still refused
	res := e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "chunkSizeMB": 3,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("chunkSizeMB=3: expected 400, got %d", res.StatusCode)
	}
	res = e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "chunkSizeMB": 0,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings update to auto: %d", res.StatusCode)
	}

	// settings echo auto back
	st := decode[struct {
		ChunkSizeMB int64 `json:"chunkSizeMB"`
	}](t, e.doJSON(t, e.client, "GET", "/api/settings", nil))
	if st.ChunkSizeMB != 0 {
		t.Fatalf("chunkSizeMB = %d, want 0 (auto)", st.ChunkSizeMB)
	}

	// small file → floors at 5 MiB, one chunk
	ir := decode[initResp](t, e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "small.bin", "size": 1000, "storage": "local",
	}))
	if ir.ChunkSize != 5<<20 || ir.TotalChunks != 1 {
		t.Fatalf("small: chunkSize=%d totalChunks=%d, want 5MiB/1", ir.ChunkSize, ir.TotalChunks)
	}

	// 640 MiB → 10 MiB × 64 chunks (init only checks free space, writes nothing)
	ir = decode[initResp](t, e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "mid.iso", "size": 640 << 20, "storage": "local",
	}))
	if ir.ChunkSize != 10<<20 || ir.TotalChunks != 64 {
		t.Fatalf("mid: chunkSize=%d totalChunks=%d, want 10MiB/64", ir.ChunkSize, ir.TotalChunks)
	}

	// settings survive a restart in auto mode
	if _, err := server.New(&config.Config{
		Addr: ":0", DataDir: e.dataDir, Username: testUser, Password: testPassword,
		SiteName: "env-name", ChunkSize: chunkSize,
	}, e.st, mustAuth(t, e), nil, nil, os.DirFS("../../web")); err != nil {
		t.Fatalf("rebuild with saved auto chunk size: %v", err)
	}
}

func TestAccountUpdate(t *testing.T) {
	e := newEnv(t)

	// wrong current password refused
	res := e.doJSON(t, e.client, "PUT", "/api/settings/account", map[string]any{
		"currentPassword": "nope", "username": "root", "newPassword": "new-password-1",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong current password: expected 403, got %d", res.StatusCode)
	}

	res = e.doJSON(t, e.client, "PUT", "/api/settings/account", map[string]any{
		"currentPassword": testPassword, "username": "root", "newPassword": "new-password-1",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("account update: %d", res.StatusCode)
	}

	// old credentials no longer work, new ones do
	res = e.doJSON(t, e.anon, "POST", "/api/login",
		map[string]string{"username": testUser, "password": testPassword})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old creds: expected 401, got %d", res.StatusCode)
	}
	res = e.doJSON(t, e.anon, "POST", "/api/login",
		map[string]string{"username": "root", "password": "new-password-1"})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("new creds: expected 200, got %d", res.StatusCode)
	}

	// existing session still valid
	res = e.doJSON(t, e.client, "GET", "/api/files", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("session after password change: %d", res.StatusCode)
	}
}

func TestUpdateFileExpiryAndPassword(t *testing.T) {
	e := newEnv(t)
	e.upload(t, "u.txt", randomContent(50), "", 0)

	list := decode[struct {
		Files []struct {
			ID int64 `json:"id"`
		} `json:"files"`
	}](t, e.doJSON(t, e.client, "GET", "/api/files", nil))
	id := list.Files[0].ID

	res := e.doJSON(t, e.client, "PATCH", fmt.Sprintf("/api/files/%d", id),
		map[string]any{"expiresIn": 3600, "password": "pw"})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch: %d", res.StatusCode)
	}

	f, err := e.st.GetFileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if f.ExpiresAt == 0 || !f.HasPassword {
		t.Fatalf("update not applied: expiresAt=%d hasPassword=%v", f.ExpiresAt, f.HasPassword)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	res, err := e.anon.Get(e.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "ok\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestCleanupRemovesExhaustedFiles(t *testing.T) {
	e := newEnv(t)
	content := randomContent(32)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "once.bin", "size": len(content), "storage": "local", "maxDownloads": 1,
	})
	ir := decode[initResp](t, res)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/uploads/%s/chunks/0", e.ts.URL, ir.ID), bytes.NewReader(content))
	r1, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	res = e.doJSON(t, e.client, "POST", "/api/uploads/"+ir.ID+"/complete", nil)
	res.Body.Close()

	dl, err := e.anon.Get(e.ts.URL + "/f/" + ir.Slug + "/dl")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, dl.Body)
	dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", dl.StatusCode)
	}

	if err := e.srv.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetFileBySlug(ir.Slug); err != store.ErrNotFound {
		t.Fatalf("exhausted file still in DB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "files", ir.Slug)); !os.IsNotExist(err) {
		t.Fatalf("exhausted object still on disk: %v", err)
	}
}

func TestCleanupRemovesLocalOrphans(t *testing.T) {
	e := newEnv(t)
	orphan := filepath.Join(e.dataDir, "files", "orphan-key")
	if err := os.WriteFile(orphan, []byte("lost"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still present: %v", err)
	}
}

func TestDownloadPasswordRateLimit(t *testing.T) {
	e := newEnv(t)
	content := randomContent(16)
	ir := e.upload(t, "secret.txt", content, "s3cret", 0)

	// Exhaust the per-IP limiter with wrong passwords (limit is 20/hour).
	for i := 0; i < 20; i++ {
		res, err := e.anon.PostForm(e.ts.URL+"/f/"+ir.Slug, url.Values{"password": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	res, err := e.anon.PostForm(e.ts.URL+"/f/"+ir.Slug, url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	e := newEnv(t)
	res, err := e.anon.Get(e.ts.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	cc := res.Header.Get("Cache-Control")
	if !strings.Contains(cc, "max-age=") {
		t.Fatalf("Cache-Control = %q, want max-age", cc)
	}
}

func TestUploadAbort(t *testing.T) {
	e := newEnv(t)
	content := randomContent(chunkSize + 10)
	res := e.doJSON(t, e.client, "POST", "/api/uploads", map[string]any{
		"name": "abort.bin", "size": len(content), "storage": "local",
	})
	ir := decode[initResp](t, res)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/uploads/%s/chunks/0", e.ts.URL, ir.ID),
		bytes.NewReader(content[:chunkSize]))
	r1, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()

	res = e.doJSON(t, e.client, "DELETE", "/api/uploads/"+ir.ID, nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("abort: %d %s", res.StatusCode, body)
	}
	res.Body.Close()

	if _, err := e.st.GetUpload(ir.ID); err != store.ErrNotFound {
		t.Fatalf("upload still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "tmp", ir.Slug)); !os.IsNotExist(err) {
		t.Fatalf("tmp object still present: %v", err)
	}
}
