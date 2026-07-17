package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"melodyshare/internal/config"
	"melodyshare/internal/server"
	"melodyshare/internal/store"
)

type pasteResp struct {
	URL       string `json:"url"`
	Slug      string `json:"slug"`
	ExpiresAt int64  `json:"expiresAt"`
}

// createPaste posts raw text with a JSON accept header and fails the test on
// any non-200.
func (e *env) createPaste(t *testing.T, content, ttl string) pasteResp {
	t.Helper()
	path := "/p"
	if ttl != "" {
		path += "?ttl=" + url.QueryEscape(ttl)
	}
	req, err := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	res, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("create paste: %d %s", res.StatusCode, body)
	}
	return decode[pasteResp](t, res)
}

// postPaste is the raw variant used by validation tests: returns the response
// without asserting the status.
func (e *env) postPaste(t *testing.T, path, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", e.ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantExpiry(t *testing.T, got, wantIn int64) {
	t.Helper()
	want := time.Now().Unix() + wantIn
	if got < want-300 || got > want+300 {
		t.Fatalf("expiresAt = %d, want ≈ now+%ds", got, wantIn)
	}
}

func TestPasteCreatePlainTextResponse(t *testing.T) {
	e := newEnv(t)
	res := e.postPaste(t, "/p", "text/plain", strings.NewReader("hello"))
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	line := strings.TrimSpace(string(body))
	i := strings.LastIndex(line, "/p/")
	if i < 0 {
		t.Fatalf("response is not a paste URL: %q", line)
	}
	slug := line[i+len("/p/"):]
	if len(slug) != 5 {
		t.Fatalf("slug %q length = %d, want 5", slug, len(slug))
	}
	p, err := e.st.GetPasteBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "hello" {
		t.Fatalf("stored content = %q", p.Content)
	}
	wantExpiry(t, p.ExpiresAt, 7200) // default 2h
}

func TestPasteCreateJSONAndTTL(t *testing.T) {
	e := newEnv(t)
	pr := e.createPaste(t, "json body", "24h")
	if pr.Slug == "" || !strings.Contains(pr.URL, "/p/"+pr.Slug) {
		t.Fatalf("bad response: %+v", pr)
	}
	wantExpiry(t, pr.ExpiresAt, 24*3600)

	// clamp up and down
	wantExpiry(t, e.createPaste(t, "x", "48h").ExpiresAt, 24*3600)
	wantExpiry(t, e.createPaste(t, "x", "5s").ExpiresAt, 60)
	wantExpiry(t, e.createPaste(t, "x", "30m").ExpiresAt, 1800)
}

func TestPasteCreateForm(t *testing.T) {
	e := newEnv(t)
	res, err := e.anon.PostForm(e.ts.URL+"/p", url.Values{"content": {"表单内容"}, "ttl": {"30m"}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("form create: %d %s", res.StatusCode, body)
	}
	body, _ := io.ReadAll(res.Body)
	line := strings.TrimSpace(string(body))
	slug := line[strings.LastIndex(line, "/p/")+len("/p/"):]
	p, err := e.st.GetPasteBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "表单内容" {
		t.Fatalf("stored content = %q", p.Content)
	}
	wantExpiry(t, p.ExpiresAt, 1800)
}

func TestPasteCreateCurlDefaultContentType(t *testing.T) {
	// curl --data-binary 不设 -H 时发送 application/x-www-form-urlencoded,
	// 但 body 是原始文本——没有 content= 字段时整个 body 就是内容
	e := newEnv(t)
	raw := "plain body, not a form: a=b&c=d"
	res := e.postPaste(t, "/p", "application/x-www-form-urlencoded", strings.NewReader(raw))
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	line := strings.TrimSpace(string(body))
	slug := line[strings.LastIndex(line, "/p/")+len("/p/"):]
	p, err := e.st.GetPasteBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != raw {
		t.Fatalf("stored content = %q, want raw body", p.Content)
	}
}

func TestPasteCreateValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		path string
		ct   string
		body []byte
		want int
	}{
		{"empty", "/p", "text/plain", nil, http.StatusBadRequest},
		{"whitespace only", "/p", "text/plain", []byte(" \n\t"), http.StatusBadRequest},
		{"invalid utf8", "/p", "application/octet-stream", []byte{0xff, 0xfe, 0xfd}, http.StatusBadRequest},
		{"bad ttl", "/p?ttl=banana", "text/plain", []byte("x"), http.StatusBadRequest},
		{"negative ttl", "/p?ttl=-5m", "text/plain", []byte("x"), http.StatusBadRequest},
	}
	for _, c := range cases {
		res := e.postPaste(t, c.path, c.ct, bytes.NewReader(c.body))
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Fatalf("%s: status = %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
}

func TestPasteSizeLimit(t *testing.T) {
	e := newEnv(t)
	// 默认上限 1 MiB:恰好 1 MiB 可以,多 1 字节 413
	ok := e.postPaste(t, "/p", "text/plain", strings.NewReader(strings.Repeat("a", 1<<20)))
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("exactly 1MiB: status = %d", ok.StatusCode)
	}
	over := e.postPaste(t, "/p", "text/plain", strings.NewReader(strings.Repeat("a", 1<<20+1)))
	over.Body.Close()
	if over.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("1MiB+1: status = %d, want 413", over.StatusCode)
	}
}

func TestPasteRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		res := e.postPaste(t, "/p", "text/plain", strings.NewReader("x"))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, res.StatusCode)
		}
	}
	res := e.postPaste(t, "/p", "text/plain", strings.NewReader("x"))
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("31st request: status = %d, want 429", res.StatusCode)
	}
}

func TestPasteRawAndNegotiation(t *testing.T) {
	e := newEnv(t)
	pr := e.createPaste(t, "negotiate me", "")

	// 无 text/html Accept(curl 场景)→ 原文
	res, err := e.anon.Get(e.ts.URL + "/p/" + pr.Slug)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if string(body) != "negotiate me" {
		t.Fatalf("body = %q", body)
	}

	// 浏览器 Accept → 查看页,含内容与 raw 链接
	req, _ := http.NewRequest("GET", e.ts.URL+"/p/"+pr.Slug, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	res2, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if ct := res2.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("page Content-Type = %q", ct)
	}
	if !strings.Contains(string(page), "negotiate me") ||
		!strings.Contains(string(page), "/p/"+pr.Slug+"/raw") {
		t.Fatalf("page missing content or raw link: %.300s", page)
	}

	// /raw 即使带 html Accept 也始终原文
	req2, _ := http.NewRequest("GET", e.ts.URL+"/p/"+pr.Slug+"/raw", nil)
	req2.Header.Set("Accept", "text/html")
	res3, err := e.anon.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res3.Body)
	res3.Body.Close()
	if !strings.HasPrefix(res3.Header.Get("Content-Type"), "text/plain") || string(raw) != "negotiate me" {
		t.Fatalf("raw: ct=%q body=%q", res3.Header.Get("Content-Type"), raw)
	}
}

func TestPasteViewEscapesHTML(t *testing.T) {
	e := newEnv(t)
	pr := e.createPaste(t, "<script>alert(1)</script>", "")
	req, _ := http.NewRequest("GET", e.ts.URL+"/p/"+pr.Slug, nil)
	req.Header.Set("Accept", "text/html")
	res, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(page), "<script>alert") {
		t.Fatal("paste content not escaped in view page")
	}
	if !strings.Contains(string(page), "&lt;script&gt;") {
		t.Fatalf("escaped content missing: %.300s", page)
	}
}

