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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/0gfoundation/0g-storage-client/core"

	"zgs-gateway/internal/store"
)

var (
	ErrEmpty    = errors.New("empty object rejected (0G cannot address zero-byte files)")
	ErrTooLarge = errors.New("object exceeds the configured size limit")
	ErrNotFound = errors.New("object not found")
	ErrGone     = errors.New("object deleted")
)

// Downloader restores an object from 0G into a local file (cold read path).
type Downloader interface {
	Download(ctx context.Context, root, dest string) error
}

type Config struct {
	DataDir string // cache root; spool files live under DataDir/tmp
	MaxSize int64  // max object size in bytes; 0 = unlimited
}

type Service struct {
	st     *store.Store
	dl     Downloader
	cfg    Config
	objDir string
	tmpDir string
}

func New(st *store.Store, dl Downloader, cfg Config) (*Service, error) {
	objDir := filepath.Join(cfg.DataDir, "objects")
	tmpDir := filepath.Join(cfg.DataDir, "tmp")
	for _, d := range []string{objDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Service{st: st, dl: dl, cfg: cfg, objDir: objDir, tmpDir: tmpDir}, nil
}

// CachePath returns the cache file location for a root (exists or not).
func (s *Service) CachePath(root string) string {
	return filepath.Join(s.objDir, root)
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
	src := r
	if s.cfg.MaxSize > 0 {
		src = io.LimitReader(r, s.cfg.MaxSize+1)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
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

	// dedup by plaintext hash: same content → same object, no second upload
	if m, ok, err := s.st.BySHA256(shaHex); err != nil {
		cleanup()
		return store.ObjectMeta{}, false, err
	} else if ok && !m.Deleted {
		if m.Status == store.StatusFailed {
			// give failed content a fresh chance with the new request
			if err := s.st.Reenqueue(m.Root); err != nil {
				cleanup()
				return store.ObjectMeta{}, false, err
			}
			m, _, _ = s.st.Get(m.Root)
		}
		cleanup()
		return m, true, nil
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

	m := store.ObjectMeta{
		Root:        root,
		SHA256:      shaHex,
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
	if m.Deleted {
		return nil, m, ErrGone
	}

	p := s.CachePath(root)
	f, err := os.Open(p)
	if err == nil {
		return f, m, nil
	}
	if !os.IsNotExist(err) {
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
	f, err = os.Open(p)
	return f, m, err
}
