package s3gw

import (
	"encoding/xml"
	"net/http"

	"github.com/johannesboyne/gofakes3"
)

// BackpressureHandler sheds object uploads when the local cache is full of
// not-yet-finalized data that eviction cannot reclaim (a writer outrunning the
// upload worker). It returns a retryable 503 SlowDown so S3 clients back off —
// their SDKs retry it automatically with exponential backoff — until the worker
// finalizes the backlog and frees evictable space. Without it, sustained writes
// would grow the cache until the disk fills.
//
// It runs at the HTTP layer, outermost in Wrap, so an over-pressure request is
// rejected before gofakes3 reads (and buffers) the upload body. gofakes3 cannot
// emit a 503 itself: its ErrorCode→status table has no SlowDown, so a Backend
// error could only surface as 500.
func (b *Backend) BackpressureHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isObjectWrite(r) && b.svc.ShouldRejectWrite() {
			writeSlowDown(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isObjectWrite reports whether r is a direct object upload (PUT of a key that
// streams new bytes into the cache). A server-side copy is excluded: it is
// zero-copy (only remaps bucket/key→root) and adds no cache bytes, so it must
// not be throttled.
func isObjectWrite(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		return false
	}
	_, _, ok := splitBucketKey(r.URL.Path)
	return ok
}

// writeSlowDown emits a standard S3 SlowDown error with a 503 status and a
// Retry-After hint.
func writeSlowDown(w http.ResponseWriter) {
	resp := gofakes3.ErrorResponse{
		Code:    gofakes3.ErrorCode("SlowDown"),
		Message: "cache is saturated with unfinalized objects; retry after the upload backlog drains",
	}
	body, err := xml.Marshal(&resp)
	if err != nil { // marshaling a fixed struct cannot realistically fail
		http.Error(w, "SlowDown", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
