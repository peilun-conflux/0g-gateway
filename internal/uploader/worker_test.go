package uploader

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"zgs-gateway/internal/store"
)

type fakeChain struct {
	mu          sync.Mutex
	batches     [][]Item
	uploadErr   error
	status      map[string]FileStatus
	statusCalls map[string]int
	txHash      string
}

func newFakeChain() *fakeChain {
	return &fakeChain{status: map[string]FileStatus{}, statusCalls: map[string]int{}, txHash: "0xtx1"}
}

func (f *fakeChain) BatchUpload(_ context.Context, items []Item) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]Item, len(items))
	copy(cp, items)
	f.batches = append(f.batches, cp)
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	return f.txHash, nil
}

func (f *fakeChain) FileStatus(_ context.Context, root string) (FileStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls[root]++
	return f.status[root], nil
}

func (f *fakeChain) totalStatusCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.statusCalls {
		n += c
	}
	return n
}

func setup(t *testing.T, ch Chain, cfg Config) (*Worker, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if cfg.PathOf == nil {
		cfg.PathOf = func(root string) string { return "/dev/null/" + root }
	}
	return New(st, ch, cfg), st
}

func addPending(t *testing.T, st *store.Store, n int) []string {
	t.Helper()
	roots := make([]string, n)
	for i := range roots {
		roots[i] = fmt.Sprintf("0x%02d", i+1)
		if err := st.CreateObject(store.ObjectMeta{Root: roots[i], SHA256: fmt.Sprintf("s%d", i), Status: store.StatusPending}); err != nil {
			t.Fatal(err)
		}
	}
	return roots
}

func TestFlushHappyPath(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 2)

	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ch.batches) != 1 || len(ch.batches[0]) != 2 {
		t.Fatalf("batches: %+v", ch.batches)
	}
	for _, it := range ch.batches[0] {
		if it.SkipTx {
			t.Fatalf("fresh item has SkipTx: %+v", it)
		}
	}
	for _, r := range roots {
		m, _, _ := st.Get(r)
		if m.Status != store.StatusOnchain || m.TxHash != "0xtx1" {
			t.Fatalf("after flush: %+v", m)
		}
	}
	// fresh pending items must not pay a reconcile RPC
	if ch.totalStatusCalls() != 0 {
		t.Fatalf("reconcile on happy path: %d calls", ch.totalStatusCalls())
	}
}

func TestSkipTxKeepsExistingTxHash(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 1)
	// the object is already on chain under its original tx, left in "submitted"
	// by a crash; reconcile will mark it SkipTx (segments-only)
	if err := st.SetStatus(roots[0], store.StatusSubmitted, "0xORIGINAL", ""); err != nil {
		t.Fatal(err)
	}
	ch.status[roots[0]] = FileUploading
	ch.txHash = "0xNEWBATCH"

	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, _, _ := st.Get(roots[0])
	if m.Status != store.StatusOnchain {
		t.Fatalf("status after flush: %+v", m)
	}
	if m.TxHash != "0xORIGINAL" {
		t.Fatalf("SkipTx item lost its original txHash: got %q want 0xORIGINAL", m.TxHash)
	}
}

func TestFlushRespectsBatchMax(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 2, MaxRetries: 3})
	addPending(t, st, 5)

	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ch.batches) != 3 {
		t.Fatalf("want 3 batches, got %d", len(ch.batches))
	}
	if len(ch.batches[0]) != 2 || len(ch.batches[1]) != 2 || len(ch.batches[2]) != 1 {
		t.Fatalf("batch sizes: %d %d %d", len(ch.batches[0]), len(ch.batches[1]), len(ch.batches[2]))
	}
}

