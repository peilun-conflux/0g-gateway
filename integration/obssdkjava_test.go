// The OBS-Java test drives the gateway's S3-compatible endpoint (gofakes3 +
// s3gw, backed by a local object.Service — no real 0G) with the real Huawei OBS
// *Java* SDK (esdk-obs-java-bundle), which is what the integration partner uses.
//
// It auto-skips unless a JDK (javac/java) is on PATH and an OBS Java bundle jar
// (esdk-obs-java-bundle-*.jar) is present under testdata/obs-java/lib/. Any 3.x
// bundle works; the partner uses >=3.21.11. Fetch one with, e.g.:
//
//	V=3.21.11
//	curl -sLo integration/testdata/obs-java/lib/esdk-obs-java-bundle-$V.jar \
//	  https://repo1.maven.org/maven2/com/huaweicloud/esdk-obs-java-bundle/$V/esdk-obs-java-bundle-$V.jar
//	go test ./integration/ -run TestOBSJavaSDK -v
//
// The jar and compiled .class files live under testdata/ (git-ignored) so
// `go test ./...` ignores them.
package integration

import (
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

func TestOBSJavaSDK(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not on PATH; skipping OBS Java SDK compatibility test")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not on PATH; skipping OBS Java SDK compatibility test")
	}
	dir := filepath.Join("testdata", "obs-java")
	jars, _ := filepath.Glob(filepath.Join(dir, "lib", "esdk-obs-java-bundle-*.jar"))
	if len(jars) == 0 {
		t.Skip("OBS Java bundle jar not present under testdata/obs-java/lib/; see this file's header for the fetch command")
	}
	jar := jars[0] // any 3.x bundle works; the partner uses >=3.21.11
	t.Logf("using OBS Java bundle: %s", jar)

	// Compile ObsCompatTest.java into a temp dir against the bundle jar.
	classes := t.TempDir()
	if out, err := exec.Command(javac, "-cp", jar, "-d", classes, filepath.Join(dir, "ObsCompatTest.java")).CombinedOutput(); err != nil {
		t.Fatalf("javac failed: %v\n%s", err, out)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := object.New(st, localDL{}, object.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	b := s3gw.New(svc, st)
	faker := gofakes3.New(b, gofakes3.WithAutoBucket(true))
	ts := httptest.NewServer(b.Wrap(faker.Server())) // same middleware stack as main.go
	defer ts.Close()

	cmd := exec.Command(java, "-cp", classes+string(os.PathListSeparator)+jar, "ObsCompatTest")
	cmd.Env = append(os.Environ(),
		"OBS_ENDPOINT="+ts.URL,
		"OBS_AK=demoAK",
		"OBS_SK=demoSK",
		"OBS_BUCKET=demo",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("java ObsCompatTest output:\n%s", out)
	if err != nil {
		t.Fatalf("OBS Java SDK compatibility test failed: %v", err)
	}
}
