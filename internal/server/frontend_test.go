package server_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readPage(t *testing.T, client *http.Client, baseURL, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%.200s", path, res.StatusCode, body)
	}
	return string(body)
}

func requireMarkup(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(body, value) {
			t.Errorf("page missing %q", value)
		}
	}
}

func TestSharedBrandAndThemeAssets(t *testing.T) {
	e := newEnv(t)
	pages := []struct {
		name   string
		client *http.Client
		path   string
	}{
		{"admin", e.client, "/"},
		{"login", e.anon, "/login"},
		{"paste", e.anon, "/p"},
	}
	for _, page := range pages {
		t.Run(page.name, func(t *testing.T) {
			body := readPage(t, page.client, e.ts.URL, page.path)
			requireMarkup(t, body,
				`/static/theme.js`,
				`/static/style.css`,
				`/static/logo.svg`,
				`class="brand-lockup"`,
				`data-theme-control`,
			)
		})
	}

	for _, path := range []string{"/static/theme.js", "/static/style.css", "/static/logo.svg"} {
		res, err := e.anon.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status=%d", path, res.StatusCode)
		}
	}
}

func TestEveryBrandedTemplateUsesSharedPartials(t *testing.T) {
	names := []string{"index.html", "login.html", "download.html", "preview.html", "message.html", "paste.html", "paste_view.html"}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		requireMarkup(t, source, `{{template "head-assets" .}}`, `{{template "brand-lockup" .}}`)
	}
}

func TestFocusedPublicPageContracts(t *testing.T) {
	e := newEnv(t)
	file := e.upload(t, "notes.txt", []byte("hello preview"), "", 0)
	protected := e.upload(t, "secret.txt", []byte("secret"), "hunter2", 0)
	paste := e.createPaste(t, "clipboard content", "2h")

	pasteReq, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/p/"+paste.Slug, nil)
	pasteReq.Header.Set("Accept", "text/html")
	pasteRes, err := e.anon.Do(pasteReq)
	if err != nil {
		t.Fatal(err)
	}
	pasteBodyBytes, _ := io.ReadAll(pasteRes.Body)
	pasteRes.Body.Close()

	pages := map[string]string{
		"login":        readPage(t, e.anon, e.ts.URL, "/login"),
		"paste-create": readPage(t, e.anon, e.ts.URL, "/p"),
		"paste-view":   string(pasteBodyBytes),
		"preview":      readPage(t, e.anon, e.ts.URL, "/f/"+file.Slug),
		"password":     readPage(t, e.anon, e.ts.URL, "/f/"+protected.Slug),
	}
	for name, body := range pages {
		t.Run(name, func(t *testing.T) {
			requireMarkup(t, body, `class="public-page`, `class="focus-card`, `/static/public.css`)
		})
	}
}

func TestPublicPagesDoNotLoadRemoteAssets(t *testing.T) {
	files := []string{"style.css", "public.css"}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join("..", "..", "web", "static", name))
		if err != nil {
			t.Fatal(err)
		}
		source := strings.ToLower(string(body))
		if strings.Contains(source, "@import url(") || strings.Contains(source, "fonts.googleapis.com") {
			t.Errorf("%s loads a remote font", name)
		}
	}
}

func TestAuthenticatedShellContract(t *testing.T) {
	e := newEnv(t)
	body := readPage(t, e.client, e.ts.URL, "/")
	requireMarkup(t, body,
		`class="app-shell"`,
		`class="app-sidebar"`,
		`id="filesView"`,
		`id="clipboardView"`,
		`id="settingsView"`,
		`data-view-target="files"`,
		`data-view-target="clipboard"`,
		`data-view-target="settings"`,
		`class="mobile-nav"`,
		`/static/app.css`,
	)
}

func TestAppSelectorsExistInIndexTemplate(t *testing.T) {
	indexBytes, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	scriptBytes, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	index := string(indexBytes)
	selector := regexp.MustCompile(`\$\('([A-Za-z][A-Za-z0-9_-]*)'\)`)
	seen := map[string]bool{}
	for _, match := range selector.FindAllStringSubmatch(string(scriptBytes), -1) {
		id := match[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("app.js expects missing index.html id %q", id)
		}
	}
}

