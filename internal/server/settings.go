package server

import (
	"fmt"
	"strconv"
	"sync"

	"melodyshare/internal/config"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

const (
	defaultPasteMaxKB = 1024
	maxPasteMaxKB     = 10240
)

// runtimeSettings holds configuration that can be changed from the settings
// page without a restart. Values saved in the DB override env defaults.
type runtimeSettings struct {
	mu        sync.RWMutex
	siteName  string
	baseURL   string
	chunkSize int64
	r2cfg     *config.R2
	r2        storage.Storage
	csp       string

	pasteEnabled  bool
	pasteMaxBytes int64
}

// loadRuntimeSettings merges DB-saved settings over the env config and builds
// the R2 client when configured. r2Override (used by tests) wins over both.
func loadRuntimeSettings(cfg *config.Config, st *store.Store, r2Override storage.Storage) (*runtimeSettings, error) {
	db, err := st.AllSettings()
	if err != nil {
		return nil, err
	}
	rt := &runtimeSettings{
		siteName:  pick(db["site_name"], cfg.SiteName),
		baseURL:   pick(db["base_url"], cfg.BaseURL),
		chunkSize: cfg.ChunkSize,
	}
	rt.pasteEnabled = db["paste_enabled"] != "0" // absent = enabled
	rt.pasteMaxBytes = defaultPasteMaxKB * 1024
	if v := db["paste_max_kb"]; v != "" {
		kb, err := strconv.Atoi(v)
		if err != nil || kb < 1 || kb > maxPasteMaxKB {
			return nil, fmt.Errorf("saved paste_max_kb invalid: %q", v)
		}
		rt.pasteMaxBytes = int64(kb) * 1024
	}
	if v := db["chunk_size_mb"]; v != "" {
		mb, err := strconv.Atoi(v)
		if err != nil || (mb != 0 && (mb < 5 || mb > 95)) { // 0 = auto
			return nil, fmt.Errorf("saved chunk_size_mb invalid: %q", v)
		}
		rt.chunkSize = int64(mb) * 1024 * 1024
	}

	// The R2 quad is treated as a unit: DB config (if present) replaces the
	// env config entirely.
	r2cfg := cfg.R2
	if db["r2_endpoint"] != "" {
		r2cfg = &config.R2{
			Endpoint:  db["r2_endpoint"],
			AccessKey: db["r2_access_key"],
			SecretKey: db["r2_secret_key"],
			Bucket:    db["r2_bucket"],
		}
		if r2cfg.AccessKey == "" || r2cfg.SecretKey == "" || r2cfg.Bucket == "" {
			return nil, fmt.Errorf("saved R2 settings are incomplete")
		}
	}

	switch {
	case r2Override != nil:
		rt.r2 = r2Override
		rt.r2cfg = r2cfg
	case r2cfg != nil:
		client, err := storage.NewR2(r2cfg.Endpoint, r2cfg.AccessKey, r2cfg.SecretKey, r2cfg.Bucket)
		if err != nil {
			return nil, fmt.Errorf("R2 client: %w", err)
		}
		rt.r2 = client
		rt.r2cfg = r2cfg
	}
	rt.csp = buildCSP(rt.r2cfg)
	return rt, nil
}

func pick(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func (rt *runtimeSettings) SiteName() string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.siteName
}

func (rt *runtimeSettings) BaseURL() string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.baseURL
}

func (rt *runtimeSettings) ChunkSize() int64 {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.chunkSize
}

func (rt *runtimeSettings) R2() storage.Storage {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.r2
}

func (rt *runtimeSettings) R2Config() *config.R2 {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.r2cfg
}

func (rt *runtimeSettings) CSP() string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.csp
}

func (rt *runtimeSettings) PasteEnabled() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.pasteEnabled
}

func (rt *runtimeSettings) PasteMaxBytes() int64 {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.pasteMaxBytes
}

// Apply swaps the runtime values in one shot (after they were persisted).
func (rt *runtimeSettings) Apply(siteName, baseURL string, chunkSize int64, pasteEnabled bool, pasteMaxBytes int64, r2cfg *config.R2, r2 storage.Storage) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.siteName = siteName
	rt.baseURL = baseURL
	rt.chunkSize = chunkSize
	rt.pasteEnabled = pasteEnabled
	rt.pasteMaxBytes = pasteMaxBytes
	rt.r2cfg = r2cfg
	rt.r2 = r2
	rt.csp = buildCSP(r2cfg)
}

// buildCSP allows only same-origin resources, plus the R2 endpoint when
// configured: XHR for direct uploads, and img/media/frame because preview
// resources redirect to presigned R2 URLs.
func buildCSP(r2 *config.R2) string {
	r2Hosts := ""
	if r2 != nil {
		r2Hosts = " https://" + r2.Endpoint + " https://" + r2.Bucket + "." + r2.Endpoint
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; " +
		"img-src 'self' data:" + r2Hosts + "; media-src 'self'" + r2Hosts + "; " +
		"frame-src 'self'" + r2Hosts + "; connect-src 'self'" + r2Hosts + "; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
}
