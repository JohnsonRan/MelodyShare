package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// resolveR2 validates the R2 form fields (trimming them), falls back to the
// stored secret when the submitted one is blank, and builds a client. It is
// shared by the save and test-connection handlers; the caller decides whether
// R2 is required or optional.
func (s *Server) resolveR2(endpoint, accessKey, secretKey, bucket string) (*config.R2, *storage.R2, error) {
	endpoint = strings.TrimSpace(endpoint)
	accessKey = strings.TrimSpace(accessKey)
	secretKey = strings.TrimSpace(secretKey)
	bucket = strings.TrimSpace(bucket)
	if endpoint == "" || accessKey == "" || bucket == "" {
		return nil, nil, errors.New("Endpoint、Access Key、Bucket 都需要填写")
	}
	if secretKey == "" {
		if cur := s.rt.R2Config(); cur != nil {
			secretKey = cur.SecretKey
		}
	}
	if secretKey == "" {
		return nil, nil, errors.New("缺少 R2 Secret Key")
	}
	cfg := &config.R2{Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey, Bucket: bucket}
	client, err := storage.NewR2(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Bucket)
	if err != nil {
		return nil, nil, fmt.Errorf("R2 配置无效：%w", err)
	}
	return cfg, client, nil
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
	if req.ChunkSizeMB != 0 && (req.ChunkSizeMB < minChunkMB || req.ChunkSizeMB > maxChunkMB) { // 0 = 自动
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
	if req.R2Endpoint != "" || req.R2AccessKey != "" || req.R2SecretKey != "" || req.R2Bucket != "" {
		cfg, client, err := s.resolveR2(req.R2Endpoint, req.R2AccessKey, req.R2SecretKey, req.R2Bucket)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		r2cfg, r2client = cfg, client
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
	jsonOK(w)
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
	_, client, err := s.resolveR2(req.R2Endpoint, req.R2AccessKey, req.R2SecretKey, req.R2Bucket)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := client.CheckBucket(ctx); err != nil {
		jsonError(w, http.StatusBadRequest, "连接失败："+err.Error())
		return
	}
	jsonOK(w)
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
	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hash != "" {
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
	jsonOK(w)
}
