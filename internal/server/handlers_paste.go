package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"melodyshare/internal/store"
)

const (
	defaultPasteTTL = 2 * time.Hour
	minPasteTTL     = time.Minute
	maxPasteTTL     = 24 * time.Hour
	pasteRateLimit  = 30 // anonymous creations per IP per hour
	pasteSlugTries  = 5
)

// handlePasteCreate accepts the web form (content/ttl fields) or any raw
// request body (curl); ttl may also arrive as a ?ttl= query parameter.
func (s *Server) handlePasteCreate(w http.ResponseWriter, r *http.Request) {
	if !s.rt.PasteEnabled() {
		jsonError(w, http.StatusNotFound, "剪切板功能未开启")
		return
	}
	if !s.pasteLimiter.allow(s.clientIP(r)) {
		jsonError(w, http.StatusTooManyRequests, "创建太频繁，请稍后再试")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.rt.PasteMaxBytes())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, http.StatusRequestEntityTooLarge, "内容超过大小上限")
			return
		}
		jsonError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}

	content := string(body)
	ttlStr := r.URL.Query().Get("ttl")
	// curl sends application/x-www-form-urlencoded by default even for raw
	// bodies (-d / --data-binary), so only treat the body as a form when a
	// content= field is actually present; otherwise the body is the paste.
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
		if vals, err := url.ParseQuery(content); err == nil && vals.Get("content") != "" {
			content = vals.Get("content")
			if v := vals.Get("ttl"); v != "" {
				ttlStr = v
			}
		}
	}

	if strings.TrimSpace(content) == "" {
		jsonError(w, http.StatusBadRequest, "内容不能为空")
		return
	}
	if !utf8.ValidString(content) {
		jsonError(w, http.StatusBadRequest, "内容必须是 UTF-8 文本；二进制文件请使用文件分享")
		return
	}

	ttl := defaultPasteTTL
	if ttlStr != "" {
		d, err := time.ParseDuration(ttlStr)
		if err != nil || d <= 0 {
			jsonError(w, http.StatusBadRequest, "ttl 无效：请使用 30m、2h、24h 这样的格式")
			return
		}
		ttl = min(max(d, minPasteTTL), maxPasteTTL)
	}

	now := time.Now().Unix()
	p := &store.Paste{Content: content, CreatedAt: now, ExpiresAt: now + int64(ttl/time.Second)}
	for i := 0; ; i++ {
		p.Slug = store.NewPasteSlug()
		err = s.st.CreatePaste(p)
		if err == nil {
			break
		}
		if !store.IsUniqueViolation(err) || i == pasteSlugTries-1 {
			slog.Error("create paste", "err", err)
			jsonError(w, http.StatusInternalServerError, "创建失败，请稍后再试")
			return
		}
	}

	link := s.pasteURL(r, p.Slug)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		jsonWrite(w, http.StatusOK, map[string]any{"url": link, "slug": p.Slug, "expiresAt": p.ExpiresAt})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, link)
}

// wantsHTML reports whether the client is a browser; curl and friends do not
// put text/html in Accept.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// pasteError writes a styled message page for browsers and a plain-text line
// for CLI clients.
func (s *Server) pasteError(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	if wantsHTML(r) {
		s.renderMessage(w, status, title, detail)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, title+"："+detail)
}

// lookupPaste loads a live paste; on any miss it writes the response itself
// and returns nil.
func (s *Server) lookupPaste(w http.ResponseWriter, r *http.Request) *store.Paste {
	if !s.rt.PasteEnabled() {
		s.pasteError(w, r, http.StatusNotFound, "剪切板功能未开启", "管理员已关闭此功能。")
		return nil
	}
	p, err := s.st.GetPasteBySlug(r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.pasteError(w, r, http.StatusNotFound, "内容不存在", "链接无效，或内容已被删除。")
		return nil
	}
	if err != nil {
		slog.Error("lookup paste", "slug", r.PathValue("slug"), "err", err)
		s.pasteError(w, r, http.StatusInternalServerError, "暂时无法打开", "请稍后重试。")
		return nil
	}
	if time.Now().Unix() > p.ExpiresAt {
		s.pasteError(w, r, http.StatusGone, "内容已过期", "该剪切板内容已过期，不再可用。")
		return nil
	}
	return p
}

func (s *Server) handlePasteGet(w http.ResponseWriter, r *http.Request) {
	p := s.lookupPaste(w, r)
	if p == nil {
		return
	}
	if !wantsHTML(r) {
		s.servePasteText(w, p)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, http.StatusOK, "paste_view.html", map[string]any{
		"Content": p.Content,
		"RawURL":  "/p/" + p.Slug + "/raw",
		"Expires": time.Unix(p.ExpiresAt, 0).Format("2006-01-02 15:04"),
	})
}

func (s *Server) handlePasteRaw(w http.ResponseWriter, r *http.Request) {
	p := s.lookupPaste(w, r)
	if p == nil {
		return
	}
	s.servePasteText(w, p)
}

// servePasteText writes the paste verbatim. Explicit text/plain plus the
// global nosniff header keeps pasted HTML from ever rendering.
func (s *Server) servePasteText(w http.ResponseWriter, p *store.Paste) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, p.Content)
}

func (s *Server) handlePastePage(w http.ResponseWriter, r *http.Request) {
	if !s.rt.PasteEnabled() {
		s.pasteError(w, r, http.StatusNotFound, "剪切板功能未开启", "管理员已关闭此功能。")
		return
	}
	s.renderPastePage(w, r, s.auth.Authed(r))
}

// renderPastePage renders the clipboard-creation page. authed toggles the
// toolbar between a login entry (public visitors) and a link back to the
// admin dashboard.
func (s *Server) renderPastePage(w http.ResponseWriter, r *http.Request, authed bool) {
	s.renderTemplate(w, http.StatusOK, "paste.html", map[string]any{
		"MaxSize":  humanSize(s.rt.PasteMaxBytes()),
		"CurlBase": s.baseURL(r) + "/p",
		"Authed":   authed,
	})
}

// --- admin (login required) ---

func (s *Server) handlePasteList(w http.ResponseWriter, r *http.Request) {
	pastes, err := s.st.ListPastes()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		ID        int64  `json:"id"`
		Slug      string `json:"slug"`
		Preview   string `json:"preview"`
		CreatedAt int64  `json:"createdAt"`
		ExpiresAt int64  `json:"expiresAt"`
		URL       string `json:"url"`
	}
	items := make([]item, len(pastes))
	for i, p := range pastes {
		items[i] = item{
			ID: p.ID, Slug: p.Slug, Preview: pastePreview(p.Content),
			CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, URL: s.pasteURL(r, p.Slug),
		}
	}
	jsonWrite(w, http.StatusOK, map[string]any{"pastes": items})
}

// pastePreview returns the first 80 runes for the admin list.
func pastePreview(content string) string {
	runes := []rune(content)
	if len(runes) <= 80 {
		return content
	}
	return string(runes[:80]) + "…"
}

func (s *Server) handlePasteDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "paste id")
	if !ok {
		return
	}
	if _, err := s.st.GetPasteByID(id); errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "paste not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.DeletePaste(id); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w)
}
