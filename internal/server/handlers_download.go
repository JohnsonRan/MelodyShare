package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

const downloadTokenTTL = 6 * time.Hour

func (s *Server) lookupShared(w http.ResponseWriter, r *http.Request) *store.File {
	f, err := s.st.GetFileBySlug(r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.renderMessage(w, http.StatusNotFound, "文件不存在", "链接无效，或文件已被删除。")
		return nil
	}
	if err != nil {
		log.Printf("lookup shared %q: %v", r.PathValue("slug"), err)
		s.renderMessage(w, http.StatusInternalServerError, "暂时无法打开", "请稍后重试。")
		return nil
	}
	if f.ExpiresAt > 0 && time.Now().Unix() > f.ExpiresAt {
		s.renderMessage(w, http.StatusGone, "链接已过期", "该分享链接已过期，文件不再可用。")
		return nil
	}
	if f.MaxDownloads > 0 && f.Downloads >= f.MaxDownloads {
		s.renderMessage(w, http.StatusGone, "下载次数已用完", "该分享链接的下载次数已达上限。")
		return nil
	}
	return f
}

// requireShareAccess additionally enforces the share password (via ?t= token).
func (s *Server) requireShareAccess(w http.ResponseWriter, r *http.Request) *store.File {
	f := s.lookupShared(w, r)
	if f == nil {
		return nil
	}
	if f.HasPassword && !s.auth.CheckToken(f.Slug, r.URL.Query().Get("t")) {
		s.renderMessage(w, http.StatusForbidden, "需要密码", "请从分享页面输入密码后访问。")
		return nil
	}
	return f
}

// handleDownloadPage renders the share landing page: password form if needed,
// otherwise a preview with a download button.
func (s *Server) handleDownloadPage(w http.ResponseWriter, r *http.Request) {
	f := s.lookupShared(w, r)
	if f == nil {
		return
	}
	token := r.URL.Query().Get("t")
	if f.HasPassword && !s.auth.CheckToken(f.Slug, token) {
		s.renderTemplate(w, http.StatusOK, "download.html", map[string]any{
			"Name": f.Name, "Size": humanSize(f.Size), "Slug": f.Slug,
		})
		return
	}
	s.renderPreview(w, f, token)
}

// Inline previews that force a full transfer are capped so a huge file
// doesn't burn bandwidth just by being viewed. Video/audio are exempt: they
// stream on demand via Range (preload=metadata), so only watched parts
// transfer. Text previews fetch at most 64KB regardless of file size.
const (
	maxImagePreview = 20 << 20 // browsers download the whole image to decode
	maxPDFPreview   = 50 << 20 // built-in PDF viewers often fetch everything
)

// previewKind classifies what the preview page can render inline for a content
// type. oversized reports that an otherwise-previewable image/PDF exceeds the
// inline size cap and should be demoted to a plain download.
func previewKind(ct string, size int64) (kind string, oversized bool) {
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image", size > maxImagePreview
	case strings.HasPrefix(ct, "video/"):
		return "video", false
	case strings.HasPrefix(ct, "audio/"):
		return "audio", false
	case ct == "application/pdf":
		return "pdf", size > maxPDFPreview
	case strings.HasPrefix(ct, "text/"),
		ct == "application/json",
		ct == "application/xml",
		ct == "application/javascript":
		return "text", false
	}
	return "none", false
}

func (s *Server) renderPreview(w http.ResponseWriter, f *store.File, token string) {
	q := ""
	if token != "" {
		q = "?t=" + url.QueryEscape(token)
	}
	kind, oversized := previewKind(f.ContentType, f.Size)
	if oversized {
		kind = "none"
	}
	note := ""
	switch {
	case f.MaxDownloads > 0 && kind != "none":
		// inline previews would bypass the download counter
		kind = "none"
		note = "该文件设有下载次数限制，已禁用在线预览"
	case oversized:
		note = "文件过大，已禁用在线预览，请直接下载"
	}
	data := map[string]any{
		"Name":    f.Name,
		"Size":    humanSize(f.Size),
		"Created": time.Unix(f.CreatedAt, 0).Format("2006-01-02 15:04"),
		"Kind":    kind,
		"Note":    note,
		"DlURL":   "/f/" + f.Slug + "/dl" + q,
		"RawURL":  "/f/" + f.Slug + "/raw" + q,
	}
	if f.MaxDownloads > 0 {
		data["Remaining"] = f.MaxDownloads - f.Downloads
	}
	s.renderTemplate(w, http.StatusOK, "preview.html", data)
}

