package server

import (
	"fmt"
	"net/http"
	"strconv"

	"melodyshare/internal/auth"
)

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	token, retryAfter := s.auth.Login(req.Username, req.Password)
	if retryAfter > 0 {
		secs := int(retryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("尝试次数过多，请 %d 秒后重试", secs))
		return
	}
	if token == "" {
		jsonError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil,
	})
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		s.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{
		"chunkSize": s.rt.ChunkSize(),
		"r2Enabled": s.rt.R2() != nil,
	})
}
