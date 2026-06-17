// TestLiveE2ESDK is the real end-to-end test the way a client actually uses the
// gateway: a real Huawei OBS Java SDK drives the gateway's S3 endpoint OVER HTTP
// (not the internal Go API), backed by a live 0G deployment. Flow:
//
//	SDK putObject ─► gateway ─► (worker) submit tx + upload segments to 0G ─► finalized
//	         ─► drop local cache ─► SDK getObject ─► proof-verified cold read from 0G
//
// Skipped unless ZGS_E2E=1 (plus ZGS_PRIVATE_KEY; ZGS_NODES / ZGS_ETH_RPC default
// to the demo testnet) and a working JDK + OBS bundle jar are present:
//
//	set -a; . ./.env; set +a            # exports ZGS_PRIVATE_KEY (gitignored)
//	ZGS_E2E=1 go test ./integration/ -run TestLiveE2ESDK -v -timeout 12m
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/chain"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/s3gw"
	"zgs-gateway/internal/store"
	"zgs-gateway/internal/uploader"
)

func TestLiveE2ESDK(t *testing.T) {
	if os.Getenv("ZGS_E2E") == "" {
		t.Skip("set ZGS_E2E=1 (+ ZGS_PRIVATE_KEY) to run the live SDK e2e")
	}
	key := os.Getenv("ZGS_PRIVATE_KEY")
	if key == "" {
		t.Fatal("ZGS_E2E set but ZGS_PRIVATE_KEY missing")
	}
	javac := workingJDKTool(t, "javac")
	java := workingJDKTool(t, "java")
	jdir := filepath.Join("testdata", "obs-java")
	jars, _ := filepath.Glob(filepath.Join(jdir, "lib", "esdk-obs-java-bundle-*.jar"))
	if len(jars) == 0 {
		t.Skip("OBS Java bundle jar not present; see obssdkjava_test.go header for the fetch command")
	}
	jar := jars[0]

	nodes := strings.Split(envOr("ZGS_NODES", "http://47.84.224.253:5678,http://47.84.225.228:5678"), ",")
	rpc := envOr("ZGS_ETH_RPC", "https://evmtestnet.confluxrpc.com")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	w := uploader.New(st, backend, uploader.Config{BatchMax: 10, MaxRetries: 3, PathOf: svc.CachePath})

	b := s3gw.New(ctx, svc, st)
	faker := gofakes3.New(b, gofakes3.WithAutoBucket(true))
	ts := httptest.NewServer(b.Wrap(faker.Server())) // same middleware stack as main.go
	defer ts.Close()

	// compile the OBS Java client
	classes := t.TempDir()
	if out, err := exec.Command(javac, "-cp", jar, "-d", classes, filepath.Join(jdir, "ObsE2E.java")).CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}

	bucket, keyName := "e2e", "live/obj.bin"
	// unique body so every run exercises a fresh 0G root (never a dedup hit)
	rnd := make([]byte, 16)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	body := "zgs-e2e-" + hex.EncodeToString(rnd)

	runJava := func(phase string) (string, error) {
		cmd := exec.Command(java, "-cp", classes+string(os.PathListSeparator)+jar, "ObsE2E")
		cmd.Env = append(os.Environ(),
			"OBS_ENDPOINT="+ts.URL,
			"OBS_BUCKET="+bucket,
			"OBS_KEY="+keyName,
			"OBS_BODY="+body,
			"OBS_PHASE="+phase,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. PUT through the OBS SDK over HTTP
	if out, err := runJava("put"); err != nil {
		t.Fatalf("SDK put: %v\n%s", err, out)
	} else {
		t.Logf("SDK put: %s", strings.TrimSpace(out))
	}

	// the gateway assigned a content-addressed root for this bucket/key
	root, ok, err := st.S3GetObjectKey(bucket, keyName)
	if err != nil || !ok {
		t.Fatalf("resolve root after put: ok=%v err=%v", ok, err)
	}
	t.Logf("root=%s", root)

	// 2. submit + upload to 0G, then wait for finality
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush (submit+upload): %v", err)
	}
	deadline := time.Now().Add(6 * time.Minute)
	for {
		if err := w.PollFinality(ctx); err != nil {
			t.Logf("poll finality: %v", err)
		}
		m, _, _ := st.Get(root)
		if m.Status == store.StatusFinalized {
			t.Logf("finalized, tx=%s", m.TxHash)
			break
		}
		if m.Status == store.StatusFailed {
			t.Fatalf("upload failed: %+v", m)
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finalized in time: status=%s tx=%s", m.Status, m.TxHash)
		}
		time.Sleep(5 * time.Second)
	}

	// 3. drop the local cache so the next read must come from 0G
	if err := os.Remove(svc.CachePath(root)); err != nil {
		t.Fatalf("drop cache: %v", err)
	}

	// 4. GET through the OBS SDK → proof-verified cold read from 0G, bytes verified
	if out, err := runJava("get"); err != nil {
		t.Fatalf("SDK get (cold read from 0G): %v\n%s", err, out)
	} else {
		t.Logf("SDK get: %s", strings.TrimSpace(out))
	}
}
