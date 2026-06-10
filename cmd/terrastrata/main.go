// Command terrastrata is a pull-through cache proxy implementing the Terraform
// provider network mirror protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
	"github.com/pascalinthecloud/terrastrata/internal/config"
	"github.com/pascalinthecloud/terrastrata/internal/httpx"
	"github.com/pascalinthecloud/terrastrata/internal/mirror"
	"github.com/pascalinthecloud/terrastrata/internal/observ"
	"github.com/pascalinthecloud/terrastrata/internal/prewarm"
)

// Build metadata, injected via -ldflags at build time (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("terrastrata exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("terrastrata %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	logger := observ.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	// Cache: local layer always present; S3 added as the durable layer when
	// configured. Layered handles a nil durable layer transparently.
	local, err := cache.NewLocal(cfg.CacheDir)
	if err != nil {
		return err
	}
	var durable cache.Cache
	if cfg.S3.Enabled() {
		durable = cache.NewS3(cfg.S3)
		logger.Info("durable S3 cache enabled", "bucket", cfg.S3.Bucket, "endpoint", cfg.S3.Endpoint)
	}
	blobCache := cache.NewLayered(local, durable, logger)

	metrics := observ.NewMetrics()
	upstream := mirror.NewUpstream(cfg.UpstreamBase, "terrastrata/"+version, cfg.UpstreamTimeout)
	handler, err := mirror.NewHandler(mirror.Options{
		Cache:    blobCache,
		Upstream: upstream,
		Metrics:  metrics,
		// Stage zips under the cache dir: the container root filesystem is
		// read-only, so this is the writable volume available for verification.
		StagingDir: filepath.Join(cfg.CacheDir, ".staging"),
		IndexTTL:   cfg.IndexTTL,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	srv := buildServer(cfg, handler, metrics, logger)

	logger.Info("starting terrastrata",
		"version", version,
		"addr", cfg.ListenAddr,
		"upstream", cfg.UpstreamBase,
		"cache_dir", cfg.CacheDir,
		"s3", cfg.S3.Enabled(),
		"auth", cfg.AuthToken != "",
		"index_ttl", cfg.IndexTTL,
		"prewarm", len(cfg.PrewarmProviders),
		"cache_max_bytes", cfg.CacheMaxBytes,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bound the local cache when a budget is configured (evicts LRU files).
	if cfg.CacheMaxBytes > 0 {
		evictor := cache.NewEvictor(cfg.CacheDir, cfg.CacheMaxBytes, metrics, logger)
		go evictor.Run(ctx)
	}

	// Pre-warm in the background so it never blocks startup or /health. It
	// replays requests against the raw mirror routes (no auth/middleware).
	if len(cfg.PrewarmProviders) > 0 {
		mirrorMux := http.NewServeMux()
		handler.Routes(mirrorMux)
		go prewarm.Run(ctx, mirrorMux, cfg.PrewarmProviders, cfg.PrewarmPlatforms, metrics, logger)
	}

	return serve(ctx, srv, logger)
}

// buildServer assembles the routing tree and the hardened http.Server.
//
// Routing: /health and /metrics are unauthenticated operational endpoints; all
// mirror traffic is wrapped in optional bearer auth. Cross-cutting middleware
// (recovery, request-id, metrics, logging) wraps the whole tree.
func buildServer(cfg config.Config, h *mirror.Handler, metrics *observ.Metrics, logger *slog.Logger) *http.Server {
	mirrorMux := http.NewServeMux()
	h.Routes(mirrorMux)

	root := http.NewServeMux()
	root.Handle("GET /health", healthHandler())
	root.Handle("GET /metrics", metrics.Handler())
	root.Handle("/", httpx.BearerAuth(cfg.AuthToken)(mirrorMux))

	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "terrastrata: Terraform provider network mirror", http.StatusNotFound)
	})

	handler := httpx.Chain(root,
		httpx.Recovery(logger),
		httpx.RequestID,
		metrics.Middleware, // creates the ResponseRecorder reused downstream
		httpx.Logging(logger),
	)

	return &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: handler,
		// Slowloris protection without capping large zip transfers: bound the
		// time to read headers and keep-alive idle, but leave WriteTimeout off so
		// big downloads on slow links are not severed mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// serve runs the server until ctx is cancelled (termination signal), then drains
// connections.
func serve(ctx context.Context, srv *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// healthHandler reports liveness/readiness. It is intentionally dependency-free:
// the local cache directory must exist for the process to have started, and the
// upstream is reached lazily, so a simple OK is the right liveness signal.
func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version,
		})
	})
}