func TestPasteNotFoundAndExpired(t *testing.T) {
	e := newEnv(t)

	res, err := e.anon.Get(e.ts.URL + "/p/zzzzz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing paste: %d, want 404", res.StatusCode)
	}

	now := time.Now().Unix()
	if err := e.st.CreatePaste(&store.Paste{Slug: "dead2", Content: "old", CreatedAt: now - 7200, ExpiresAt: now - 10}); err != nil {
		t.Fatal(err)
	}
	res2, err := e.anon.Get(e.ts.URL + "/p/dead2")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusGone {
		t.Fatalf("expired paste: %d, want 410", res2.StatusCode)
	}

	// 浏览器 Accept 时错误是 HTML 页
	req, _ := http.NewRequest("GET", e.ts.URL+"/p/dead2", nil)
	req.Header.Set("Accept", "text/html")
	res3, err := e.anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res3.Body)
	res3.Body.Close()
	if res3.StatusCode != http.StatusGone || !strings.Contains(res3.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("expired html: status=%d ct=%q %.200s", res3.StatusCode, res3.Header.Get("Content-Type"), page)
	}
}

func TestPastePageServed(t *testing.T) {
	e := newEnv(t)
	res, err := e.anon.Get(e.ts.URL + "/p")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /p: status=%d ct=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	for _, want := range []string{`id="pasteForm"`, `class="cli-help"`, "/static/paste.js"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("paste page missing %q: %.300s", want, body)
		}
	}
	if strings.Contains(string(body), "命令行用法：") {
		t.Fatal("command-line tutorial is still permanently visible")
	}
}

func TestCleanupRemovesExpiredPastes(t *testing.T) {
	e := newEnv(t)
	now := time.Now().Unix()
	live := &store.Paste{Slug: "keep1", Content: "x", CreatedAt: now, ExpiresAt: now + 3600}
	dead := &store.Paste{Slug: "gone1", Content: "x", CreatedAt: now - 7200, ExpiresAt: now - 10}
	for _, p := range []*store.Paste{live, dead} {
		if err := e.st.CreatePaste(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.srv.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetPasteBySlug("keep1"); err != nil {
		t.Fatalf("live paste removed by cleanup: %v", err)
	}
	if _, err := e.st.GetPasteBySlug("gone1"); err == nil {
		t.Fatal("expired paste survived cleanup")
	}
}

func TestPasteSettingsLimitAndToggle(t *testing.T) {
	e := newEnv(t)

	// 默认值回显
	st := decode[struct {
		PasteEnabled bool  `json:"pasteEnabled"`
		PasteMaxKB   int64 `json:"pasteMaxKB"`
	}](t, e.doJSON(t, e.client, "GET", "/api/settings", nil))
	if !st.PasteEnabled || st.PasteMaxKB != 1024 {
		t.Fatalf("defaults: %+v", st)
	}

	// 上限改成 2 KB:3 KB 被拒,1 KB 通过
	res := e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "pasteEnabled": true, "pasteMaxKB": 2,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings update: %d", res.StatusCode)
	}
	big := e.postPaste(t, "/p", "text/plain", strings.NewReader(strings.Repeat("a", 3*1024)))
	big.Body.Close()
	if big.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("3KB paste after 2KB limit: %d, want 413", big.StatusCode)
	}
	pr := e.createPaste(t, strings.Repeat("a", 1024), "")

	// 超范围报 400
	res = e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "pasteEnabled": true, "pasteMaxKB": 99999,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("pasteMaxKB=99999: %d, want 400", res.StatusCode)
	}

	// 关闭功能:/p 全部 404,包括已存在的内容
	res = e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "pasteEnabled": false, "pasteMaxKB": 2,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable: %d", res.StatusCode)
	}
	for _, path := range []string{"/p", "/p/" + pr.Slug, "/p/" + pr.Slug + "/raw"} {
		r2, err := e.anon.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s while disabled: %d, want 404", path, r2.StatusCode)
		}
	}
	post := e.postPaste(t, "/p", "text/plain", strings.NewReader("x"))
	post.Body.Close()
	if post.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /p while disabled: %d, want 404", post.StatusCode)
	}

	// rebuild 模拟重启:同一数据库上重建 Server
	rebuild := func() *httptest.Server {
		srv, err := server.New(&config.Config{
			Addr: ":0", DataDir: e.dataDir, Username: testUser, Password: testPassword,
			SiteName: "env-name", ChunkSize: chunkSize,
		}, e.st, mustAuth(t, e), nil, nil, os.DirFS("../../web"))
		if err != nil {
			t.Fatalf("rebuild with saved paste settings: %v", err)
		}
		ts := httptest.NewServer(srv)
		t.Cleanup(ts.Close)
		return ts
	}

	// 关闭状态在重启后仍生效
	ts2 := rebuild()
	r3, err := e.anon.Get(ts2.URL + "/p")
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != http.StatusNotFound {
		t.Fatalf("rebuilt server GET /p while disabled: %d, want 404", r3.StatusCode)
	}

	// 重新开启:原服务器立即恢复
	res = e.doJSON(t, e.client, "PUT", "/api/settings", map[string]any{
		"siteName": "s", "pasteEnabled": true, "pasteMaxKB": 2,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-enable: %d", res.StatusCode)
	}
	for _, path := range []string{"/p", "/p/" + pr.Slug} {
		r4, err := e.anon.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r4.Body.Close()
		if r4.StatusCode != http.StatusOK {
			t.Fatalf("GET %s after re-enable: %d, want 200", path, r4.StatusCode)
		}
	}

	// 开启状态与 2 KB 上限也随重启恢复
	ts3 := rebuild()
	big2, err := e.anon.Post(ts3.URL+"/p", "text/plain", strings.NewReader(strings.Repeat("a", 3*1024)))
	if err != nil {
		t.Fatal(err)
	}
	big2.Body.Close()
	if big2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("rebuilt server 3KB paste: %d, want 413", big2.StatusCode)
	}
	ok2, err := e.anon.Post(ts3.URL+"/p", "text/plain", strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}
	ok2.Body.Close()
	if ok2.StatusCode != http.StatusOK {
		t.Fatalf("rebuilt server small paste: %d, want 200", ok2.StatusCode)
	}
}

