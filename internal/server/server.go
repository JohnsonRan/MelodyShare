package server

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"melodyshare/internal/auth"
	"melodyshare/internal/config"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

type Server struct {
	cfg   *config.Config
	st    *store.Store
	auth  *auth.Manager
	local storage.Storage
	rt    *runtimeSettings
	tpl   *template.Template
	mux   *http.ServeMux

	pasteLimiter *rateLimiter
}

// New builds the server. r2Override, when non-nil, is used instead of
// constructing an R2 client from settings (tests inject a stub this way).
func New(cfg *config.Config, st *store.Store, am *auth.Manager, local, r2Override storage.Storage, webFS fs.FS) (*Server, error) {
	tpl, err := template.ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	rt, err := loadRuntimeSettings(cfg, st, r2Override)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, st: st, auth: am, local: local, rt: rt, tpl: tpl, mux: http.NewServeMux(),
		pasteLimiter: newRateLimiter(pasteRateLimit, time.Hour)}

	static, err := fs.Sub(webFS, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)

	s.mux.Handle("GET /api/config", s.requireAuth(s.handleConfig))
	s.mux.Handle("POST /api/uploads", s.requireAuth(s.handleUploadInit))
	s.mux.Handle("GET /api/uploads/{id}", s.requireAuth(s.handleUploadStatus))
	s.mux.Handle("PUT /api/uploads/{id}/chunks/{idx}", s.requireAuth(s.handleUploadChunk))
	s.mux.Handle("GET /api/uploads/{id}/chunks/{idx}/url", s.requireAuth(s.handleChunkURL))
	s.mux.Handle("POST /api/uploads/{id}/chunks/{idx}/etag", s.requireAuth(s.handleChunkETag))
	s.mux.Handle("POST /api/uploads/{id}/complete", s.requireAuth(s.handleUploadComplete))
	s.mux.Handle("DELETE /api/uploads/{id}", s.requireAuth(s.handleUploadAbort))
	s.mux.Handle("GET /api/files", s.requireAuth(s.handleFileList))
	s.mux.Handle("GET /api/stats", s.requireAuth(s.handleStats))
	s.mux.Handle("GET /api/settings", s.requireAuth(s.handleSettingsGet))
	s.mux.Handle("PUT /api/settings", s.requireAuth(s.handleSettingsUpdate))
	s.mux.Handle("PUT /api/settings/account", s.requireAuth(s.handleAccountUpdate))
	s.mux.Handle("POST /api/settings/r2/test", s.requireAuth(s.handleR2Test))
	s.mux.Handle("PATCH /api/files/{id}", s.requireAuth(s.handleFileUpdate))
	s.mux.Handle("DELETE /api/files/{id}", s.requireAuth(s.handleFileDelete))
	s.mux.Handle("GET /api/pastes", s.requireAuth(s.handlePasteList))
	s.mux.Handle("DELETE /api/pastes/{id}", s.requireAuth(s.handlePasteDelete))

	s.mux.HandleFunc("GET /p", s.handlePastePage)
	s.mux.HandleFunc("POST /p", s.handlePasteCreate)
	s.mux.HandleFunc("GET /p/{slug}", s.handlePasteGet)
	s.mux.HandleFunc("GET /p/{slug}/raw", s.handlePasteRaw)

	s.mux.HandleFunc("GET /f/{slug}", s.handleDownloadPage)
	s.mux.HandleFunc("POST /f/{slug}", s.handleDownloadPassword)
	s.mux.HandleFunc("GET /f/{slug}/dl", s.handleDownloadBytes)
	s.mux.HandleFunc("GET /f/{slug}/raw", s.handleRawBytes)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer") // keeps ?t= download tokens out of referrers
	h.Set("Content-Security-Policy", s.rt.CSP())
	s.mux.ServeHTTP(w, r)
}

func (s *Server) storageFor(name string) storage.Storage {
	switch name {
	case "local":
		return s.local
	case "r2":
		return s.rt.R2()
	}
	return nil
}

func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Authed(r) {
			jsonError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		next(w, r)
	})
}

// baseURL returns the prefix for share links, preferring the configured one.
func (s *Server) baseURL(r *http.Request) string {
	if base := s.rt.BaseURL(); base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) fileURL(r *http.Request, slug string) string {
	return s.baseURL(r) + "/f/" + slug
}

func (s *Server) pasteURL(r *http.Request, slug string) string {
	return s.baseURL(r) + "/p/" + slug
}

// --- pages ---

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.auth.Authed(r) {
		s.renderTemplate(w, http.StatusOK, "index.html", nil)
		return
	}
	// Anonymous visitors land on the public network clipboard instead of a
	// login wall — the toolbar keeps a login entry for the admin. With the
	// clipboard turned off there is nothing public to show, so fall back to
	// the login page.
	if s.rt.PasteEnabled() {
		s.renderPastePage(w, r, false)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.Authed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderTemplate(w, http.StatusOK, "login.html", nil)
}

// renderTemplate injects the site name alongside the page data.
func (s *Server) renderTemplate(w http.ResponseWriter, status int, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Site"] = s.rt.SiteName()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// --- helpers ---

func jsonWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonWrite(w, status, map[string]string{"error": msg})
}

// jsonOK writes the standard {"ok": true} success envelope.
func jsonOK(w http.ResponseWriter) {
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}

// pathInt64 parses an int64 {id} path segment, writing a 400 ("invalid
// <label>") on failure and reporting whether it succeeded.
func pathInt64(w http.ResponseWriter, r *http.Request, label string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid "+label)
		return 0, false
	}
	return v, true
}

// hashPassword bcrypt-hashes a plaintext password; an empty password yields an
// empty hash, meaning "no password".
func hashPassword(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(h), err
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
