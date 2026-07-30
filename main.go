package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"melodyshare/internal/auth"
	"melodyshare/internal/config"
	"melodyshare/internal/server"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

//go:embed web
var embeddedWeb embed.FS

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "share.db"))
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Credentials saved via the settings page (stored in the DB) override the
	// env vars; env is required only for the first run.
	saved, err := st.AllSettings()
	if err != nil {
		slog.Error("load settings", "err", err)
		os.Exit(1)
	}
	username := cfg.Username
	if saved["username"] != "" {
		username = saved["username"]
	}
	var am *auth.Manager
	if hash := saved["password_hash"]; hash != "" {
		am, err = auth.NewWithHash(cfg.DataDir, username, hash, st)
	} else if cfg.Password != "" {
		am, err = auth.New(cfg.DataDir, username, cfg.Password, st)
	} else {
		slog.Error("SHARE_PASSWORD is required on first run (no password saved in settings yet)")
		os.Exit(1)
	}
	if err != nil {
		slog.Error("auth", "err", err)
		os.Exit(1)
	}

	local, err := storage.NewLocal(cfg.DataDir)
	if err != nil {
		slog.Error("local storage", "err", err)
		os.Exit(1)
	}

	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		slog.Error("embed web", "err", err)
		os.Exit(1)
	}
	srv, err := server.New(cfg, st, am, local, nil, webFS)
	if err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cleanupLoop(ctx, srv)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// Bound slowloris on the request body; large uploads still fit within
		// this window because chunk PUTs are individual requests.
		ReadTimeout: 15 * time.Minute,
		IdleTimeout: 2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(shutCtx)
	}()

	if cfg.ChunkSize > 0 {
		slog.Info("listening", "addr", cfg.Addr, "chunk_mib", cfg.ChunkSize/(1024*1024))
	} else {
		slog.Info("listening", "addr", cfg.Addr, "chunk_mib", "auto")
	}
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	slog.Info("bye")
}

// cleanupLoop removes expired files and abandoned partial uploads.
func cleanupLoop(ctx context.Context, srv *server.Server) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		cleanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := srv.Cleanup(cleanCtx); err != nil {
			slog.Error("cleanup", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
