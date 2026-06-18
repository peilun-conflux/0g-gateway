// Package object implements the gateway's PUT/GET pipeline: spool → SHA256 →
// dedup → materialize cache file → merkle root → persist metadata + enqueue.
//
// Design note (encryption): if gateway-side encryption is ever enabled, it
// slots in as a single transform when materializing the cache file in Put
// (cache holds ciphertext, root is computed over ciphertext) and a single
// transform when serving in Open. No abstraction is pre-built for it on
// purpose; ObjectMeta is JSON so new fields are backward compatible.
package object

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0gfoundation/0g-storage-client/core"

	"zgs-gateway/internal/store"
)

var (
	ErrEmpty    = errors.New("empty object rejected (0G cannot address zero-byte files)")
	ErrTooLarge = errors.New("object exceeds the configured size limit")
	ErrNotFound = errors.New("object not found")
)

// Downloader restores an object from 0G into a local file (cold read path).
type Downloader interface {
	Download(ctx context.Context, root, dest string) error
}

type Config struct {
	DataDir       string // cache root; spool files live under DataDir/tmp
	MaxSize       int64  // max object size in bytes; 0 = unlimited
	CacheMaxBytes int64  // cache dir size that triggers finalized-LRU eviction; 0 = unbounded
}

type Service struct {
	st     *store.Store
	dl     Downloader
	cfg    Config
	objDir string
	tmpDir string

	// cacheBytes is a best-effort running total of the cache directory size,
	// seeded by an exact scan at startup and adjusted as files are added/evicted;
	// any drift self-heals on the next restart. Only meaningful when
	// CacheMaxBytes > 0. evicting single-flights the eviction sweep.
	cacheBytes atomic.Int64
	evicting   atomic.Bool
}

func New(st *store.Store, dl Downloader, cfg Config) (*Service, error) {
	objDir := filepath.Join(cfg.DataDir, "objects")
	tmpDir := filepath.Join(cfg.DataDir, "tmp")
	for _, d := range []string{objDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	s := &Service{st: st, dl: dl, cfg: cfg, objDir: objDir, tmpDir: tmpDir}
	// Spool/download temp files have unique names and are never reused, so any
	// left in tmp/ are debris from an interrupted PUT or cold read — clear them.
	clearDir(tmpDir)
	if cfg.CacheMaxBytes > 0 {
		total, err := dirSize(objDir)
		if err != nil {
			return nil, fmt.Errorf("size cache dir: %w", err)
		}
		s.cacheBytes.Store(total)
		s.evictIfNeeded() // a restart with a lowered limit should trim down now
	}
	return s, nil
}

// CachePath returns the cache file location for a root (exists or not).
func (s *Service) CachePath(root string) string {
	return filepath.Join(s.objDir, root)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// clearDir removes the (non-directory) entries directly under dir, best-effort.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// dirSize sums the sizes of the regular files directly under dir.
func dirSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total, nil
}

// touchInterval coarsens LRU updates so the read path stays write-free in the
// common case; minute-granularity recency is plenty for cache eviction.
const touchInterval = time.Minute

// noteAdded accounts for a freshly materialized cache file (fresh PUT, salvage,
// or cold-read restore) and triggers eviction when the cache has grown past its
// limit. A no-op when the cache is unbounded.
func (s *Service) noteAdded(size int64) {
	if s.cfg.CacheMaxBytes <= 0 {
		return
	}
	s.cacheBytes.Add(size)
	s.evictIfNeeded()
}

// touch bumps an object's LRU recency, but only when eviction is enabled and the
// recorded access is stale enough to matter — so reads write metadata at most
// once per touchInterval per object, and never at all when the cache is unbounded.
func (s *Service) touch(root string, last time.Time) {
	if s.cfg.CacheMaxBytes <= 0 || time.Since(last) < touchInterval {
		return
	}
	if err := s.st.Touch(root, time.Now().UTC()); err != nil {
		slog.Warn("cache lru touch", "root", root, "err", err)
	}
}

// ShouldRejectWrite reports whether a new upload must be shed for backpressure:
// the cache is over its limit and eviction can't reclaim enough because the
// overflow is not-yet-finalized data (whose cache file is its only copy and so
// is never evictable). It first attempts a reclaim, so it only returns true when
// the gateway is genuinely stuck — i.e. ingest is outrunning finalization. The
// caller (s3gw) turns a true into a retryable 503 SlowDown. Always false when
// the cache is unbounded.
func (s *Service) ShouldRejectWrite() bool {
	if s.cfg.CacheMaxBytes <= 0 {
		return false
	}
	s.evictIfNeeded() // reclaim finalized space first; reject only if still over
	return s.cacheBytes.Load() > s.cfg.CacheMaxBytes
}

// evictIfNeeded drops the least-recently-used FINALIZED objects' cache files
// until the cache is back under a low-water mark. Non-finalized objects are
// never evicted (their cache file is the only copy until 0G finalization);
// finalized objects are safe to drop because Open restores them from 0G
// (proof-verified) on the next read. One evictor runs at a time; concurrent
// callers return immediately.
func (s *Service) evictIfNeeded() {
	limit := s.cfg.CacheMaxBytes
	if limit <= 0 || s.cacheBytes.Load() <= limit {
		return
	}
	if !s.evicting.CompareAndSwap(false, true) {
		return // another goroutine is already evicting
	}
	defer s.evicting.Store(false)

	low := limit - limit/10 // 10% headroom so we don't evict on every PUT
	cands, err := s.st.FinalizedCacheEntries()
	if err != nil {
		slog.Warn("cache eviction: list candidates", "err", err)
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].LastAccess.Before(cands[j].LastAccess) })
	for _, c := range cands {
		if s.cacheBytes.Load() <= low {
			break
		}
		p := s.CachePath(c.Root)
		fi, err := os.Stat(p)
		if err != nil {
			continue // already evicted / never cached
		}
		if err := os.Remove(p); err != nil {
			slog.Warn("cache eviction: remove", "root", c.Root, "err", err)
			continue
		}
		s.cacheBytes.Add(-fi.Size())
	}
}

