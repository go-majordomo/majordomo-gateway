// Package app wires all gateway dependencies and returns a running Server.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/auth"
	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/controllers"
	"github.com/go-majordomo/majordomo-gateway/internal/deprecated"
	"github.com/go-majordomo/majordomo-gateway/internal/migrate"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
	"github.com/go-majordomo/majordomo-gateway/internal/proxy"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/requestlog"
	"github.com/go-majordomo/majordomo-gateway/internal/server"
	"github.com/go-majordomo/majordomo-gateway/internal/storage"
)

// Server is a fully initialised gateway ready to call Start() on.
type Server struct {
	inner     *server.Server
	logWriter *requestlog.Writer
	pricing   *pricing.Service
}

// Start begins listening for HTTP requests. It blocks until the server stops.
func (s *Server) Start() error {
	return s.inner.Start()
}

// ShutdownWithTimeout gracefully stops the server, then closes the log writer and
// pricing service.
func (s *Server) ShutdownWithTimeout(timeout time.Duration) error {
	err := s.inner.ShutdownWithTimeout(timeout)
	s.pricing.Close()
	s.logWriter.Close()
	return err
}

// Build initialises all gateway dependencies and returns a Server.
func Build(ctx context.Context, cfg *config.Config) (*Server, error) {
	// ── Database ──────────────────────────────────────────────────────────────
	db, err := repositories.Connect(ctx, cfg.Storage.Postgres.DSN(), cfg.Storage.Postgres.MaxConns)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	// ── Migrations ────────────────────────────────────────────────────────────
	if err := migrate.Run(db.DB, "./migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	slog.Info("migrations applied")

	// ── Metadata infrastructure (HLL cardinality + active-keys cache) ─────────
	repoCfg := repositories.DefaultConfig()
	if cfg.Metadata.HLLFlushInterval != 0 {
		repoCfg.HLLFlushInterval = cfg.Metadata.HLLFlushInterval
	}
	if cfg.Metadata.ActiveKeysCacheTTL != 0 {
		repoCfg.ActiveKeysCacheTTL = cfg.Metadata.ActiveKeysCacheTTL
	}
	activeKeyCache := repositories.NewActiveKeysCache(db, repoCfg.ActiveKeysCacheTTL)
	hllManager := repositories.NewHLLManager(db, repoCfg.HLLFlushInterval)
	if err := hllManager.LoadFromDB(ctx); err != nil {
		slog.Warn("failed to load HLL state from DB", "error", err)
	}
	hllManager.Start()

	// ── Repositories ──────────────────────────────────────────────────────────
	apiKeyRepo := repositories.NewAPIKeyRepository(db)
	usageRepo := repositories.NewUsageRepository(db)
	metadataKeyRepo := repositories.NewMetadataKeyRepository(db, activeKeyCache)

	// ── Request Log Writer ────────────────────────────────────────────────────
	logWriter := requestlog.New(db, activeKeyCache, hllManager)

	// ── Pricing ───────────────────────────────────────────────────────────────
	pricingSvc := pricing.NewService(
		cfg.Pricing.RemoteURL,
		cfg.Pricing.FallbackFile,
		cfg.Pricing.AliasesFile,
		cfg.Pricing.RefreshInterval,
	)

	// ── Deprecated Models ─────────────────────────────────────────────────────
	deprecatedSvc := deprecated.NewService(cfg.Pricing.DeprecatedModelsFile)

	// ── Auth ──────────────────────────────────────────────────────────────────
	resolver := auth.NewResolver(apiKeyRepo)

	// ── Body archival (optional S3/GCS object storage) ────────────────────────
	bodyStore, err := buildBodyStore(ctx, cfg)
	if err != nil {
		logWriter.Close()
		pricingSvc.Close()
		return nil, err
	}

	// ── Proxy Handler ─────────────────────────────────────────────────────────
	proxyHandler := proxy.NewHandler(logWriter, pricingSvc, deprecatedSvc, resolver, cfg, bodyStore)

	// ── Controllers (admin/query API) ─────────────────────────────────────────
	keysController := controllers.NewKeysController(apiKeyRepo)
	usageController := controllers.NewUsageController(usageRepo, bodyStore)
	metadataController := controllers.NewMetadataController(metadataKeyRepo)
	if cfg.Admin.Token == "" {
		slog.Warn("ADMIN_TOKEN not set — key management and usage query API are disabled; proxy traffic only")
	}

	// ── HTTP Server ───────────────────────────────────────────────────────────
	srv := server.New(&cfg.Server, proxyHandler, logWriter, cfg.Admin.Token, keysController, usageController, metadataController)

	return &Server{
		inner:     srv,
		logWriter: logWriter,
		pricing:   pricingSvc,
	}, nil
}

// buildBodyStore constructs the optional request/response body archive from config.
// Returns nil (archival disabled) for the "none"/empty backend.
func buildBodyStore(ctx context.Context, cfg *config.Config) (storage.Store, error) {
	switch cfg.BodyStore.Backend {
	case "", "none":
		return nil, nil
	case "s3":
		store, err := storage.NewS3Store(ctx, storage.S3Config{
			Bucket:   cfg.BodyStore.S3Bucket,
			Region:   cfg.BodyStore.S3Region,
			Endpoint: cfg.BodyStore.S3Endpoint,
		})
		if err != nil {
			return nil, fmt.Errorf("init s3 body store: %w", err)
		}
		slog.Info("body archival enabled", "backend", "s3", "bucket", cfg.BodyStore.S3Bucket)
		return store, nil
	case "gcs":
		store, err := storage.NewGCSStore(ctx, storage.GCSConfig{Bucket: cfg.BodyStore.GCSBucket})
		if err != nil {
			return nil, fmt.Errorf("init gcs body store: %w", err)
		}
		slog.Info("body archival enabled", "backend", "gcs", "bucket", cfg.BodyStore.GCSBucket)
		return store, nil
	default:
		return nil, fmt.Errorf("unknown BODY_STORAGE backend %q (want none|s3|gcs)", cfg.BodyStore.Backend)
	}
}
