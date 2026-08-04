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
	"github.com/pascalinthecloud/terrastrata/internal/modules"
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
	// configured. Layered handles a nil durable layer transparently. Access
	// tracking (an extra syscall per read) is only enabled when eviction needs
	// the LRU signal.
	var localOpts []cache.LocalOption
	if cfg.CacheMaxBytes > 0 {
		localOpts = append(localOpts, cache.WithAccessTracking())
	}
	local, err := cache.NewLocal(cfg.CacheDir, localOpts...)
	if err != nil {
		return err
	}
	var durable cache.Cache
	if cfg.S3.Enabled() {
		s3c, err := cache.NewS3(context.Background(), cfg.S3)
		if err != nil {
			return err
		}
		durable = s3c
		logger.Info("durable S3 cache enabled",
			"bucket", cfg.S3.Bucket, "endpoint", cfg.S3.Endpoint,
			"static_credentials", cfg.S3.AccessKey != "")
	}
	blobCache := cache.NewLayered(local, durable, logger)

	metrics := observ.NewMetrics()
	// One upstream client per mirrored registry. They share the cache, which is
	// safe because every cache key is namespaced by hostname.
	upstreams := make(map[string]*mirror.Upstream, len(cfg.Upstreams))
	for _, up := range cfg.Upstreams {
		upstreams[up.Hostname] = mirror.NewUpstream(up.Base, "terrastrata/"+version, cfg.UpstreamTimeout)
	}
	handler, err := mirror.NewHandler(mirror.Options{
		Cache:     blobCache,
		Upstreams: upstreams,
		Metrics:   metrics,
		// Stage zips under the cache dir: the container root filesystem is
		// read-only, so this is the writable volume available for verification.
		StagingDir: filepath.Join(cfg.CacheDir, ".staging"),
		IndexTTL:   cfg.IndexTTL,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	// Optional module registry. Unlike providers there is no mirror protocol
	// for modules, so terrastrata serves the registry protocol directly and
	// clients address it by source = "<this host>/<ns>/<name>/<system>".
	var modHandler *modules.Handler
	if cfg.Modules.Enabled {
		modHandler, err = modules.NewHandler(modules.Options{
			Cache:       blobCache,
			Upstream:    modules.NewUpstream(cfg.Modules.UpstreamBase, "terrastrata/"+version, cfg.UpstreamTimeout),
			Metrics:     metrics,
			StagingDir:  filepath.Join(cfg.CacheDir, ".staging"),
			VersionsTTL: cfg.IndexTTL,
			Logger:      logger,
		})
		if err != nil {
			return err
		}
	}

	srv := buildServer(cfg, handler, modHandler, metrics, logger)

	// Log every mirrored registry: with several configured, "which hostname maps
	// where" is the first thing to check when a client gets an unexpected 404.
	served := make([]string, 0, len(cfg.Upstreams))
	for _, up := range cfg.Upstreams {
		served = append(served, up.Hostname+" -> "+up.Base)
	}

	logger.Info("starting terrastrata",
		"version", version,
		"addr", cfg.ListenAddr,
		"upstreams", served,
		"cache_dir", cfg.CacheDir,
		"s3", cfg.S3.Enabled(),
		"auth", cfg.AuthToken != "",
		"index_ttl", cfg.IndexTTL,
		"prewarm", len(cfg.PrewarmProviders),
		"cache_max_bytes", cfg.CacheMaxBytes,
		"modules", cfg.Modules.Enabled,
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
		go prewarm.Run(ctx, mirrorMux, cfg.MirrorHostname, cfg.PrewarmProviders, cfg.PrewarmPlatforms, metrics, logger)
	}

	return serve(ctx, srv, logger)
}

// buildServer assembles the routing tree and the hardened http.Server.
//
// Routing: /health and /metrics are unauthenticated operational endpoints; all
// mirror traffic is wrapped in optional bearer auth. Cross-cutting middleware
// (recovery, request-id, metrics, logging) wraps the whole tree.
//
// The provider mirror patterns are confined to their own mux on purpose. The
// module route /v1/modules/{ns}/{name}/{system}/{version}/download and the
// provider route /{hostname}/{ns}/{type}/{version}/download/{platform}/{filename}
// both match some paths with neither being more specific, so registering them on
// one ServeMux panics at startup. Keeping providers in mirrorMux and modules on
// root makes that collision structurally impossible.
func buildServer(cfg config.Config, h *mirror.Handler, mods *modules.Handler, metrics *observ.Metrics, logger *slog.Logger) *http.Server {
	mirrorMux := http.NewServeMux()
	h.Routes(mirrorMux)

	root := http.NewServeMux()
	root.Handle("GET /health", healthHandler())
	root.Handle("GET /metrics", metrics.Handler())
	root.Handle("/", httpx.BearerAuth(cfg.AuthToken)(mirrorMux))

	if mods != nil {
		moduleMux := http.NewServeMux()
		mods.RoutesMeta(moduleMux)

		// Service discovery must be unauthenticated: it is the first request a
		// client makes, before it looks up any credentials for the host.
		root.HandleFunc("GET /.well-known/terraform.json", mods.Discovery)
		// The archive endpoint must also stay unauthenticated. Terraform sends
		// registry credentials only to registry endpoints; the go-getter fetch of
		// X-Terraform-Get that follows carries no Authorization header, so putting
		// the archive behind auth would break terraform init whenever AUTH_TOKEN
		// is set. The exact pattern takes precedence over the /v1/modules/
		// subtree below.
		root.HandleFunc(modules.ArchivePattern, mods.ArchiveHandler())
		root.Handle("/v1/modules/", httpx.BearerAuth(cfg.AuthToken)(moduleMux))
	}

	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "terrastrata: Terraform provider network mirror", http.StatusNotFound)
	})

	// Recovery sits innermost so the 500 it writes lands in the shared
	// ResponseRecorder and panicking requests still show up in metrics and the
	// access log (both do their accounting after next.ServeHTTP returns).
	handler := httpx.Chain(root,
		httpx.RequestID,
		metrics.Middleware, // creates the ResponseRecorder reused downstream
		httpx.Logging(logger),
		httpx.Recovery(logger),
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
