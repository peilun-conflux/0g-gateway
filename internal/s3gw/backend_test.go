package s3gw

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/object"
	"zgs-gateway/internal/store"
)

type fakeDL struct{ data map[string][]byte }

func (f *fakeDL) Download(_ context.Context, root, dest string) error {
	b, ok := f.data[root]
	if !ok {
		return os.ErrNotExist
	}
	return os.WriteFile(dest, b, 0o644)
}

func newBackend(t *testing.T) (*Backend, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := object.New(st, &fakeDL{data: map[string][]byte{}}, object.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return New(context.Background(), svc, st), st
}

func put(t *testing.T, b *Backend, bucket, key string, content []byte, ctype string) {
	t.Helper()
	meta := map[string]string{}
	if ctype != "" {
		meta["Content-Type"] = ctype
	}
	if _, err := b.PutObject(bucket, key, meta, bytes.NewReader(content), int64(len(content)), nil); err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

func TestBucketLifecycle(t *testing.T) {
	b, _ := newBackend(t)

	if err := b.CreateBucket("demo"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket("demo"); err == nil {
		t.Fatal("duplicate bucket accepted")
	}
	if ok, _ := b.BucketExists("demo"); !ok {
		t.Fatal("bucket should exist")
	}
	if list, _ := b.ListBuckets(); len(list) != 1 || list[0].Name != "demo" {
		t.Fatalf("list buckets: %+v", list)
	}

	// non-empty delete is refused; force delete works
	put(t, b, "demo", "a.txt", []byte("hi"), "text/plain")
	if err := b.DeleteBucket("demo"); err == nil {
		t.Fatal("deleted a non-empty bucket")
	}
	if err := b.ForceDeleteBucket("demo"); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if ok, _ := b.BucketExists("demo"); ok {
		t.Fatal("bucket should be gone")
	}
}

func TestPutGetHeadDelete(t *testing.T) {
	b, _ := newBackend(t)
	content := []byte("the quick brown fox")
	sum := md5.Sum(content)

	if err := b.CreateBucket("demo"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "demo", "docs/fox.txt", content, "text/plain")

	// GET returns bytes + correct MD5 (ETag) + content type
	obj, err := b.GetObject("demo", "docs/fox.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("get bytes: %q", got)
	}
	if hex.EncodeToString(obj.Hash) != hex.EncodeToString(sum[:]) {
		t.Fatalf("etag/md5 mismatch: %x", obj.Hash)
	}
	if obj.Metadata["Content-Type"] != "text/plain" {
		t.Fatalf("content-type: %q", obj.Metadata["Content-Type"])
	}

	// HEAD: metadata only, no body
	head, err := b.HeadObject("demo", "docs/fox.txt")
	if err != nil {
		t.Fatal(err)
	}
	if head.Size != int64(len(content)) {
		t.Fatalf("head size: %d", head.Size)
	}
	hb, _ := io.ReadAll(head.Contents)
	head.Contents.Close()
	if len(hb) != 0 {
		t.Fatalf("head returned a body: %d bytes", len(hb))
	}

	// DELETE then GET → not found
	if _, err := b.DeleteObject("demo", "docs/fox.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetObject("demo", "docs/fox.txt", nil); err == nil {
		t.Fatal("get after delete should fail")
	}
	// deleting an absent key is not an error (S3 semantics)
	if _, err := b.DeleteObject("demo", "docs/fox.txt"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

func TestGetRange(t *testing.T) {
	b, _ := newBackend(t)
	content := []byte("0123456789")
	if err := b.CreateBucket("demo"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "demo", "nums", content, "")

	obj, err := b.GetObject("demo", "nums", &gofakes3.ObjectRangeRequest{Start: 2, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if string(got) != "2345" {
		t.Fatalf("range bytes: %q", got)
	}
	if obj.Range == nil || obj.Range.Start != 2 || obj.Range.Length != 4 {
		t.Fatalf("range meta: %+v", obj.Range)
	}
	if obj.Size != int64(len(content)) {
		t.Fatalf("range obj.Size should be full size: %d", obj.Size)
	}
}

func TestListBucketPrefix(t *testing.T) {
	b, _ := newBackend(t)
	if err := b.CreateBucket("demo"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "demo", "a/1.txt", []byte("a1"), "")
	put(t, b, "demo", "a/2.txt", []byte("a2"), "")
	put(t, b, "demo", "b/1.txt", []byte("b1"), "")

	// prefix "a/" lists both a/ objects
	list, err := b.ListBucket("demo", &gofakes3.Prefix{HasPrefix: true, Prefix: "a/"}, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Contents) != 2 {
		t.Fatalf("prefix a/ contents: %d (%+v)", len(list.Contents), list.Contents)
	}

	// no prefix lists all three
	all, err := b.ListBucket("demo", nil, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Contents) != 3 {
		t.Fatalf("all contents: %d", len(all.Contents))
	}
}

func TestHTTPRoundTripWithAutoBucket(t *testing.T) {
	b, _ := newBackend(t)
	faker := gofakes3.New(b, gofakes3.WithAutoBucket(true))
	ts := httptest.NewServer(faker.Server())
	defer ts.Close()

	content := []byte("hello via the s3 wire protocol")

	// PUT to a not-yet-existing bucket: WithAutoBucket creates it
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/demo/hello.txt", bytes.NewReader(content))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status: %d", resp.StatusCode)
	}
	sum := md5.Sum(content)
	if etag := resp.Header.Get("ETag"); etag != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("PUT ETag: %s", etag)
	}

	// GET it back
	get, err := http.Get(ts.URL + "/demo/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("GET: %d, %d bytes", get.StatusCode, len(body))
	}
	if ct := get.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("GET Content-Type round-trip: %q", ct)
	}
}

func TestCopyObjectIsZeroCopy(t *testing.T) {
	b, st := newBackend(t)
	if err := b.CreateBucket("src"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket("dst"); err != nil {
		t.Fatal(err)
	}
	content := []byte("copy me please")
	put(t, b, "src", "a.txt", content, "text/plain")

	if _, err := b.CopyObject("src", "a.txt", "dst", "b.txt", nil); err != nil {
		t.Fatal(err)
	}
	obj, err := b.GetObject("dst", "b.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("copied bytes: %q", got)
	}
	// identical bytes ⇒ identical root ⇒ the copy is a pure re-map, not a re-upload
	srcRoot, _, _ := st.S3GetObjectKey("src", "a.txt")
	dstRoot, _, _ := st.S3GetObjectKey("dst", "b.txt")
	if srcRoot == "" || srcRoot != dstRoot {
		t.Fatalf("copy not zero-copy: src=%q dst=%q", srcRoot, dstRoot)
	}
}

func TestConditionalPut(t *testing.T) {
	b, _ := newBackend(t)
	if err := b.CreateBucket("c"); err != nil {
		t.Fatal(err)
	}
	star := "*"
	v1 := []byte("v1")

	// If-None-Match:* on a new key → allowed (atomic create)
	if _, err := b.PutObject("c", "k", map[string]string{"Content-Type": "text/plain"},
		bytes.NewReader(v1), int64(len(v1)), &gofakes3.PutConditions{IfNoneMatch: &star}); err != nil {
		t.Fatalf("create-if-absent on new key: %v", err)
	}
	// If-None-Match:* on an existing key → rejected
	if _, err := b.PutObject("c", "k", nil, bytes.NewReader(v1), int64(len(v1)),
		&gofakes3.PutConditions{IfNoneMatch: &star}); err == nil {
		t.Fatal("create-if-absent on existing key should fail")
	}
	// If-Match with the current ETag → allowed
	sum := md5.Sum(v1)
	etag := hex.EncodeToString(sum[:])
	if _, err := b.PutObject("c", "k", nil, bytes.NewReader([]byte("v2")), 2,
		&gofakes3.PutConditions{IfMatch: &etag}); err != nil {
		t.Fatalf("if-match with correct etag: %v", err)
	}
	// If-Match with a stale ETag → rejected
	stale := "00000000000000000000000000000000"
	if _, err := b.PutObject("c", "k", nil, bytes.NewReader([]byte("v3")), 2,
		&gofakes3.PutConditions{IfMatch: &stale}); err == nil {
		t.Fatal("if-match with wrong etag should fail")
	}
}

func TestImageProcessResizeOverS3(t *testing.T) {
	b, _ := newBackend(t)
	faker := gofakes3.New(b, gofakes3.WithAutoBucket(true))
	ts := httptest.NewServer(b.ImageProcessHandler(faker.Server()))
	defer ts.Close()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 100, 50))); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/pics/a.png", bytes.NewReader(src))
	req.Header.Set("Content-Type", "image/png")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put: %d", resp.StatusCode)
		}
	}

	dims := func(t *testing.T, spec string) (int, int, string) {
		t.Helper()
		u := ts.URL + "/pics/a.png?x-image-process=" + url.QueryEscape(spec)
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("resize get %q: %d", spec, resp.StatusCode)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("decode resized %q: %v", spec, err)
		}
		return cfg.Width, cfg.Height, resp.Header.Get("Content-Type")
	}

	// lfit (default): 100x50 into w=20 → 20x10, content-type image/png
	if w, h, ct := dims(t, "image/resize,w_20"); w != 20 || h != 10 || ct != "image/png" {
		t.Fatalf("lfit resize: %dx%d ct=%q", w, h, ct)
	}
	// fill to exact 30x30
	if w, h, _ := dims(t, "image/resize,w_30,h_30,m_fill"); w != 30 || h != 30 {
		t.Fatalf("fill resize: %dx%d", w, h)
	}

	// passthrough: no process param → original bytes, unchanged
	orig, _ := http.Get(ts.URL + "/pics/a.png")
	ob, _ := io.ReadAll(orig.Body)
	orig.Body.Close()
	if !bytes.Equal(ob, src) {
		t.Fatalf("passthrough altered bytes: %d vs %d", len(ob), len(src))
	}
}

func TestOverwriteRepointsToNewRoot(t *testing.T) {
	b, st := newBackend(t)
	if err := b.CreateBucket("c"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "c", "k", []byte("first"), "")
	r1, _, _ := st.S3GetObjectKey("c", "k")
	put(t, b, "c", "k", []byte("second"), "")
	r2, _, _ := st.S3GetObjectKey("c", "k")
	if r1 == "" || r1 == r2 {
		t.Fatalf("overwrite should repoint to a new root: %q vs %q", r1, r2)
	}
	obj, err := b.GetObject("c", "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if string(got) != "second" {
		t.Fatalf("overwrite content: %q", got)
	}
}
