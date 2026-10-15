// Command harnessctl-server runs the control plane: the admin and agent APIs,
// the health check and the embedded web UI. It is a reference host for the
// server library: people, teams and tokens come from one JSON file (see
// directory.go). Configuration is environment only; see server/config.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/tetriz-ai/tetriz-harness/server"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/config"
	"github.com/tetriz-ai/tetriz-harness/server/store"
	"github.com/tetriz-ai/tetriz-harness/web"
)

const shutdownGrace = 15 * time.Second

// version is set at link time with -ldflags "-X main.version=..."; empty
// falls back to the module version and VCS revision.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	v := info.Main.Version
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			v += "+" + s.Value[:12]
		}
	}
	return v
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// healthcheck probes the local /healthz so a container without a shell or
// curl can be health-checked by its own binary.
func healthcheck() int {
	addr := os.Getenv("HARNESS_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, err := loadDirectory(cfg.DirectoryFile)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	var blobs blob.Store
	switch cfg.BlobBackend {
	case "s3":
		blobs, err = blob.NewS3(ctx, blob.S3Config{
			Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, Region: cfg.S3Region,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, UseSSL: cfg.S3UseSSL, Prefix: cfg.S3Prefix,
		})
	default:
		blobs, err = blob.NewFS(cfg.BlobDir)
	}
	if err != nil {
		return fmt.Errorf("opening blob store: %w", err)
	}

	ui, hasUI := web.Dist()
	if !hasUI {
		ui = nil
	}
	h, err := server.New(server.Options{
		Store: st, Directory: dir, Blobs: blobs, AllowHooks: cfg.AllowHooks, Logger: log, UI: ui,
		Version: buildVersion(),
	})
	if err != nil {
		return fmt.Errorf("building handler: %w", err)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           dir.authenticate(h),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "blob_backend", cfg.BlobBackend, "allow_hooks", cfg.AllowHooks, "web_ui", hasUI,
			"persons", len(dir.persons), "teams", len(dir.teams))
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}
