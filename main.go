package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
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
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "share.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// Credentials saved via the settings page (stored in the DB) override the
	// env vars; env is required only for the first run.
	saved, err := st.AllSettings()
	if err != nil {
		log.Fatal(err)
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
		log.Fatal("SHARE_PASSWORD is required on first run (no password saved in settings yet)")
	}
	if err != nil {
		log.Fatal(err)
	}

	local, err := storage.NewLocal(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		log.Fatal(err)
	}
	srv, err := server.New(cfg, st, am, local, nil, webFS)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cleanupLoop(ctx, srv)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		log.Print("shutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(shutCtx)
	}()

	log.Printf("listening on %s (chunk size %d MiB)", cfg.Addr, cfg.ChunkSize/(1024*1024))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("bye")
}

// cleanupLoop removes expired files and abandoned partial uploads.
func cleanupLoop(ctx context.Context, srv *server.Server) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		cleanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := srv.Cleanup(cleanCtx); err != nil {
			log.Printf("cleanup: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
