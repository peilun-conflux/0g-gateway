// Package uploader drains the upload queue in batches: one BatchUpload call
// submits many files in a single chain transaction (kills nonce contention,
// amortizes gas). On failure it reconciles each member against the storage
// nodes before retrying, because a failed batch returns no per-file result
// (SDK: uploader_batch.go returns (txHash, nil, err)).
package uploader

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"zgs-gateway/internal/store"
)

// FileStatus is the gateway's view of one root on the storage nodes.
type FileStatus int

const (
	FileUnknown   FileStatus = iota // no node knows the root (tx not landed)
	FileUploading                   // entry exists on chain, not yet finalized
	FileFinalized                   // finalized on the nodes
	FilePruned                      // pruned — data is gone from the nodes
)

// Item is one member of an upload batch.
type Item struct {
	Root   string
	Path   string // local cache file
	SkipTx bool   // entry already on chain; only re-upload segments
}

// Chain is the minimal surface the worker needs from the 0G backend.
type Chain interface {
	// BatchUpload submits all items in one chain tx and uploads their
	// segments. It either fully succeeds or returns an error with no
	// per-item result (SDK behavior), hence the reconcile pass.
	BatchUpload(ctx context.Context, items []Item) (txHash string, err error)
	FileStatus(ctx context.Context, root string) (FileStatus, error)
}

type Config struct {
	BatchMax   int                      // max files per chain tx
	MaxRetries int                      // attempts before an object is marked failed
	PathOf     func(root string) string // cache path resolver
}

type Worker struct {
	st  *store.Store
	ch  Chain
	cfg Config
}

