// Package s3gw adapts the gofakes3 S3 server onto the gateway's content-
// addressed 0G store. It implements gofakes3.Backend by mapping S3 bucket+key
// operations onto object.Service (Put/Open over 0G) and store (bucket registry
// + bucket/key→root index).
//
// Scope: demo-grade. No signature verification (gofakes3 doesn't verify), no
// multipart backend (gofakes3 buffers parts in memory and calls PutObject once
// — fine for small files), no versioning.
package s3gw

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"time"

	"github.com/johannesboyne/gofakes3"

	"zgs-gateway/internal/object"
	"zgs-gateway/internal/store"
)

type Backend struct {
	// ctx is the gateway's lifecycle context. gofakes3's Backend interface does
	// not pass the per-request context down, so a cold read from 0G (a possibly
	// slow, proof-verified download) cannot be tied to the HTTP request and
	// canceled on client disconnect. Tying it to ctx at least cancels in-flight
	// cold reads on server shutdown instead of leaking them.
	ctx context.Context
	svc *object.Service
	st  *store.Store
}

var _ gofakes3.Backend = (*Backend)(nil)

func New(ctx context.Context, svc *object.Service, st *store.Store) *Backend {
	return &Backend{ctx: ctx, svc: svc, st: st}
}

// --- buckets ---

func (b *Backend) ListBuckets() ([]gofakes3.BucketInfo, error) {
	recs, err := b.st.S3ListBuckets()
	if err != nil {
		return nil, err
	}
	out := make([]gofakes3.BucketInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, gofakes3.BucketInfo{Name: r.Name, CreationDate: gofakes3.NewContentTime(r.CreatedAt)})
	}
	return out, nil
}

func (b *Backend) CreateBucket(name string) error {
	if err := b.st.S3CreateBucket(name, time.Now()); err != nil {
		if errors.Is(err, store.ErrBucketExists) {
			return gofakes3.ResourceError(gofakes3.ErrBucketAlreadyExists, name)
		}
		return err
	}
	return nil
}

func (b *Backend) BucketExists(name string) (bool, error) {
	return b.st.S3BucketExists(name)
}

func (b *Backend) DeleteBucket(name string) error {
	switch err := b.st.S3DeleteBucket(name); {
	case errors.Is(err, store.ErrBucketNotFound):
		return gofakes3.ErrNoSuchBucket
	case errors.Is(err, store.ErrBucketNotEmpty):
		return gofakes3.ResourceError(gofakes3.ErrBucketNotEmpty, name)
	default:
		return err
	}
}

func (b *Backend) ForceDeleteBucket(name string) error {
	if err := b.st.S3ForceDeleteBucket(name); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			return gofakes3.BucketNotFound(name)
		}
		return err
	}
	return nil
}

// --- objects ---

func (b *Backend) PutObject(bucketName, key string, meta map[string]string, input io.Reader, size int64, conditions *gofakes3.PutConditions) (gofakes3.PutObjectResult, error) {
	var res gofakes3.PutObjectResult
	if err := b.requireBucket(bucketName); err != nil {
		return res, err
	}

	// Honor If-Match / If-None-Match before ingesting. The check is not atomic
	// with the write (acceptable for this layer), but avoids silently succeeding
	// on a conditional PUT that should fail.
	if conditions != nil {
		info, err := b.conditionalInfo(bucketName, key)
		if err != nil {
			return res, err
		}
		if err := gofakes3.CheckPutConditions(conditions, info); err != nil {
			return res, err
		}
	}

	m, _, err := b.svc.Put(b.ctx, input, path.Base(key), contentType(meta))
	if err != nil {
		switch {
		case errors.Is(err, object.ErrEmpty):
			// 0G cannot address zero-byte objects (no merkle root for 0 bytes).
			return res, gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, "zero-byte objects are not supported by the 0G backend")
		case errors.Is(err, object.ErrTooLarge):
			return res, gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, err.Error())
		default:
			return res, err
		}
	}
	if err := b.st.S3PutObjectKey(bucketName, key, m.Root); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			// bucket was deleted between the guard above and this write
			return res, gofakes3.BucketNotFound(bucketName)
		}
		return res, err
	}
	return res, nil
}

// conditionalInfo reports the destination object's existence + ETag for
// CheckPutConditions.
func (b *Backend) conditionalInfo(bucket, key string) (*gofakes3.ConditionalObjectInfo, error) {
	root, ok, err := b.st.S3GetObjectKey(bucket, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &gofakes3.ConditionalObjectInfo{Exists: false}, nil
	}
	m, ok, err := b.st.Get(root)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &gofakes3.ConditionalObjectInfo{Exists: false}, nil
	}
	return &gofakes3.ConditionalObjectInfo{Exists: true, Hash: hexBytes(m.MD5)}, nil
}

func (b *Backend) GetObject(bucketName, objectName string, rangeRequest *gofakes3.ObjectRangeRequest) (*gofakes3.Object, error) {
	root, ok, err := b.resolve(bucketName, objectName)
	if err != nil || !ok {
		return nil, err
	}

	f, m, err := b.svc.Open(b.ctx, root)
	if err != nil {
		if errors.Is(err, object.ErrNotFound) {
			return nil, gofakes3.KeyNotFound(objectName)
		}
		return nil, err
	}

	obj := &gofakes3.Object{
		Name:     objectName,
		Size:     m.Size,
		Hash:     hexBytes(m.MD5),
		Metadata: metadataOf(m),
		Contents: f,
	}

	rng, err := rangeRequest.Range(m.Size)
	if err != nil {
		f.Close()
		return nil, err
	}
	if rng != nil {
		if _, err := f.Seek(rng.Start, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		obj.Range = rng
		obj.Contents = sectionCloser{Reader: io.LimitReader(f, rng.Length), closer: f}
	}
	return obj, nil
}

func (b *Backend) HeadObject(bucketName, objectName string) (*gofakes3.Object, error) {
	// HEAD only needs metadata; avoid restoring the object from 0G.
	root, ok, err := b.resolve(bucketName, objectName)
	if err != nil || !ok {
		return nil, err
	}
	m, ok, err := b.st.Get(root)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, gofakes3.KeyNotFound(objectName)
	}
	return &gofakes3.Object{
		Name:     objectName,
		Size:     m.Size,
		Hash:     hexBytes(m.MD5),
		Metadata: metadataOf(m),
		Contents: noopReadCloser{},
	}, nil
}

