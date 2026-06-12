// Package integration holds the live end-to-end test against a real 0G
// deployment. It is skipped unless ZGS_E2E=1; configuration comes from env:
//
//	ZGS_E2E=1 ZGS_PRIVATE_KEY=<hex> \
//	ZGS_NODES=http://node1:5678,http://node2:5678 \
//	ZGS_ETH_RPC=https://evmtestnet.confluxrpc.com \
//	go test ./integration/ -v -timeout 10m
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zgs-gateway/internal/chain"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/store"
	"zgs-gateway/internal/uploader"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestLiveE2E(t *testing.T) {
	if os.Getenv("ZGS_E2E") == "" {
		t.Skip("set ZGS_E2E=1 (plus ZGS_PRIVATE_KEY / ZGS_NODES / ZGS_ETH_RPC) to run the live test")
	}
	key := os.Getenv("ZGS_PRIVATE_KEY")
	if key == "" {
		t.Fatal("ZGS_E2E set but ZGS_PRIVATE_KEY missing")
	}
	nodes := strings.Split(envOr("ZGS_NODES", "http://47.84.224.253:5678,http://47.84.225.228:5678"), ",")
	rpc := envOr("ZGS_ETH_RPC", "https://evmtestnet.confluxrpc.com")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	backend, err := chain.New(ctx, chain.Options{
		Nodes:           nodes,
		EthRPC:          rpc,
		PrivateKey:      key,
		ExpectedReplica: uint(len(nodes)),
	})
	if err != nil {
		t.Fatalf("chain backend: %v", err)
	}
	defer backend.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := object.New(st, backend, object.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	w := uploader.New(st, backend, uploader.Config{
		BatchMax:   10,
		MaxRetries: 3,
		PathOf:     svc.CachePath,
	})

	// unique random content so every run exercises a fresh root end to end
	content := make([]byte, 3000)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}

	m, _, err := svc.Put(ctx, bytes.NewReader(content), "e2e.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Logf("root=%s sha256=%s", m.Root, m.SHA256)

	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush (submit+upload): %v", err)
	}
	got, _, _ := st.Get(m.Root)
	t.Logf("after flush: status=%s tx=%s", got.Status, got.TxHash)
	if got.Status != store.StatusOnchain && got.Status != store.StatusFinalized {
		t.Fatalf("unexpected status after flush: %+v", got)
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		if err := w.PollFinality(ctx); err != nil {
			t.Logf("poll finality: %v", err)
		}
		got, _, _ = st.Get(m.Root)
		if got.Status == store.StatusFinalized {
			break
		}
		if got.Status == store.StatusFailed {
			t.Fatalf("upload failed: %+v", got)
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finalized in time: %+v", got)
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("finalized, tx=%s", got.TxHash)

	// cold read: drop the cache and force a proof-verified download from 0G
	if err := os.Remove(svc.CachePath(m.Root)); err != nil {
		t.Fatal(err)
	}
	f, _, err := svc.Open(ctx, m.Root)
	if err != nil {
		t.Fatalf("cold open: %v", err)
	}
	back, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(back, content) {
		t.Fatalf("cold read mismatch: %d bytes", len(back))
	}
	t.Log("cold read from 0G verified byte-identical")
}