func New(st *store.Store, ch Chain, cfg Config) *Worker {
	if cfg.BatchMax <= 0 {
		cfg.BatchMax = 10
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	return &Worker{st: st, ch: ch, cfg: cfg}
}

// Flush drains the upload queue once, in batches of at most BatchMax. The
// queue is snapshotted up front so members of a failed batch are not retried
// within the same flush.
func (w *Worker) Flush(ctx context.Context) error {
	queue, err := w.st.UploadQueue(0)
	if err != nil {
		return err
	}
	var firstErr error
	for start := 0; start < len(queue); start += w.cfg.BatchMax {
		end := min(start+w.cfg.BatchMax, len(queue))
		if err := w.processBatch(ctx, queue[start:end]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (w *Worker) processBatch(ctx context.Context, batch []store.ObjectMeta) error {
	// Reconcile crash leftovers (submitted) and retried items against the
	// nodes before re-submitting; fresh pending items skip the extra RPC.
	items := make([]Item, 0, len(batch))
	var deferred error
	for _, m := range batch {
		// The queue was snapshotted up front, so re-read each object to pick up
		// any state change a concurrent PUT landed since (e.g. a salvage that
		// reset the object to pending and rearmed its upload).
		cur, ok, err := w.st.Get(m.Root)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		m = cur
		if m.Status == store.StatusSubmitted || m.Retries > 0 {
			st, err := w.ch.FileStatus(ctx, m.Root)
			if err != nil {
				// We can't tell whether this item's prior tx already landed on
				// chain. Re-submitting now could duplicate it (double upload /
				// wasted gas), so defer it to a later flush rather than uploading
				// blind. Its status is left unchanged, so it stays queued.
				if deferred == nil {
					deferred = fmt.Errorf("reconcile %s: %w", m.Root, err)
				}
				continue
			}
			switch st {
			case FileFinalized:
				if err := w.st.SetStatus(m.Root, store.StatusFinalized, "", ""); err != nil {
					return err
				}
				continue
			case FileUploading:
				m.SkipTx = true
				_ = w.st.SetSkipTx(m.Root, true)
			case FileUnknown, FilePruned:
				// entry not (or no longer) on chain: needs a fresh tx
				m.SkipTx = false
				_ = w.st.SetSkipTx(m.Root, false)
			}
		}
		items = append(items, Item{Root: m.Root, Path: w.cfg.PathOf(m.Root), SkipTx: m.SkipTx})
	}
	if len(items) == 0 {
		return deferred
	}

	for _, it := range items {
		if err := w.st.SetStatus(it.Root, store.StatusSubmitted, "", ""); err != nil {
			return err
		}
	}
	txHash, err := w.ch.BatchUpload(ctx, items)
	if err != nil {
		w.reconcileFailedBatch(ctx, items, err)
		return err
	}
	for _, it := range items {
		// SkipTx items were already on chain under their original tx; this
		// batch only re-uploaded their segments, so keep their recorded txHash
		// (an empty hash tells SetStatus to leave it untouched) instead of
		// stamping this batch's hash — which may even be the zero hash for an
		// all-SkipTx batch that submitted no new transaction.
		th := txHash
		if it.SkipTx {
			th = ""
		}
		if err := w.st.SetStatus(it.Root, store.StatusOnchain, th, ""); err != nil {
			return err
		}
	}
	return deferred
}

// reconcileFailedBatch classifies every member of a failed batch: some may
// have made it on chain (retry segments only via SkipTx), some may even be
// finalized; the rest go back to pending until the retry cap.
func (w *Worker) reconcileFailedBatch(ctx context.Context, items []Item, batchErr error) {
	for _, it := range items {
		st, err := w.ch.FileStatus(ctx, it.Root)
		if err == nil && st == FileFinalized {
			_ = w.st.SetStatus(it.Root, store.StatusFinalized, "", "")
			continue
		}
		if err == nil && st == FileUploading {
			_ = w.st.SetSkipTx(it.Root, true)
		}
		n, rerr := w.st.IncRetries(it.Root)
		if rerr != nil {
			slog.Warn("inc retries", "root", it.Root, "err", rerr)
			continue
		}
		if n > w.cfg.MaxRetries {
			_ = w.st.SetStatus(it.Root, store.StatusFailed, "", fmt.Sprintf("retries exhausted: %v", batchErr))
		} else {
			_ = w.st.SetStatus(it.Root, store.StatusPending, "", "")
		}
	}
}

// PollFinality advances onchain objects to finalized (or failed when pruned —
// that is an alert-worthy condition for an archival deployment).
func (w *Worker) PollFinality(ctx context.Context) error {
	queue, err := w.st.FinalizeQueue(0)
	if err != nil {
		return err
	}
	var firstErr error
	for _, m := range queue {
		st, err := w.ch.FileStatus(ctx, m.Root)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch st {
		case FileFinalized:
			if err := w.st.SetStatus(m.Root, store.StatusFinalized, "", ""); err != nil && firstErr == nil {
				firstErr = err
			}
		case FilePruned:
			slog.Error("object pruned on storage nodes", "root", m.Root)
			if err := w.st.SetStatus(m.Root, store.StatusFailed, "", "pruned"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Run loops Flush and PollFinality until ctx is canceled.
//
// There is deliberately no panic recovery. An unrecovered panic in this
// goroutine aborts the whole process (Go crashes on any unrecovered goroutine
// panic) — the intended fail-fast for the demo, so a worker bug surfaces loudly
// with a stack trace instead of being swallowed while uploads silently stop.
// For production, wrap the loop body in recover()+log+continue so a transient
// panic doesn't take the gateway down.
//
// Neither fail-fast nor recovery catches a *hang*: if Flush blocks forever (a
// stuck node RPC with no per-op timeout) the goroutine never panics or returns,
// the process stays up, and uploads stall silently. Guarding against that needs
// a per-operation timeout or a liveness/status signal, not panic handling.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Flush(ctx); err != nil {
				slog.Warn("upload flush", "err", err)
			}
			if err := w.PollFinality(ctx); err != nil {
				slog.Warn("finality poll", "err", err)
			}
		}
	}
}