func (s *Server) handleDownloadPassword(w http.ResponseWriter, r *http.Request) {
	f := s.lookupShared(w, r)
	if f == nil {
		return
	}
	if !f.HasPassword {
		http.Redirect(w, r, "/f/"+f.Slug, http.StatusSeeOther)
		return
	}
	password := r.FormValue("password")
	if bcrypt.CompareHashAndPassword([]byte(f.PasswordHash), []byte(password)) != nil {
		time.Sleep(500 * time.Millisecond)
		s.renderTemplate(w, http.StatusUnauthorized, "download.html", map[string]any{
			"Name": f.Name, "Size": humanSize(f.Size), "Slug": f.Slug, "Error": "密码错误",
		})
		return
	}
	token := s.auth.MakeToken(f.Slug, downloadTokenTTL)
	http.Redirect(w, r, "/f/"+f.Slug+"?t="+url.QueryEscape(token), http.StatusSeeOther)
}

// handleDownloadBytes serves the file as an attachment and counts the download.
func (s *Server) handleDownloadBytes(w http.ResponseWriter, r *http.Request) {
	f := s.requireShareAccess(w, r)
	if f == nil {
		return
	}
	// Count once per transfer: initial requests only, not the follow-up
	// Range requests of a resumed/multi-threaded download.
	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" || strings.HasPrefix(rangeHdr, "bytes=0-") {
		if err := s.st.IncrementDownloads(f.ID); err != nil {
			log.Printf("count download %s: %v", f.Slug, err)
		}
	}
	s.serveFileBytes(w, r, f, false)
}

// handleRawBytes serves the file inline for the preview page; not counted.
func (s *Server) handleRawBytes(w http.ResponseWriter, r *http.Request) {
	f := s.requireShareAccess(w, r)
	if f == nil {
		return
	}
	if f.MaxDownloads > 0 {
		s.renderMessage(w, http.StatusForbidden, "预览不可用", "该文件设有下载次数限制，请直接下载。")
		return
	}
	s.serveFileBytes(w, r, f, true)
}

func (s *Server) serveFileBytes(w http.ResponseWriter, r *http.Request, f *store.File, inline bool) {
	disposition := contentDisposition(f.Name, inline)
	backend := s.storageFor(f.Storage)

	// Backends with presigned GETs (R2) serve the bytes themselves; the VPS
	// only pays for a redirect.
	if p, ok := backend.(storage.Presigner); ok {
		u, err := p.PresignGet(r.Context(), f.Slug, disposition, f.ContentType)
		if err == nil {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
		log.Printf("presign get %s: %v (falling back to proxy)", f.Slug, err)
	}

	rs, _, err := backend.Open(r.Context(), f.Slug)
	if err != nil {
		log.Printf("open %s (%s): %v", f.Slug, f.Storage, err)
		s.renderMessage(w, http.StatusInternalServerError, "服务器错误", "读取文件失败，请稍后重试。")
		return
	}
	defer rs.Close()
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Disposition", disposition)
	http.ServeContent(w, r, "", time.Unix(f.CreatedAt, 0), rs)
}

func (s *Server) renderMessage(w http.ResponseWriter, status int, title, detail string) {
	s.renderTemplate(w, status, "message.html", map[string]any{"Title": title, "Detail": detail})
}

// contentDisposition builds a disposition header with an ASCII fallback and an
// RFC 5987 encoded UTF-8 filename.
func contentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	fallback := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, fallback, url.PathEscape(name))
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
