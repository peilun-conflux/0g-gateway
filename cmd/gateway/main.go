// Command gateway runs the 0G storage gateway: an OBS-shaped object store
// backed by a 0G storage deployment. Configuration is environment-based; see
// .env.example in the repository root.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/chain"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/s3gw"
	"zgs-gateway/internal/store"
	"zgs-gateway/internal/uploader"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Error("bad integer env", "key", k, "value", v)
		os.Exit(1)
	}
	return n
}

func main() {
	var (
		listen        = envOr("ZGS_GW_LISTEN", ":8080")
		dataDir       = envOr("ZGS_GW_DATA_DIR", "./data")
		nodesCSV      = os.Getenv("ZGS_NODES")
		ethRPC        = os.Getenv("ZGS_ETH_RPC")
		privateKey    = os.Getenv("ZGS_PRIVATE_KEY")
		maxSize       = envInt("ZGS_GW_MAX_SIZE", 4<<30) // one object = one root: cap at the SDK fragment size
		batchMax      = int(envInt("ZGS_GW_BATCH_MAX", 20))
		maxRetries    = int(envInt("ZGS_GW_MAX_RETRIES", 5))
		flushInterval = time.Duration(envInt("ZGS_GW_FLUSH_INTERVAL_MS", 3000)) * time.Millisecond
	)
	if nodesCSV == "" || ethRPC == "" || privateKey == "" {
		slog.Error("ZGS_NODES, ZGS_ETH_RPC and ZGS_PRIVATE_KEY are required")
		os.Exit(1)
	}
	if flushInterval <= 0 {
		slog.Error("ZGS_GW_FLUSH_INTERVAL_MS must be a positive integer")
		os.Exit(1)
	}
	nodes := strings.Split(nodesCSV, ",")
	replica := uint(envInt("ZGS_EXPECTED_REPLICA", int64(len(nodes))))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		slog.Error("open metadata store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	backend, err := chain.New(ctx, chain.Options{
		Nodes:           nodes,
		EthRPC:          ethRPC,
		PrivateKey:      privateKey,
		ExpectedReplica: replica,
	})
	if err != nil {
		slog.Error("init 0g backend", "err", err)
		os.Exit(1)
	}
	defer backend.Close()

	svc, err := object.New(st, backend, object.Config{DataDir: dataDir, MaxSize: maxSize})
	if err != nil {
		slog.Error("init object service", "err", err)
		os.Exit(1)
	}

	w := uploader.New(st, backend, uploader.Config{
		BatchMax:   batchMax,
		MaxRetries: maxRetries,
		PathOf:     svc.CachePath,
	})
	go w.Run(ctx, flushInterval)

	// The S3-compatible endpoint (gofakes3) is the gateway's sole interface:
	// OBS/S3 SDK clients address objects by bucket+key. Wrap applies the S3-compat
	// middlewares (X-Amz-Copy-Source normalization + Huawei-OBS-style image
	// processing ?x-image-process=image/resize,...) in front of gofakes3.
	// No signature verification — keep it bound to an internal interface only.
	s3Backend := s3gw.New(svc, st)
	faker := gofakes3.New(s3Backend, gofakes3.WithAutoBucket(true))
	httpSrv := &http.Server{
		Addr:    listen,
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

	slog.Warn("gateway endpoint is UNAUTHENTICATED (no S3 signature check); bind to an internal interface only", "addr", listen)
	slog.Info("gateway listening", "addr", listen, "nodes", nodes, "replica", replica)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
	slog.Info("gateway stopped")
}
