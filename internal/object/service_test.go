package object

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0gfoundation/0g-storage-client/core"

	"zgs-gateway/internal/store"
)

type fakeDL struct {
	data  map[string][]byte
	calls int
}

func (f *fakeDL) Download(_ context.Context, root, dest string) error {
	f.calls++
	b, ok := f.data[root]
	if !ok {
		return fmt.Errorf("not on 0g: %s", root)
	}
	return os.WriteFile(dest, b, 0o644)
}

func newSvc(t *testing.T, dl Downloader, maxSize int64) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := New(st, dl, Config{DataDir: t.TempDir(), MaxSize: maxSize})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// sdkRoot computes the expected merkle root of content via the SDK itself.
func sdkRoot(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "expected")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := core.MerkleRoot(p)
	if err != nil {
		t.Fatal(err)
	}
	return h.Hex()
}

func TestPutComputesMerkleRootAndPersists(t *testing.T) {
	content := randBytes(t, 3000)
	svc, _ := newSvc(t, &fakeDL{}, 0)

	m, dedup, err := svc.Put(context.Background(), bytes.NewReader(content), "a.bin", "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if dedup {
		t.Fatal("first put flagged dedup")
	}
	if want := sdkRoot(t, content); m.Root != want {
		t.Fatalf("root mismatch: got %s want %s", m.Root, want)
	}
	if m.Status != store.StatusPending || m.Size != int64(len(content)) {
		t.Fatalf("meta: %+v", m)
	}
	sum := sha256.Sum256(content)
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha mismatch: %s", m.SHA256)
	}
	cached, err := os.ReadFile(svc.CachePath(m.Root))
	if err != nil || !bytes.Equal(cached, content) {
		t.Fatalf("cache file wrong: err=%v len=%d", err, len(cached))
	}
}

func TestPutEmptyRejected(t *testing.T) {
	svc, _ := newSvc(t, &fakeDL{}, 0)
	if _, _, err := svc.Put(context.Background(), bytes.NewReader(nil), "e", ""); err != ErrEmpty {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestPutTooLargeRejectedAndSpoolCleaned(t *testing.T) {
	svc, _ := newSvc(t, &fakeDL{}, 10)
	if _, _, err := svc.Put(context.Background(), bytes.NewReader(make([]byte, 11)), "big", ""); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// the spool area must not accumulate aborted uploads
	tmpDir := filepath.Join(filepath.Dir(filepath.Dir(svc.CachePath("0xdead"))), "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err == nil && len(entries) != 0 {
		t.Fatalf("spool not cleaned: %d files", len(entries))
	}
}

func TestPutDedup(t *testing.T) {
	content := randBytes(t, 500)
	svc, st := newSvc(t, &fakeDL{}, 0)

	m1, d1, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	m2, d2, err := svc.Put(context.Background(), bytes.NewReader(content), "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if m1.Root != m2.Root {
		t.Fatalf("dedup roots differ: %s %s", m1.Root, m2.Root)
	}
	if d1 || !d2 {
		t.Fatalf("dedup flags: first=%v second=%v", d1, d2)
	}
	q, _ := st.UploadQueue(10)
	if len(q) != 1 {
		t.Fatalf("dedup enqueued twice: %d", len(q))
	}

	// dedup must also return objects that already completed
	if err := st.SetStatus(m1.Root, store.StatusFinalized, "0xtx", ""); err != nil {
		t.Fatal(err)
	}
	m3, _, err := svc.Put(context.Background(), bytes.NewReader(content), "c", "")
	if err != nil {
		t.Fatal(err)
	}
	if m3.Status != store.StatusFinalized {
		t.Fatalf("dedup status: %+v", m3)
	}
}

func TestOpenCacheHit(t *testing.T) {
	content := randBytes(t, 256)
	dl := &fakeDL{}
	svc, _ := newSvc(t, dl, 0)
	m, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}

	f, meta, err := svc.Open(context.Background(), m.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, content) || meta.Root != m.Root {
		t.Fatalf("cache hit read wrong: %d bytes", len(got))
	}
	if dl.calls != 0 {
		t.Fatalf("downloader called on cache hit: %d", dl.calls)
	}
}

func TestOpenColdDownloadsFrom0G(t *testing.T) {
	content := randBytes(t, 256)
	dl := &fakeDL{data: map[string][]byte{}}
	svc, _ := newSvc(t, dl, 0)
	m, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	dl.data[m.Root] = content
	if err := os.Remove(svc.CachePath(m.Root)); err != nil {
		t.Fatal(err)
	}

	f, _, err := svc.Open(context.Background(), m.Root)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("cold read wrong: %d bytes", len(got))
	}
	if dl.calls != 1 {
		t.Fatalf("download calls: %d", dl.calls)
	}

	// cache must be repopulated: second open is a hit
	f2, _, err := svc.Open(context.Background(), m.Root)
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if dl.calls != 1 {
		t.Fatalf("cache not repopulated, calls=%d", dl.calls)
	}
}

