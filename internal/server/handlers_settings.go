package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"melodyshare/internal/config"
	"melodyshare/internal/storage"
)

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	r2cfg := s.rt.R2Config()
	r2 := map[string]any{"endpoint": "", "accessKey": "", "bucket": "", "secretSet": false}
	if r2cfg != nil {
		r2 = map[string]any{
			"endpoint": r2cfg.Endpoint, "accessKey": r2cfg.AccessKey,
			"bucket": r2cfg.Bucket, "secretSet": true, // the secret itself is never echoed
		}
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"siteName":     s.rt.SiteName(),
		"baseURL":      s.rt.BaseURL(),
		"chunkSizeMB":  s.rt.ChunkSize() / (1024 * 1024),
		"pasteEnabled": s.rt.PasteEnabled(),
		"pasteMaxKB":   s.rt.PasteMaxBytes() / 1024,
		"r2":           r2,
		"username":     s.auth.Username(),
	})
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SiteName     string `json:"siteName"`
		BaseURL      string `json:"baseURL"`
		ChunkSizeMB  int64  `json:"chunkSizeMB"`
		PasteEnabled bool   `json:"pasteEnabled"` // full-replace endpoint: omitted = false (the settings dialog always sends it)
		PasteMaxKB   int64  `json:"pasteMaxKB"`   // 0 = default (1024)
		R2Endpoint   string `json:"r2Endpoint"`
		R2AccessKey  string `json:"r2AccessKey"`
		R2SecretKey  string `json:"r2SecretKey"` // empty = keep the stored one
		R2Bucket     string `json:"r2Bucket"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.SiteName = strings.TrimSpace(req.SiteName)
	req.BaseURL = strings.TrimSpace(strings.TrimRight(req.BaseURL, "/"))
	req.R2Endpoint = strings.TrimSpace(req.R2Endpoint)
	req.R2AccessKey = strings.TrimSpace(req.R2AccessKey)
	req.R2SecretKey = strings.TrimSpace(req.R2SecretKey)
	req.R2Bucket = strings.TrimSpace(req.R2Bucket)

	if req.SiteName == "" || len(req.SiteName) > 64 {
		jsonError(w, http.StatusBadRequest, "站点名称不能为空且不超过 64 字符")
		return
	}
	if req.BaseURL != "" && !strings.HasPrefix(req.BaseURL, "http://") && !strings.HasPrefix(req.BaseURL, "https://") {
		jsonError(w, http.StatusBadRequest, "Base URL 需以 http:// 或 https:// 开头")
		return
	}
	if req.ChunkSizeMB != 0 && (req.ChunkSizeMB < 5 || req.ChunkSizeMB > 95) { // 0 = 自动
		jsonError(w, http.StatusBadRequest, "分片大小需为自动或 5–95 MB 之间")
		return
	}
	if req.PasteMaxKB == 0 {
		req.PasteMaxKB = defaultPasteMaxKB
	}
	if req.PasteMaxKB < 1 || req.PasteMaxKB > maxPasteMaxKB {
		jsonError(w, http.StatusBadRequest, "剪切板单条上限需在 1–10240 KB 之间")
		return
	}

	// R2 is configured as a unit: all fields empty disables it.
	var r2cfg *config.R2
	var r2client storage.Storage
	anyR2 := req.R2Endpoint != "" || req.R2AccessKey != "" || req.R2SecretKey != "" || req.R2Bucket != ""
	if anyR2 {
		if req.R2Endpoint == "" || req.R2AccessKey == "" || req.R2Bucket == "" {
			jsonError(w, http.StatusBadRequest, "R2 配置不完整：Endpoint、Access Key、Bucket 都需要填写")
			return
		}
		secret := req.R2SecretKey
		if secret == "" {
			if cur := s.rt.R2Config(); cur != nil {
				secret = cur.SecretKey
			}
		}
		if secret == "" {
			jsonError(w, http.StatusBadRequest, "缺少 R2 Secret Key")
			return
		}
		r2cfg = &config.R2{
			Endpoint: req.R2Endpoint, AccessKey: req.R2AccessKey,
			SecretKey: secret, Bucket: req.R2Bucket,
		}
		client, err := storage.NewR2(r2cfg.Endpoint, r2cfg.AccessKey, r2cfg.SecretKey, r2cfg.Bucket)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "R2 配置无效："+err.Error())
			return
		}
		r2client = client
	}

	kv := map[string]string{
		"site_name":     req.SiteName,
		"base_url":      req.BaseURL,
		"chunk_size_mb": strconv.FormatInt(req.ChunkSizeMB, 10),
	}
	kv["paste_enabled"] = "1"
	if !req.PasteEnabled {
		kv["paste_enabled"] = "0"
	}
	kv["paste_max_kb"] = strconv.FormatInt(req.PasteMaxKB, 10)
	if r2cfg != nil {
		kv["r2_endpoint"] = r2cfg.Endpoint
		kv["r2_access_key"] = r2cfg.AccessKey
		kv["r2_secret_key"] = r2cfg.SecretKey
		kv["r2_bucket"] = r2cfg.Bucket
	} else {
		kv["r2_endpoint"], kv["r2_access_key"], kv["r2_secret_key"], kv["r2_bucket"] = "", "", "", ""
	}
	if err := s.st.SetSettings(kv); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.rt.Apply(req.SiteName, req.BaseURL, req.ChunkSizeMB*1024*1024, req.PasteEnabled, req.PasteMaxKB*1024, r2cfg, r2client)
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleR2Test verifies R2 credentials from the settings form by checking the
// bucket is reachable, without saving anything.
func (s *Server) handleR2Test(w http.ResponseWriter, r *http.Request) {
	var req struct {
		R2Endpoint  string `json:"r2Endpoint"`
		R2AccessKey string `json:"r2AccessKey"`
		R2SecretKey string `json:"r2SecretKey"` // empty = use the stored one
		R2Bucket    string `json:"r2Bucket"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.R2Endpoint = strings.TrimSpace(req.R2Endpoint)
	req.R2AccessKey = strings.TrimSpace(req.R2AccessKey)
	req.R2SecretKey = strings.TrimSpace(req.R2SecretKey)
	req.R2Bucket = strings.TrimSpace(req.R2Bucket)
	if req.R2Endpoint == "" || req.R2AccessKey == "" || req.R2Bucket == "" {
		jsonError(w, http.StatusBadRequest, "Endpoint、Access Key、Bucket 都需要填写")
		return
	}
	if req.R2SecretKey == "" {
		if cur := s.rt.R2Config(); cur != nil {
			req.R2SecretKey = cur.SecretKey
		}
	}
	if req.R2SecretKey == "" {
		jsonError(w, http.StatusBadRequest, "缺少 Secret Key")
		return
	}
	client, err := storage.NewR2(req.R2Endpoint, req.R2AccessKey, req.R2SecretKey, req.R2Bucket)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "配置无效："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := client.CheckBucket(ctx); err != nil {
		jsonError(w, http.StatusBadRequest, "连接失败："+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		Username        string `json:"username"`
		NewPassword     string `json:"newPassword"` // empty = unchanged
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.auth.CheckPassword(req.CurrentPassword) {
		jsonError(w, http.StatusForbidden, "当前密码不正确")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Username) > 64 {
		jsonError(w, http.StatusBadRequest, "用户名不能为空且不超过 64 字符")
		return
	}
	if req.NewPassword != "" && len(req.NewPassword) < 8 {
		jsonError(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}

	kv := map[string]string{"username": req.Username}
	hash := ""
	if req.NewPassword != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		hash = string(h)
		kv["password_hash"] = hash
	}
	if err := s.st.SetSettings(kv); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hash == "" {
		// keep the current hash, only the username changes
		s.auth.UpdateUsername(req.Username)
	} else {
		s.auth.UpdateCredentials(req.Username, hash)
	}
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}
