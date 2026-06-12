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

	"zgs-gateway/internal/chain"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/server"
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
		authSecret    = os.Getenv("ZGS_GW_AUTH_SECRET")
		adminSecret   = os.Getenv("ZGS_GW_ADMIN_SECRET")
		maxSize       = envInt("ZGS_GW_MAX_SIZE", 4<<30) // one object = one root: cap at the SDK fragment size
		batchMax      = int(envInt("ZGS_GW_BATCH_MAX", 20))
		maxRetries    = int(envInt("ZGS_GW_MAX_RETRIES", 5))
		flushInterval = time.Duration(envInt("ZGS_GW_FLUSH_INTERVAL_MS", 3000)) * time.Millisecond
	)
	if nodesCSV == "" || ethRPC == "" || privateKey == "" {
		slog.Error("ZGS_NODES, ZGS_ETH_RPC and ZGS_PRIVATE_KEY are required")
		os.Exit(1)
	}
	nodes := strings.Split(nodesCSV, ",")
	replica := uint(envInt("ZGS_EXPECTED_REPLICA", int64(len(nodes))))
	if authSecret == "" {
		slog.Warn("ZGS_GW_AUTH_SECRET empty: object reads are UNAUTHENTICATED")
	}

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

	httpSrv := &http.Server{
		Addr:    listen,
		Handler: server.New(svc, st, server.Config{AuthSecret: authSecret, AdminSecret: adminSecret}),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("gateway listening", "addr", listen, "nodes", nodes, "replica", replica)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
	slog.Info("gateway stopped")
}