func TestPutResurrectsDeletedContent(t *testing.T) {
	content := randBytes(t, 300)
	svc, st := newSvc(t, &fakeDL{}, 0)

	m1, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDeleted(m1.Root); err != nil {
		t.Fatal(err)
	}

	// re-uploading identical content must bring the object back, not hand back
	// a deleted record that still serves 410 Gone
	m2, dedup, err := svc.Put(context.Background(), bytes.NewReader(content), "a-again", "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Root != m1.Root {
		t.Fatalf("resurrect root differs: %s vs %s", m2.Root, m1.Root)
	}
	if dedup {
		t.Fatal("resurrect reported as a dedup hit")
	}
	if got, _, _ := st.Get(m1.Root); got.Deleted {
		t.Fatalf("still deleted after re-upload: %+v", got)
	}
	f, _, err := svc.Open(context.Background(), m1.Root)
	if err != nil {
		t.Fatalf("open after resurrect: %v", err)
	}
	f.Close()
	if q, _ := st.UploadQueue(10); len(q) != 1 {
		t.Fatalf("resurrected object not enqueued for upload: %d", len(q))
	}
}

func TestPutSalvagesMissingCacheOnDedup(t *testing.T) {
	content := randBytes(t, 400)
	svc, st := newSvc(t, &fakeDL{}, 0)

	m1, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	// the cache file is lost while the object is still pending (only copy)
	if err := os.Remove(svc.CachePath(m1.Root)); err != nil {
		t.Fatal(err)
	}

	// a re-upload of identical content must rebuild the cache file from the new
	// body rather than discard it and leave a queued upload that can't open it
	m2, dedup, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if !dedup || m2.Root != m1.Root {
		t.Fatalf("dedup salvage: dedup=%v root=%s", dedup, m2.Root)
	}
	cached, err := os.ReadFile(svc.CachePath(m1.Root))
	if err != nil || !bytes.Equal(cached, content) {
		t.Fatalf("cache not salvaged: err=%v", err)
	}
	if q, _ := st.UploadQueue(10); len(q) != 1 {
		t.Fatalf("salvaged object not enqueued: %d", len(q))
	}
}

func TestOpenRejectsCorruptCacheAndRestores(t *testing.T) {
	content := randBytes(t, 512)
	dl := &fakeDL{data: map[string][]byte{}}
	svc, st := newSvc(t, dl, 0)
	m, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	// finalized on 0G ⇒ a proof-verified restore is possible
	if err := st.SetStatus(m.Root, store.StatusFinalized, "0xtx", ""); err != nil {
		t.Fatal(err)
	}
	dl.data[m.Root] = content
	// truncate the cache file so its size no longer matches the metadata
	if err := os.WriteFile(svc.CachePath(m.Root), content[:100], 0o644); err != nil {
		t.Fatal(err)
	}

	f, _, err := svc.Open(context.Background(), m.Root)
	if err != nil {
		t.Fatalf("open with corrupt cache: %v", err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("served corrupt bytes: %d", len(got))
	}
	if dl.calls != 1 {
		t.Fatalf("corrupt cache not restored from 0g: calls=%d", dl.calls)
	}
}

func TestOpenCorruptCacheNotOn0GFailsClosed(t *testing.T) {
	content := randBytes(t, 512)
	dl := &fakeDL{data: map[string][]byte{}} // nothing on 0G
	svc, _ := newSvc(t, dl, 0)
	m, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.CachePath(m.Root), content[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	// corrupt local copy and nothing to restore ⇒ must fail rather than serve
	// bytes that do not match the root
	if _, _, err := svc.Open(context.Background(), m.Root); err == nil {
		t.Fatal("served corrupt bytes instead of failing closed")
	}
}

func TestOpenUnknownAndDeleted(t *testing.T) {
	svc, st := newSvc(t, &fakeDL{}, 0)
	if _, _, err := svc.Open(context.Background(), "0xnope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	content := randBytes(t, 64)
	m, _, err := svc.Put(context.Background(), bytes.NewReader(content), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDeleted(m.Root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Open(context.Background(), m.Root); err != ErrGone {
		t.Fatalf("want ErrGone, got %v", err)
	}
}
