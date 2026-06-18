// Command gateway runs the 0G storage gateway: an OBS-shaped object store
// backed by a 0G storage deployment. Configuration comes from a YAML file
// and/or environment variables; see config.example.yaml and .env.example in the
// repository root.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/chain"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/s3gw"
	"zgs-gateway/internal/store"
	"zgs-gateway/internal/uploader"
)

func main() {
	configPath := flag.String("config", os.Getenv("ZGS_CONFIG"),
		"path to the YAML config file (defaults to ./config.yaml if present; env vars override file values)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	flushInterval := time.Duration(cfg.FlushIntervalMS) * time.Millisecond
	replica := cfg.ExpectedReplica
	if replica == 0 {
		replica = uint(len(cfg.Nodes))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "meta.db"))
	if err != nil {
		slog.Error("open metadata store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	backend, err := chain.New(ctx, chain.Options{
		Nodes:           cfg.Nodes,
		EthRPC:          cfg.EthRPC,
		PrivateKey:      cfg.PrivateKey,
		ExpectedReplica: replica,
	})
	if err != nil {
		slog.Error("init 0g backend", "err", err)
		os.Exit(1)
	}
	defer backend.Close()

	svc, err := object.New(st, backend, object.Config{DataDir: cfg.DataDir, MaxSize: cfg.MaxSize, CacheMaxBytes: cfg.CacheMaxBytes})
	if err != nil {
		slog.Error("init object service", "err", err)
		os.Exit(1)
	}

	w := uploader.New(st, backend, uploader.Config{
		BatchMax:   cfg.BatchMax,
		MaxRetries: cfg.MaxRetries,
		PathOf:     svc.CachePath,
	})
	go w.Run(ctx, flushInterval)

	// The S3-compatible endpoint (gofakes3) is the gateway's sole interface:
	// OBS/S3 SDK clients address objects by bucket+key. Wrap applies the S3-compat
	// middlewares (X-Amz-Copy-Source normalization + Huawei-OBS-style image
	// processing ?x-image-process=image/resize,...) in front of gofakes3.
	// No signature verification — keep it bound to an internal interface only.
	s3Backend := s3gw.New(ctx, svc, st)
	faker := gofakes3.New(s3Backend, gofakes3.WithAutoBucket(true))
	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: s3Backend.Wrap(faker.Server()),
		// ReadHeaderTimeout bounds slow-header (Slowloris) clients without
		// capping body transfer time, since objects can be multi-GB and take a
		// while to stream. IdleTimeout reaps idle keep-alive connections.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Warn("gateway endpoint is UNAUTHENTICATED (no S3 signature check); bind to an internal interface only", "addr", cfg.Listen)
	slog.Info("gateway listening", "addr", cfg.Listen, "nodes", cfg.Nodes, "replica", replica)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
	slog.Info("gateway stopped")
}