func (b *Backend) DeleteObject(bucketName, objectName string) (gofakes3.ObjectDeleteResult, error) {
	var res gofakes3.ObjectDeleteResult
	if err := b.requireBucket(bucketName); err != nil {
		return res, err
	}
	// S3 DeleteObject must not error when the key is absent.
	return res, b.st.S3DeleteObjectKey(bucketName, objectName)
}

func (b *Backend) DeleteMulti(bucketName string, objects ...string) (gofakes3.MultiDeleteResult, error) {
	var res gofakes3.MultiDeleteResult
	if err := b.requireBucket(bucketName); err != nil {
		return res, err
	}
	for _, o := range objects {
		if err := b.st.S3DeleteObjectKey(bucketName, o); err != nil {
			res.Error = append(res.Error, gofakes3.ErrorResult{Code: gofakes3.ErrInternal, Message: err.Error(), Key: o})
		} else {
			res.Deleted = append(res.Deleted, gofakes3.ObjectID{Key: o})
		}
	}
	return res, nil
}

// CopyObject re-maps the destination key to the source's root — zero-copy,
// thanks to content addressing (identical bytes ⇒ identical root, no re-upload
// or byte movement). Metadata replacement (x-amz-metadata-directive: REPLACE)
// is not supported: the copy shares the source object's stored content type.
func (b *Backend) CopyObject(srcBucket, srcKey, dstBucket, dstKey string, meta map[string]string) (gofakes3.CopyObjectResult, error) {
	var res gofakes3.CopyObjectResult
	root, ok, err := b.resolve(srcBucket, srcKey)
	if err != nil || !ok {
		return res, err
	}
	if err := b.requireBucket(dstBucket); err != nil {
		return res, err
	}
	m, ok, err := b.st.Get(root)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, gofakes3.KeyNotFound(srcKey)
	}
	if err := b.st.S3PutObjectKey(dstBucket, dstKey, root); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			return res, gofakes3.BucketNotFound(dstBucket)
		}
		return res, err
	}
	return gofakes3.CopyObjectResult{
		ETag:         gofakes3.FormatETag(hexBytes(m.MD5)),
		LastModified: gofakes3.NewContentTime(m.UpdatedAt),
	}, nil
}

func (b *Backend) ListBucket(name string, prefix *gofakes3.Prefix, page gofakes3.ListBucketPage) (*gofakes3.ObjectList, error) {
	if err := b.requireBucket(name); err != nil {
		return nil, err
	}
	if prefix == nil {
		prefix = &gofakes3.Prefix{}
	}
	if !page.IsEmpty() {
		// Pagination not implemented; gofakes3 may retry without a page.
		return nil, gofakes3.ErrInternalPageNotImplemented
	}

	objs, err := b.st.S3ListObjects(name) // resolves metadata in one transaction
	if err != nil {
		return nil, err
	}
	list := gofakes3.NewObjectList()
	var match gofakes3.PrefixMatch
	for _, o := range objs {
		switch {
		case !prefix.Match(o.Key, &match):
			continue
		case match.CommonPrefix:
			list.AddPrefix(match.MatchedPart)
		default:
			list.Add(&gofakes3.Content{
				Key:          o.Key,
				ETag:         gofakes3.FormatETag(hexBytes(o.Meta.MD5)),
				Size:         o.Meta.Size,
				LastModified: gofakes3.NewContentTime(o.Meta.UpdatedAt),
			})
		}
	}
	return list, nil
}

// --- helpers ---

// requireBucket returns gofakes3.BucketNotFound(name) if the bucket is absent.
func (b *Backend) requireBucket(name string) error {
	exists, err := b.st.S3BucketExists(name)
	if err != nil {
		return err
	}
	if !exists {
		return gofakes3.BucketNotFound(name)
	}
	return nil
}

// resolve maps bucket+key to a root, returning the right gofakes3 error when
// the bucket or key is missing.
func (b *Backend) resolve(bucket, key string) (root string, ok bool, err error) {
	root, ok, err = b.st.S3GetObjectKey(bucket, key)
	if err != nil {
		return "", false, err
	}
	if ok {
		return root, true, nil
	}
	if err := b.requireBucket(bucket); err != nil {
		return "", false, err
	}
	return "", false, gofakes3.KeyNotFound(key)
}

func contentType(meta map[string]string) string {
	// gofakes3 builds meta from canonical http.Header keys, so the key is exactly
	// "Content-Type" — a direct lookup suffices.
	if ct := meta["Content-Type"]; ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func metadataOf(m store.ObjectMeta) map[string]string {
	md := map[string]string{}
	if m.ContentType != "" {
		md["Content-Type"] = m.ContentType
	}
	return md
}

func hexBytes(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

type noopReadCloser struct{}

func (noopReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (noopReadCloser) Close() error             { return nil }

type sectionCloser struct {
	io.Reader
	closer io.Closer
}

func (s sectionCloser) Close() error { return s.closer.Close() }
