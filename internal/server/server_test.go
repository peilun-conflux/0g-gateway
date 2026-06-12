package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zgs-gateway/internal/auth"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/store"
)

type fakeDL struct{ data map[string][]byte }

func (f *fakeDL) Download(_ context.Context, root, dest string) error {
	b, ok := f.data[root]
	if !ok {
		return fmt.Errorf("not on 0g: %s", root)
	}
	return os.WriteFile(dest, b, 0o644)
}

type env struct {
	ts  *httptest.Server
	st  *store.Store
	svc *object.Service
	dl  *fakeDL
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dl := &fakeDL{data: map[string][]byte{}}
	svc, err := object.New(st, dl, object.Config{DataDir: t.TempDir(), MaxSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(svc, st, cfg))
	t.Cleanup(ts.Close)
	return &env{ts: ts, st: st, svc: svc, dl: dl}
}

type putResp struct {
	Root   string `json:"root"`
	Status string `json:"status"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Dedup  bool   `json:"dedup"`
}

func (e *env) putMultipart(t *testing.T, content []byte, filename, ctype string) (*http.Response, putResp) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if ctype != "" {
		hdr.Set("Content-Type", ctype)
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/objects", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var pr putResp
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			t.Fatalf("decode put response: %v", err)
		}
	}
	resp.Body.Close()
	return resp, pr
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutGetRoundtrip(t *testing.T) {
	e := newEnv(t, Config{})
	content := randBytes(t, 2048)

	resp, pr := e.putMultipart(t, content, "报告.pdf", "application/pdf")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status: %d", resp.StatusCode)
	}
	if !strings.HasPrefix(pr.Root, "0x") || pr.Status != "pending" || pr.Size != int64(len(content)) {
		t.Fatalf("put response: %+v", pr)
	}

	get, err := http.Get(e.ts.URL + "/objects/" + pr.Root)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("get: %d, %d bytes", get.StatusCode, len(body))
	}
	if ct := get.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("content-type: %q", ct)
	}
	if cd := get.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename") {
		t.Fatalf("content-disposition: %q", cd)
	}

	// HEAD reports length without a body
	req, _ := http.NewRequest(http.MethodHead, e.ts.URL+"/objects/"+pr.Root, nil)
	head, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusOK || head.Header.Get("Content-Length") != strconv.Itoa(len(content)) {
		t.Fatalf("head: %d len=%s", head.StatusCode, head.Header.Get("Content-Length"))
	}
}

func TestRawBodyPut(t *testing.T) {
	e := newEnv(t, Config{})
	content := randBytes(t, 100)
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/objects", bytes.NewReader(content))
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-Filename", "shot.png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var pr putResp
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || pr.Root == "" {
		t.Fatalf("raw put: %d %+v", resp.StatusCode, pr)
	}
	m, ok, _ := e.st.Get(pr.Root)
	if !ok || m.ContentType != "image/png" || m.Filename != "shot.png" {
		t.Fatalf("raw put meta: %+v", m)
	}
}

func TestRangeRequest(t *testing.T) {
	e := newEnv(t, Config{})
	content := randBytes(t, 1000)
	_, pr := e.putMultipart(t, content, "v.mp4", "video/mp4")

	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/objects/"+pr.Root, nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[2:6]) {
		t.Fatalf("range: %d %d bytes", resp.StatusCode, len(body))
	}
}

func TestStatusEndpoint(t *testing.T) {
	e := newEnv(t, Config{})
	content := randBytes(t, 64)
	_, pr := e.putMultipart(t, content, "a", "")

	var st1 struct {
		Root   string `json:"root"`
		Status string `json:"status"`
		TxHash string `json:"txHash"`
	}
	resp, _ := http.Get(e.ts.URL + "/objects/" + pr.Root + "/status")
	json.NewDecoder(resp.Body).Decode(&st1)
	resp.Body.Close()
	if st1.Status != "pending" || st1.Root != pr.Root {
		t.Fatalf("status: %+v", st1)
	}

	e.st.SetStatus(pr.Root, store.StatusOnchain, "0xdeadbeef", "")
	resp, _ = http.Get(e.ts.URL + "/objects/" + pr.Root + "/status")
	json.NewDecoder(resp.Body).Decode(&st1)
	resp.Body.Close()
	if st1.Status != "onchain" || st1.TxHash != "0xdeadbeef" {
		t.Fatalf("status after onchain: %+v", st1)
	}

	resp, _ = http.Get(e.ts.URL + "/objects/0x0000000000000000000000000000000000000000000000000000000000000000/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown status: %d", resp.StatusCode)
	}
}

func TestAuthToken(t *testing.T) {
	secret := "shh"
	e := newEnv(t, Config{AuthSecret: secret})
	content := randBytes(t, 64)
	_, pr := e.putMultipart(t, content, "a", "")

	// no token
	resp, _ := http.Get(e.ts.URL + "/objects/" + pr.Root)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	// valid token
	exp := time.Now().Add(time.Minute)
	tok := auth.Sign(secret, pr.Root, exp)
	url := fmt.Sprintf("%s/objects/%s?e=%d&t=%s", e.ts.URL, pr.Root, exp.Unix(), tok)
	resp, _ = http.Get(url)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("valid token: %d", resp.StatusCode)
	}

	// expired token
	old := time.Now().Add(-time.Minute)
	url = fmt.Sprintf("%s/objects/%s?e=%d&t=%s", e.ts.URL, pr.Root, old.Unix(), auth.Sign(secret, pr.Root, old))
	resp, _ = http.Get(url)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", resp.StatusCode)
	}

	// status endpoint stays open for the backend
	resp, _ = http.Get(e.ts.URL + "/objects/" + pr.Root + "/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with auth on: %d", resp.StatusCode)
	}
}

func TestDeleteLifecycle(t *testing.T) {
	e := newEnv(t, Config{AdminSecret: "admin"})
	content := randBytes(t, 64)
	_, pr := e.putMultipart(t, content, "a", "")

	req, _ := http.NewRequest(http.MethodDelete, e.ts.URL+"/objects/"+pr.Root, nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delete without admin token: %d", resp.StatusCode)
	}

	req.Header.Set("X-Admin-Token", "admin")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	get, _ := http.Get(e.ts.URL + "/objects/" + pr.Root)
	get.Body.Close()
	if get.StatusCode != http.StatusGone {
		t.Fatalf("get deleted: %d", get.StatusCode)
	}
}

func TestPutErrors(t *testing.T) {
	e := newEnv(t, Config{})

	resp, _ := e.putMultipart(t, nil, "empty", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty: %d", resp.StatusCode)
	}

	resp, _ = e.putMultipart(t, randBytes(t, (1<<20)+1), "big", "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", resp.StatusCode)
	}

	get, _ := http.Get(e.ts.URL + "/objects/0x0000000000000000000000000000000000000000000000000000000000000001")
	get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown get: %d", get.StatusCode)
	}
}

func TestPutDedupFlag(t *testing.T) {
	e := newEnv(t, Config{})
	content := randBytes(t, 64)
	_, first := e.putMultipart(t, content, "a", "")
	if first.Dedup {
		t.Fatalf("first put marked dedup: %+v", first)
	}
	_, second := e.putMultipart(t, content, "b", "")
	if !second.Dedup || second.Root != first.Root {
		t.Fatalf("second put: %+v", second)
	}
}
