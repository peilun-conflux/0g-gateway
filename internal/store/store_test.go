package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, m ObjectMeta) {
	t.Helper()
	if err := s.CreateObject(m); err != nil {
		t.Fatalf("create %s: %v", m.Root, err)
	}
}

func TestCreateGet(t *testing.T) {
	s := tempStore(t)
	m := ObjectMeta{Root: "0x01", SHA256: "aa", Size: 3, Filename: "a.txt", ContentType: "text/plain", Status: StatusPending}
	mustCreate(t, s, m)

	got, ok, err := s.Get("0x01")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.SHA256 != "aa" || got.Status != StatusPending || got.Filename != "a.txt" {
		t.Fatalf("bad meta: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", got)
	}

	if err := s.CreateObject(m); err == nil {
		t.Fatal("duplicate root accepted")
	}

	_, ok, err = s.Get("0xnope")
	if err != nil || ok {
		t.Fatalf("missing object: ok=%v err=%v", ok, err)
	}
}

func TestBySHA256(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "abcd", Status: StatusPending})

	got, ok, err := s.BySHA256("abcd")
	if err != nil || !ok || got.Root != "0x01" {
		t.Fatalf("by sha: %+v ok=%v err=%v", got, ok, err)
	}
	_, ok, _ = s.BySHA256("ffff")
	if ok {
		t.Fatal("unknown sha found")
	}
}

func TestQueuesAndTransitions(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})
	mustCreate(t, s, ObjectMeta{Root: "0x02", SHA256: "b", Status: StatusPending})

	q, err := s.UploadQueue(10)
	if err != nil || len(q) != 2 {
		t.Fatalf("upload queue: %d err=%v", len(q), err)
	}

	// pending → submitted stays in the upload queue (crash recovery scans it)
	if err := s.SetStatus("0x01", StatusSubmitted, "", ""); err != nil {
		t.Fatal(err)
	}
	if q, _ = s.UploadQueue(10); len(q) != 2 {
		t.Fatalf("submitted dropped from upload queue: %d", len(q))
	}

	// submitted → onchain moves to the finalize queue, records txHash
	if err := s.SetStatus("0x01", StatusOnchain, "0xtx", ""); err != nil {
		t.Fatal(err)
	}
	if q, _ = s.UploadQueue(10); len(q) != 1 {
		t.Fatalf("onchain still in upload queue: %d", len(q))
	}
	f, _ := s.FinalizeQueue(10)
	if len(f) != 1 || f[0].Root != "0x01" || f[0].TxHash != "0xtx" {
		t.Fatalf("finalize queue: %+v", f)
	}

	// onchain → finalized leaves all queues; empty txHash must not erase it
	if err := s.SetStatus("0x01", StatusFinalized, "", ""); err != nil {
		t.Fatal(err)
	}
	if f, _ = s.FinalizeQueue(10); len(f) != 0 {
		t.Fatalf("finalized still in finalize queue: %d", len(f))
	}
	got, _, _ := s.Get("0x01")
	if got.Status != StatusFinalized || got.TxHash != "0xtx" {
		t.Fatalf("finalized meta: %+v", got)
	}

	// failed leaves the upload queue and records the reason
	if err := s.SetStatus("0x02", StatusFailed, "", "boom"); err != nil {
		t.Fatal(err)
	}
	if q, _ = s.UploadQueue(10); len(q) != 0 {
		t.Fatalf("failed still in upload queue: %d", len(q))
	}
	got, _, _ = s.Get("0x02")
	if got.Status != StatusFailed || got.FailReason != "boom" {
		t.Fatalf("failed meta: %+v", got)
	}
}

func TestUploadQueueLimit(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})
	mustCreate(t, s, ObjectMeta{Root: "0x02", SHA256: "b", Status: StatusPending})
	mustCreate(t, s, ObjectMeta{Root: "0x03", SHA256: "c", Status: StatusPending})
	q, _ := s.UploadQueue(2)
	if len(q) != 2 {
		t.Fatalf("limit ignored: %d", len(q))
	}
}

func TestRetriesAndSkipTx(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})

	if n, err := s.IncRetries("0x01"); err != nil || n != 1 {
		t.Fatalf("first inc: n=%d err=%v", n, err)
	}
	if n, _ := s.IncRetries("0x01"); n != 2 {
		t.Fatalf("second inc: n=%d", n)
	}
	if err := s.SetSkipTx("0x01", true); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("0x01")
	if got.Retries != 2 || !got.SkipTx {
		t.Fatalf("meta after retry/skiptx: %+v", got)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	q, _ := s2.UploadQueue(10)
	if len(q) != 1 || q[0].Root != "0x01" {
		t.Fatalf("queue lost after reopen: %+v", q)
	}
}

func TestMarkDeleted(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusFinalized})
	if err := s.MarkDeleted("0x01"); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Get("0x01")
	if !ok || !got.Deleted {
		t.Fatalf("not marked deleted: %+v ok=%v", got, ok)
	}
}

func TestMarkDeletedDropsFromQueues(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})
	mustCreate(t, s, ObjectMeta{Root: "0x02", SHA256: "b", Status: StatusPending})
	if err := s.SetStatus("0x02", StatusOnchain, "0xtx", ""); err != nil {
		t.Fatal(err)
	}

	// deleting a pending object must remove it from the upload queue so the
	// worker never uploads it to immutable 0G
	if err := s.MarkDeleted("0x01"); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.UploadQueue(10); len(q) != 0 {
		t.Fatalf("deleted pending object still in upload queue: %d", len(q))
	}
	// and an onchain object must leave the finalize queue
	if err := s.MarkDeleted("0x02"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.FinalizeQueue(10); len(f) != 0 {
		t.Fatalf("deleted onchain object still in finalize queue: %d", len(f))
	}
}

func TestUndeleteRequeues(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusPending})
	if err := s.MarkDeleted("0x01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Undelete("0x01"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("0x01")
	if got.Deleted || got.Status != StatusPending {
		t.Fatalf("undelete state: %+v", got)
	}
	if q, _ := s.UploadQueue(10); len(q) != 1 {
		t.Fatalf("undeleted object not re-enqueued: %d", len(q))
	}
}

func TestS3PutObjectKeyRequiresBucket(t *testing.T) {
	s := tempStore(t)
	// Writing a key under a non-existent bucket is refused in-transaction, so a
	// bucket delete racing a slow PUT can never leave an orphan key behind.
	if err := s.S3PutObjectKey("demo", "k", "0x01"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("want ErrBucketNotFound for missing bucket, got %v", err)
	}
	if err := s.S3CreateBucket("demo", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.S3PutObjectKey("demo", "k", "0x01"); err != nil {
		t.Fatalf("put after create: %v", err)
	}
	if root, ok, _ := s.S3GetObjectKey("demo", "k"); !ok || root != "0x01" {
		t.Fatalf("get after put: root=%q ok=%v", root, ok)
	}
}

func TestUndeleteFinalizedStaysFinalized(t *testing.T) {
	s := tempStore(t)
	mustCreate(t, s, ObjectMeta{Root: "0x01", SHA256: "a", Status: StatusFinalized})
	if err := s.MarkDeleted("0x01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Undelete("0x01"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("0x01")
	if got.Deleted || got.Status != StatusFinalized {
		t.Fatalf("finalized undelete must not reset status: %+v", got)
	}
	if q, _ := s.UploadQueue(10); len(q) != 0 {
		t.Fatalf("finalized object wrongly enqueued for upload: %d", len(q))
	}
}