// materialize fsyncs the spool file and atomically moves it into the cache at
// dst. The spool is consumed on success and removed on any failure.
func materialize(tmp *os.File, tmpPath, dst string) error {
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// Put ingests one object. The returned bool is true when the content was
// already known (dedup hit) and no new upload was enqueued.
//
// Crash-consistency order: the spool file is fsynced and renamed into the
// cache BEFORE the metadata+task record is committed, so a crash in between
// leaves an orphan cache file (harmless) rather than a task without data.
func (s *Service) Put(ctx context.Context, r io.Reader, filename, contentType string) (store.ObjectMeta, bool, error) {
	tmp, err := os.CreateTemp(s.tmpDir, "spool-*")
	if err != nil {
		return store.ObjectMeta{}, false, err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	h := sha256.New()
	md := md5.New() // S3 ETag for the gofakes3 layer; cheap to compute in the same pass
	src := r
	if s.cfg.MaxSize > 0 {
		src = io.LimitReader(r, s.cfg.MaxSize+1)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h, md), src)
	if err != nil {
		cleanup()
		return store.ObjectMeta{}, false, err
	}
	if n == 0 {
		cleanup()
		return store.ObjectMeta{}, false, ErrEmpty
	}
	if s.cfg.MaxSize > 0 && n > s.cfg.MaxSize {
		cleanup()
		return store.ObjectMeta{}, false, ErrTooLarge
	}
	shaHex := hex.EncodeToString(h.Sum(nil))
	md5Hex := hex.EncodeToString(md.Sum(nil))

	// dedup by plaintext hash: same content → same object, no second upload
	if m, ok, err := s.st.BySHA256(shaHex); err != nil {
		cleanup()
		return store.ObjectMeta{}, false, err
	} else if ok {
		// Until an object is finalized on 0G, the local cache file is its only
		// copy. If a non-finalized object's cache file went missing, salvage it
		// from the bytes we just spooled (same content ⇒ same root) instead of
		// discarding them and leaving a queued upload that can never open it.
		needsCache := m.Status != store.StatusFinalized && !fileExists(s.CachePath(m.Root))
		switch {
		case needsCache:
			if err := materialize(tmp, tmpPath, s.CachePath(m.Root)); err != nil {
				return store.ObjectMeta{}, false, err
			}
			s.noteAdded(m.Size)
			if err := s.st.Reenqueue(m.Root); err != nil {
				return store.ObjectMeta{}, false, err
			}
			m, _, err = s.st.Get(m.Root)
			return m, true, err
		case m.Status == store.StatusFailed:
			// give failed content a fresh chance with the new request
			cleanup()
			if err := s.st.Reenqueue(m.Root); err != nil {
				return store.ObjectMeta{}, false, err
			}
			m, _, _ = s.st.Get(m.Root)
			return m, true, nil
		default:
			cleanup()
			return m, true, nil
		}
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		return store.ObjectMeta{}, false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return store.ObjectMeta{}, false, err
	}

	rootHash, err := core.MerkleRoot(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return store.ObjectMeta{}, false, fmt.Errorf("merkle root: %w", err)
	}
	root := rootHash.Hex()

	if err := os.Rename(tmpPath, s.CachePath(root)); err != nil {
		os.Remove(tmpPath)
		return store.ObjectMeta{}, false, err
	}
	// The new object is not finalized yet, so eviction here can only reclaim
	// OTHER finalized objects — never the bytes just written.
	s.noteAdded(n)

	m := store.ObjectMeta{
		Root:        root,
		SHA256:      shaHex,
		MD5:         md5Hex,
		Size:        n,
		Filename:    filename,
		ContentType: contentType,
		Status:      store.StatusPending,
	}
	if err := s.st.CreateObject(m); err != nil {
		if errors.Is(err, store.ErrExists) {
			// lost a race with an identical concurrent PUT — that's a dedup hit
			ex, _, gerr := s.st.Get(root)
			return ex, true, gerr
		}
		return store.ObjectMeta{}, false, err
	}
	created, _, err := s.st.Get(root)
	return created, false, err
}

// Open returns a readable+seekable handle on the object content, restoring
// it from 0G into the cache first when necessary (proof-verified download).
func (s *Service) Open(ctx context.Context, root string) (*os.File, store.ObjectMeta, error) {
	m, ok, err := s.st.Get(root)
	if err != nil {
		return nil, store.ObjectMeta{}, err
	}
	if !ok {
		return nil, store.ObjectMeta{}, ErrNotFound
	}

	p := s.CachePath(root)
	if f, err := os.Open(p); err == nil {
		// Serve from the local cache only when its size matches the recorded
		// size. A truncated/corrupt file is not served; we fall through to a
		// merkle-proof-verified restore from 0G, which overwrites the bad file
		// on success. (Re-hashing every read would be prohibitive for large
		// objects, so size is the cheap local integrity gate.)
		if fi, statErr := f.Stat(); statErr == nil && fi.Size() == m.Size {
			s.touch(root, m.LastAccess)
			return f, m, nil
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, m, err
	}

	// cold read: restore from 0G via a unique temp path, then move into place
	tmp := filepath.Join(s.tmpDir, fmt.Sprintf("dl-%s-%d", strings.TrimPrefix(root, "0x")[:16], rand.Int63()))
	if err := s.dl.Download(ctx, root, tmp); err != nil {
		os.Remove(tmp)
		return nil, m, fmt.Errorf("restore from 0g: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return nil, m, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, m, err
	}
	// Mark freshest in the LRU BEFORE accounting for the bytes: the open handle
	// above survives an unlink, so even if a racing eviction targets this root
	// the served bytes are safe — but bumping recency first keeps the object we
	// just paid to restore from being the immediate eviction victim.
	if s.cfg.CacheMaxBytes > 0 {
		_ = s.st.Touch(root, time.Now().UTC())
	}
	s.noteAdded(m.Size)
	return f, m, nil
}
