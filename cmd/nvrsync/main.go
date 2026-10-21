// Command nvrsync synchronises NVR recordings locally and serves them over
// HTTP as an HLS VOD library.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/myml/nvrsync/internal/api"
	"github.com/myml/nvrsync/internal/config"
	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
)

// Version is the release version reported by /api/health and startup logs.
// It is overridden at build time via -ldflags "-X main.Version=...".
var Version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store %q: %w", cfg.DataDir, err)
	}
	defer st.Close()

	sy := syncer.New(st)
	pl := play.New(st)
	handler := api.New(st, sy, pl, nil, Version)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// Media segments can be large and are served over possibly slow LANs.
		WriteTimeout:   30 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       log.Default(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// AutoLoop reads the current settings from the store on every iteration,
	// so settings saved through PUT /api/settings take effect without a restart.
	go sy.AutoLoop(ctx)
	go sy.CatalogLoop(ctx)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	set := st.Settings()
	log.Printf("nvrsync %s listening on %s (data=%s, nvr=%s, channels=%v)",
		Version, cfg.Listen, cfg.DataDir, set.NVRHost, set.Channels)

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop()
		log.Printf("shutdown requested, stopping syncer and HTTP server")
		sy.Stop()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		log.Printf("bye")
		return nil
	}
}
