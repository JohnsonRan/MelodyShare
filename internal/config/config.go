package config

import (
	"fmt"
	"os"
	"strconv"
)

type R2 struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
}

type Config struct {
	Addr      string
	DataDir   string
	SiteName  string
	Username  string
	Password  string // may be empty when a password was saved via the settings page
	BaseURL   string
	ChunkSize int64
	R2        *R2
}

func Load() (*Config, error) {
	cfg := &Config{
		Addr:     getenv("SHARE_ADDR", ":8080"),
		DataDir:  getenv("SHARE_DATA_DIR", "./data"),
		SiteName: getenv("SHARE_SITE_NAME", "MelodyShare"),
		Username: getenv("SHARE_USERNAME", "admin"),
		Password: os.Getenv("SHARE_PASSWORD"),
		BaseURL:  os.Getenv("SHARE_BASE_URL"),
	}

	// "auto" (the default) sizes chunks per upload from the file size;
	// ChunkSize == 0 encodes auto throughout.
	chunkMB := getenv("SHARE_CHUNK_SIZE_MB", "auto")
	if chunkMB == "auto" || chunkMB == "0" {
		cfg.ChunkSize = 0
	} else {
		mb, err := strconv.Atoi(chunkMB)
		if err != nil || mb < 5 || mb > 95 {
			return nil, fmt.Errorf("SHARE_CHUNK_SIZE_MB must be \"auto\" or an integer between 5 and 95, got %q", chunkMB)
		}
		cfg.ChunkSize = int64(mb) * 1024 * 1024
	}

	r2 := R2{
		Endpoint:  os.Getenv("SHARE_R2_ENDPOINT"),
		AccessKey: os.Getenv("SHARE_R2_ACCESS_KEY"),
		SecretKey: os.Getenv("SHARE_R2_SECRET_KEY"),
		Bucket:    os.Getenv("SHARE_R2_BUCKET"),
	}
	switch {
	case r2.Endpoint != "" && r2.AccessKey != "" && r2.SecretKey != "" && r2.Bucket != "":
		cfg.R2 = &r2
	case r2.Endpoint != "" || r2.AccessKey != "" || r2.SecretKey != "" || r2.Bucket != "":
		return nil, fmt.Errorf("R2 is partially configured: SHARE_R2_ENDPOINT, SHARE_R2_ACCESS_KEY, SHARE_R2_SECRET_KEY and SHARE_R2_BUCKET must all be set")
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
