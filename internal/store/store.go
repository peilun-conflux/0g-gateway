// Package store persists object metadata and the async upload task queues in
// a single bbolt database. Every state transition is one transaction so a
// crash never loses an enqueued upload (design: "任务队列必须落库").
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Status string

const (
	StatusPending   Status = "pending"   // cached locally, queued for upload
	StatusSubmitted Status = "submitted" // handed to the chain backend (crash here ⇒ reconcile)
	StatusOnchain   Status = "onchain"   // tx mined and segments uploaded, awaiting finality
	StatusFinalized Status = "finalized" // storage nodes report finalized
	StatusFailed    Status = "failed"    // retries exhausted or pruned
)

var ErrExists = errors.New("object already exists")

// ObjectMeta is the metadata record for one stored object, keyed by its 0G
// merkle root. It is stored as JSON, so adding fields later (e.g. encryption
// parameters) is backward compatible.
type ObjectMeta struct {
	Root        string    `json:"root"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	Filename    string    `json:"filename,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	Status      Status    `json:"status"`
	TxHash      string    `json:"txHash,omitempty"`
	FailReason  string    `json:"failReason,omitempty"`
	Retries     int       `json:"retries"`
	SkipTx      bool      `json:"skipTx,omitempty"` // reconcile decided the entry is already on chain
	Deleted     bool      `json:"deleted,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

var (
	bucketObjects  = []byte("objects")
	bucketSHA      = []byte("sha256")
	bucketUpload   = []byte("q_upload")   // roots with status pending|submitted
	bucketFinalize = []byte("q_finalize") // roots with status onchain
)

type Store struct {
	db *bolt.DB
}

// Open opens (creating if needed) the metadata database at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketObjects, bucketSHA, bucketUpload, bucketFinalize} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func putMeta(tx *bolt.Tx, m ObjectMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketObjects).Put([]byte(m.Root), raw)
}

func getMeta(tx *bolt.Tx, root string) (ObjectMeta, bool, error) {
	raw := tx.Bucket(bucketObjects).Get([]byte(root))
	if raw == nil {
		return ObjectMeta{}, false, nil
	}
	var m ObjectMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return ObjectMeta{}, false, err
	}
	return m, true, nil
}

// requeue places root in the queue matching its status.
func requeue(tx *bolt.Tx, root string, st Status) error {
	key := []byte(root)
	if err := tx.Bucket(bucketUpload).Delete(key); err != nil {
		return err
	}
	if err := tx.Bucket(bucketFinalize).Delete(key); err != nil {
		return err
	}
	switch st {
	case StatusPending, StatusSubmitted:
		return tx.Bucket(bucketUpload).Put(key, []byte{})
	case StatusOnchain:
		return tx.Bucket(bucketFinalize).Put(key, []byte{})
	}
	return nil
}

// CreateObject inserts a new object, indexes its SHA256 and enqueues it for
// upload. Returns ErrExists if the root is already present.
func (s *Store) CreateObject(m ObjectMeta) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketObjects).Get([]byte(m.Root)) != nil {
			return ErrExists
		}
		now := time.Now().UTC()
		m.CreatedAt, m.UpdatedAt = now, now
		if err := putMeta(tx, m); err != nil {
			return err
		}
		if err := tx.Bucket(bucketSHA).Put([]byte(m.SHA256), []byte(m.Root)); err != nil {
			return err
		}
		return requeue(tx, m.Root, m.Status)
	})
}

func (s *Store) Get(root string) (ObjectMeta, bool, error) {
	var m ObjectMeta
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		m, ok, err = getMeta(tx, root)
		return err
	})
	return m, ok, err
}

func (s *Store) BySHA256(sha string) (ObjectMeta, bool, error) {
	var m ObjectMeta
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketSHA).Get([]byte(sha))
		if root == nil {
			return nil
		}
		var err error
		m, ok, err = getMeta(tx, string(root))
		return err
	})
	return m, ok, err
}

// mutate applies fn to an existing object inside one transaction and keeps
// queue membership in sync with the (possibly changed) status.
func (s *Store) mutate(root string, fn func(*ObjectMeta)) (ObjectMeta, error) {
	var out ObjectMeta
	err := s.db.Update(func(tx *bolt.Tx) error {
		m, ok, err := getMeta(tx, root)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("unknown object %s", root)
		}
		fn(&m)
		m.UpdatedAt = time.Now().UTC()
		if err := putMeta(tx, m); err != nil {
			return err
		}
		out = m
		return requeue(tx, root, m.Status)
	})
	return out, err
}

// SetStatus transitions an object and maintains queue membership. txHash and
// failReason are only written when non-empty.
func (s *Store) SetStatus(root string, st Status, txHash, failReason string) error {
	_, err := s.mutate(root, func(m *ObjectMeta) {
		m.Status = st
		if txHash != "" {
			m.TxHash = txHash
		}
		if failReason != "" {
			m.FailReason = failReason
		}
	})
	return err
}

// Reenqueue resets a failed object for a fresh upload attempt.
func (s *Store) Reenqueue(root string) error {
	_, err := s.mutate(root, func(m *ObjectMeta) {
		m.Status = StatusPending
		m.Retries = 0
		m.FailReason = ""
	})
	return err
}

// IncRetries increments and returns the retry counter.
func (s *Store) IncRetries(root string) (int, error) {
	m, err := s.mutate(root, func(m *ObjectMeta) { m.Retries++ })
	return m.Retries, err
}

func (s *Store) SetSkipTx(root string, v bool) error {
	_, err := s.mutate(root, func(m *ObjectMeta) { m.SkipTx = v })
	return err
}

func (s *Store) MarkDeleted(root string) error {
	_, err := s.mutate(root, func(m *ObjectMeta) { m.Deleted = true })
	return err
}

func (s *Store) listQueue(bucket []byte, limit int) ([]ObjectMeta, error) {
	var out []ObjectMeta
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if limit > 0 && len(out) >= limit {
				break
			}
			m, ok, err := getMeta(tx, string(k))
			if err != nil {
				return err
			}
			if ok {
				out = append(out, m)
			}
		}
		return nil
	})
	return out, err
}

// UploadQueue returns objects with status pending or submitted (submitted
// entries are crash leftovers that need reconciliation). limit<=0 means all.
func (s *Store) UploadQueue(limit int) ([]ObjectMeta, error) {
	return s.listQueue(bucketUpload, limit)
}

// FinalizeQueue returns objects with status onchain awaiting finality.
func (s *Store) FinalizeQueue(limit int) ([]ObjectMeta, error) {
	return s.listQueue(bucketFinalize, limit)
}
