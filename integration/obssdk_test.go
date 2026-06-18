// Package integration's OBS-JS test drives the gateway's S3-compatible endpoint
// (gofakes3 + s3gw, backed by a local object.Service — no real 0G) with the
// real Huawei OBS Node.js SDK, validating end-to-end protocol compatibility.
//
// It auto-skips unless `node` is on PATH and the SDK is installed:
//
//	cd integration/testdata/obs-js && npm install
//	go test ./integration/ -run TestOBSJavaScriptSDK -v
//
// The npm fixture lives under testdata/ so `go test ./...` ignores node_modules.
package integration

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/object"
	"zgs-gateway/internal/s3gw"
	"zgs-gateway/internal/store"
)

// localDL is a no-op downloader: the test only reads freshly-written objects
// (cache hits), so cold restore from 0G is never exercised.
type localDL struct{}

func (localDL) Download(_ context.Context, _, _ string) error { return os.ErrNotExist }

// newLocalS3Server stands up the gateway's S3 endpoint backed by a local no-op
// downloader (no real 0G) — the shared wiring for the OBS SDK compatibility
// harnesses, using the same middleware stack (b.Wrap) as main.go.
func newLocalS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := object.New(st, localDL{}, object.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b := s3gw.New(context.Background(), svc, st)
	faker := gofakes3.New(b, gofakes3.WithAutoBucket(true))
	ts := httptest.NewServer(b.Wrap(faker.Server()))
	t.Cleanup(ts.Close)
	return ts
}

func TestOBSJavaScriptSDK(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping OBS JS SDK compatibility test")
	}
	script := filepath.Join("testdata", "obs-js", "obs_sdk_test.js")
	if _, err := os.Stat(filepath.Join("testdata", "obs-js", "node_modules", "esdk-obs-nodejs")); err != nil {
		t.Skip("esdk-obs-nodejs not installed; run: cd integration/testdata/obs-js && npm install")
	}

	ts := newLocalS3Server(t)

	cmd := exec.Command("node", script)
	cmd.Env = append(os.Environ(),
		"OBS_ENDPOINT="+ts.URL,
		"OBS_AK=demoAK",
		"OBS_SK=demoSK",
		"OBS_BUCKET=demo",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("node %s output:\n%s", script, out)
	if err != nil {
		t.Fatalf("OBS JS SDK compatibility test failed: %v", err)
	}
}
