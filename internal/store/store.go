// Package store persists object metadata and the async upload task queues in
// a single bbolt database. Every state transition is one transaction so a
// crash never loses an enqueued upload (design: "任务队列必须落库").
package store

import (
	"bytes"
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

var (
	ErrExists         = errors.New("object already exists")
	ErrBucketExists   = errors.New("bucket already exists")
	ErrBucketNotFound = errors.New("bucket not found")
	ErrBucketNotEmpty = errors.New("bucket not empty")
)

// BucketRecord is the registry entry for one S3-style bucket.
type BucketRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// S3Object pairs a bucket-relative key with its resolved object metadata.
type S3Object struct {
	Key  string
	Meta ObjectMeta
}

// ObjectMeta is the metadata record for one stored object, keyed by its 0G
// merkle root. It is stored as JSON, so adding fields later (e.g. encryption
// parameters) is backward compatible.
type ObjectMeta struct {
	Root        string    `json:"root"`
	SHA256      string    `json:"sha256"`
	MD5         string    `json:"md5,omitempty"` // hex MD5 of content; S3 ETag for the gofakes3 layer
	Size        int64     `json:"size"`
	Filename    string    `json:"filename,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	Status      Status    `json:"status"`
	TxHash      string    `json:"txHash,omitempty"`
	FailReason  string    `json:"failReason,omitempty"`
	Retries     int       `json:"retries"`
	SkipTx      bool      `json:"skipTx,omitempty"` // reconcile decided the entry is already on chain
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

var (
	bucketObjects  = []byte("objects")
	bucketSHA      = []byte("sha256")
	bucketUpload   = []byte("q_upload")   // roots with status pending|submitted
	bucketFinalize = []byte("q_finalize") // roots with status onchain
	bucketS3Bkts   = []byte("s3_buckets") // S3 bucket name → BucketRecord(JSON)
	bucketS3Keys   = []byte("s3_keys")    // "bucket/key" → root (S3 object index)
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
		for _, b := range [][]byte{bucketObjects, bucketSHA, bucketUpload, bucketFinalize, bucketS3Bkts, bucketS3Keys} {
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

// requeue keeps an object's queue membership in sync with its current status:
// it clears both queues, then re-enqueues into the one matching the status.
func requeue(tx *bolt.Tx, m ObjectMeta) error {
	key := []byte(m.Root)
	if err := tx.Bucket(bucketUpload).Delete(key); err != nil {
		return err
	}
	if err := tx.Bucket(bucketFinalize).Delete(key); err != nil {
		return err
	}
	switch m.Status {
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
		return requeue(tx, m)
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
		return requeue(tx, m)
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

// --- S3-style bucket + object-key index (gofakes3 layer) ---
//
// Object keys are stored in bucketS3Keys under the composed key "bucket/key".
// S3 bucket names cannot contain "/", so "bucket/" is an unambiguous prefix.

func s3Composite(bucket, key string) []byte { return []byte(bucket + "/" + key) }
func s3Prefix(bucket string) []byte         { return []byte(bucket + "/") }

// S3CreateBucket registers a bucket. Returns ErrBucketExists if it already does.
func (s *Store) S3CreateBucket(name string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketS3Bkts)
		if b.Get([]byte(name)) != nil {
			return ErrBucketExists
		}
		raw, err := json.Marshal(BucketRecord{Name: name, CreatedAt: now.UTC()})
		if err != nil {
			return err
		}
		return b.Put([]byte(name), raw)
	})
}

func (s *Store) S3BucketExists(name string) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bucketS3Bkts).Get([]byte(name)) != nil
		return nil
	})
	return ok, err
}

func (s *Store) S3ListBuckets() ([]BucketRecord, error) {
	var out []BucketRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketS3Bkts).ForEach(func(_, v []byte) error {
			var r BucketRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *Store) bucketHasKeys(tx *bolt.Tx, name string) bool {
	pfx := s3Prefix(name)
	k, _ := tx.Bucket(bucketS3Keys).Cursor().Seek(pfx)
	return k != nil && bytes.HasPrefix(k, pfx)
}

// S3DeleteBucket deletes an empty bucket. ErrBucketNotFound / ErrBucketNotEmpty otherwise.
func (s *Store) S3DeleteBucket(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketS3Bkts)
		if b.Get([]byte(name)) == nil {
			return ErrBucketNotFound
		}
		if s.bucketHasKeys(tx, name) {
			return ErrBucketNotEmpty
		}
		return b.Delete([]byte(name))
	})
}

// S3ForceDeleteBucket deletes a bucket and all its object-key mappings.
func (s *Store) S3ForceDeleteBucket(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketS3Bkts)
		if b.Get([]byte(name)) == nil {
			return ErrBucketNotFound
		}
		keys := tx.Bucket(bucketS3Keys)
		c := keys.Cursor()
		pfx := s3Prefix(name)
		for k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
			if err := keys.Delete(k); err != nil {
				return err
			}
		}
		return b.Delete([]byte(name))
	})
}

// S3PutObjectKey maps bucket/key → root. The bucket's existence is verified in
// the same transaction as the key write: callers check the bucket earlier (before
// the slow ingest), but a concurrent bucket delete could land in between, so the
// authoritative check is here — otherwise a raced delete would leave an orphan
// key under a non-existent bucket. Returns ErrBucketNotFound if the bucket is gone.
func (s *Store) S3PutObjectKey(bucket, key, root string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketS3Bkts).Get([]byte(bucket)) == nil {
			return ErrBucketNotFound
		}
		return tx.Bucket(bucketS3Keys).Put(s3Composite(bucket, key), []byte(root))
	})
}

func (s *Store) S3GetObjectKey(bucket, key string) (root string, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketS3Keys).Get(s3Composite(bucket, key)); v != nil {
			root, ok = string(v), true
		}
		return nil
	})
	return root, ok, err
}

func (s *Store) S3DeleteObjectKey(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketS3Keys).Delete(s3Composite(bucket, key))
	})
}

// S3ListObjects returns every object in the bucket as (key, metadata),
// resolving each root within the SAME read transaction to avoid N+1 lookups.
// Prefix matching for the S3 list request is applied by the caller.
func (s *Store) S3ListObjects(bucket string) ([]S3Object, error) {
	var out []S3Object
	pfx := s3Prefix(bucket)
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketS3Keys).Cursor()
		for k, v := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx); k, v = c.Next() {
			m, ok, err := getMeta(tx, string(v))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			out = append(out, S3Object{Key: string(k[len(pfx):]), Meta: m})
		}
		return nil
	})
	return out, err
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