func TestPasteAdminListDelete(t *testing.T) {
	e := newEnv(t)

	// 未登录一律 401
	res := e.doJSON(t, e.anon, "GET", "/api/pastes", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon list: %d, want 401", res.StatusCode)
	}
	res = e.doJSON(t, e.anon, "DELETE", "/api/pastes/1", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon delete: %d, want 401", res.StatusCode)
	}

	first := e.createPaste(t, "第一条", "")
	long := strings.Repeat("很", 100)
	second := e.createPaste(t, long, "")

	type pasteItem struct {
		ID        int64  `json:"id"`
		Slug      string `json:"slug"`
		Preview   string `json:"preview"`
		CreatedAt int64  `json:"createdAt"`
		ExpiresAt int64  `json:"expiresAt"`
		URL       string `json:"url"`
	}
	list := decode[struct {
		Pastes []pasteItem `json:"pastes"`
	}](t, e.doJSON(t, e.client, "GET", "/api/pastes", nil))
	if len(list.Pastes) != 2 {
		t.Fatalf("list length = %d, want 2", len(list.Pastes))
	}
	for _, p := range list.Pastes {
		if p.URL == "" || p.ExpiresAt == 0 {
			t.Fatalf("bad item: %+v", p)
		}
		if p.Slug == second.Slug {
			if got := len([]rune(p.Preview)); got != 81 { // 80 + "…"
				t.Fatalf("preview rune length = %d, want 81", got)
			}
			if !strings.HasSuffix(p.Preview, "…") {
				t.Fatalf("preview not truncated: %q", p.Preview)
			}
		}
		if p.Slug == first.Slug && p.Preview != "第一条" {
			t.Fatalf("short preview = %q", p.Preview)
		}
	}

	// 删除后 /p/{slug} 立即 404
	var delID int64
	for _, p := range list.Pastes {
		if p.Slug == first.Slug {
			delID = p.ID
		}
	}
	res = e.doJSON(t, e.client, "DELETE", fmt.Sprintf("/api/pastes/%d", delID), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	gone, err := e.anon.Get(e.ts.URL + "/p/" + first.Slug)
	if err != nil {
		t.Fatal(err)
	}
	gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %d, want 404", gone.StatusCode)
	}

	// 重复删除 404
	res = e.doJSON(t, e.client, "DELETE", fmt.Sprintf("/api/pastes/%d", delID), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("double delete: %d, want 404", res.StatusCode)
	}
}