func TestFlushFailureReconciles(t *testing.T) {
	ch := newFakeChain()
	ch.uploadErr = errors.New("rpc down")
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 5})
	roots := addPending(t, st, 3)       // A, B, C
	ch.status[roots[0]] = FileUploading // A: tx landed, segments incomplete
	ch.status[roots[1]] = FileUnknown   // B: tx never landed
	ch.status[roots[2]] = FileFinalized // C: actually done

	if err := w.Flush(context.Background()); err == nil {
		t.Fatal("flush should surface the batch error")
	}

	a, _, _ := st.Get(roots[0])
	if a.Status != store.StatusPending || !a.SkipTx || a.Retries != 1 {
		t.Fatalf("A after reconcile: %+v", a)
	}
	b, _, _ := st.Get(roots[1])
	if b.Status != store.StatusPending || b.SkipTx || b.Retries != 1 {
		t.Fatalf("B after reconcile: %+v", b)
	}
	c, _, _ := st.Get(roots[2])
	if c.Status != store.StatusFinalized {
		t.Fatalf("C after reconcile: %+v", c)
	}

	// next round: A retried with SkipTx, B with a fresh tx, C gone
	ch.uploadErr = nil
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := ch.batches[len(ch.batches)-1]
	if len(last) != 2 {
		t.Fatalf("retry batch: %+v", last)
	}
	for _, it := range last {
		switch it.Root {
		case roots[0]:
			if !it.SkipTx {
				t.Fatal("A lost SkipTx")
			}
		case roots[1]:
			if it.SkipTx {
				t.Fatal("B gained SkipTx")
			}
		default:
			t.Fatalf("unexpected item %+v", it)
		}
	}
}

func TestRetryCapMarksFailed(t *testing.T) {
	ch := newFakeChain()
	ch.uploadErr = errors.New("rpc down")
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 2})
	roots := addPending(t, st, 1)

	for i := 0; i < 3; i++ {
		_ = w.Flush(context.Background())
	}
	m, _, _ := st.Get(roots[0])
	if m.Status != store.StatusFailed {
		t.Fatalf("not failed after cap: %+v", m)
	}
	if m.FailReason == "" {
		t.Fatal("fail reason empty")
	}
}

func TestPollFinality(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 2)
	for _, r := range roots {
		if err := st.SetStatus(r, store.StatusOnchain, "0xtx", ""); err != nil {
			t.Fatal(err)
		}
	}
	ch.status[roots[0]] = FileFinalized
	ch.status[roots[1]] = FileUploading

	if err := w.PollFinality(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _, _ := st.Get(roots[0])
	if a.Status != store.StatusFinalized {
		t.Fatalf("A: %+v", a)
	}
	b, _, _ := st.Get(roots[1])
	if b.Status != store.StatusOnchain {
		t.Fatalf("B should stay onchain: %+v", b)
	}
}

func TestPollFinalityPrunedFails(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 1)
	if err := st.SetStatus(roots[0], store.StatusOnchain, "0xtx", ""); err != nil {
		t.Fatal(err)
	}
	ch.status[roots[0]] = FilePruned

	if err := w.PollFinality(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, _, _ := st.Get(roots[0])
	if m.Status != store.StatusFailed || m.FailReason != "pruned" {
		t.Fatalf("pruned object: %+v", m)
	}
}

// A crash between BatchUpload and SetStatus leaves objects in "submitted";
// the next Flush must reconcile them instead of blindly re-submitting.
func TestRecoverSubmittedReconcilesFirst(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 1)
	if err := st.SetStatus(roots[0], store.StatusSubmitted, "", ""); err != nil {
		t.Fatal(err)
	}
	ch.status[roots[0]] = FileUploading

	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ch.statusCalls[roots[0]] == 0 {
		t.Fatal("submitted leftover not reconciled")
	}
	last := ch.batches[len(ch.batches)-1]
	if len(last) != 1 || !last[0].SkipTx {
		t.Fatalf("recovered item should re-upload with SkipTx: %+v", last)
	}
	m, _, _ := st.Get(roots[0])
	if m.Status != store.StatusOnchain {
		t.Fatalf("after recovery flush: %+v", m)
	}
}

// A submitted leftover whose batch actually finalized must be closed out
// without another upload.
func TestRecoverSubmittedAlreadyFinalized(t *testing.T) {
	ch := newFakeChain()
	w, st := setup(t, ch, Config{BatchMax: 10, MaxRetries: 3})
	roots := addPending(t, st, 1)
	if err := st.SetStatus(roots[0], store.StatusSubmitted, "", ""); err != nil {
		t.Fatal(err)
	}
	ch.status[roots[0]] = FileFinalized

	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ch.batches) != 0 {
		t.Fatalf("finalized leftover re-uploaded: %+v", ch.batches)
	}
	m, _, _ := st.Get(roots[0])
	if m.Status != store.StatusFinalized {
		t.Fatalf("after recovery: %+v", m)
	}
}
