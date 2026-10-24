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
var Version = "0.1.1"

// Commit and BuildDate are injected by the Docker/CI build. Together with
// Version they let /api/health identify the exact image that is running.
var (
	Commit    = ""
	BuildDate = ""
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

// applyResetAuth clears the stored login password when -reset-auth was passed.
// It is separate from run so the recovery flag can be tested without starting
// the HTTP server, and it deliberately lets the process continue starting.
func applyResetAuth(st *store.Store, reset bool) error {
	if !reset {
		return nil
	}
	if err := st.ResetAuth(); err != nil {
		return err
	}
	log.Printf("login protection reset: auth_enabled=false and auth_hash cleared; " +
		"set a new password in Settings (account security) or via POST /api/auth/password")
	return nil
}

func valueOrUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
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

	// Recovery path for a forgotten password. The reset happens before the
	// server starts so the next request sees an unprotected instance.
	if err := applyResetAuth(st, cfg.ResetAuth); err != nil {
		return fmt.Errorf("reset auth: %w", err)
	}

	sy := syncer.New(st)
	pl := play.New(st)
	handler := api.New(st, sy, pl, nil, Version, Commit, BuildDate)

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
	log.Printf("nvrsync %s (commit=%s build=%s) listening on %s (data=%s, nvr=%s, channels=%v)",
		Version, valueOrUnknown(Commit), valueOrUnknown(BuildDate), cfg.Listen, cfg.DataDir, set.NVRHost, set.Channels)

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