func TestUploadSurfaceContract(t *testing.T) {
	e := newEnv(t)
	body := readPage(t, e.client, e.ts.URL, "/")
	requireMarkup(t, body,
		`id="dropzone"`,
		`id="fileInput"`,
		`id="advancedUploadOptions"`,
		`id="queue"`,
		`aria-live="polite"`,
		`id="expiry"`,
		`id="uploadPassword"`,
		`id="concurrency"`,
	)
	for _, text := range []string{"Cloudflare 100MB", "不受 Cloudflare", "重新选择同一文件即可断点续传"} {
		if strings.Contains(body, text) {
			t.Errorf("upload surface contains persistent technical copy %q", text)
		}
	}
}

func TestManagementListContainers(t *testing.T) {
	e := newEnv(t)
	body := readPage(t, e.client, e.ts.URL, "/")
	requireMarkup(t, body,
		`id="fileList"`,
		`class="record-list"`,
		`id="emptyHint"`,
		`id="stats"`,
		`id="pasteList"`,
		`id="pasteEmptyHint"`,
	)
	if strings.Contains(body, "<table") {
		t.Fatal("desktop-only table remains in responsive management views")
	}
}

func TestInPageSettingsContract(t *testing.T) {
	e := newEnv(t)
	body := readPage(t, e.client, e.ts.URL, "/")
	requireMarkup(t, body,
		`class="settings-layout"`,
		`class="settings-nav"`,
		`id="settings-site"`,
		`id="settings-upload"`,
		`id="settings-clipboard"`,
		`id="settings-r2"`,
		`id="settings-account"`,
		`id="settingsSave"`,
		`id="accountSave"`,
	)
	if strings.Contains(body, `id="settingsDialog"`) {
		t.Fatal("settings still use a modal dialog")
	}
}

func TestFrontendAccessibilityAndCopyContracts(t *testing.T) {
	e := newEnv(t)
	admin := readPage(t, e.client, e.ts.URL, "/")
	public := readPage(t, e.anon, e.ts.URL, "/login")
	requireMarkup(t, admin,
		`role="status"`,
		`aria-live="polite"`,
		`aria-label="选择或拖入文件"`,
		`aria-label="主导航"`,
	)
	requireMarkup(t, public, `role="alert"`, `autocomplete="username"`, `autocomplete="current-password"`)

	allTemplates, err := filepath.Glob(filepath.Join("..", "..", "web", "templates", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range allTemplates {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"Cloudflare 100MB", "不受 Cloudflare", "多线程上传", "分片上传"} {
			if strings.Contains(string(body), text) {
				t.Errorf("%s contains persistent technical copy %q", filepath.Base(path), text)
			}
		}
	}
}

func TestThemeHasLightDarkAndReducedMotionRules(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	requireMarkup(t, string(css),
		`:root[data-theme="dark"]`,
		`prefers-reduced-motion: reduce`,
		`--focus:`,
	)
}

func TestAppKeyboardInteractionContracts(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	requireMarkup(t, string(script),
		`event.key === 'Enter' || event.key === ' '`,
		`dropzone.contains(event.relatedTarget)`,
		`event.key !== 'Escape'`,
		`.record-menu[open]`,
	)
}

func TestPublicDatabaseErrorsAreSanitized(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		e := newEnv(t)
		if err := e.st.Close(); err != nil {
			t.Fatal(err)
		}
		res, err := e.anon.Get(e.ts.URL + "/f/missing")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", res.StatusCode, body)
		}
		if strings.Contains(strings.ToLower(string(body)), "database") || !strings.Contains(string(body), "请稍后重试") {
			t.Fatalf("unsafe public error: %s", body)
		}
	})

	t.Run("paste", func(t *testing.T) {
		e := newEnv(t)
		if err := e.st.Close(); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/p/miss1", nil)
		req.Header.Set("Accept", "text/html")
		res, err := e.anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", res.StatusCode, body)
		}
		if strings.Contains(strings.ToLower(string(body)), "database") || !strings.Contains(string(body), "请稍后重试") {
			t.Fatalf("unsafe public error: %s", body)
		}
	})
}
