package s3gw

import (
	"net/http"
	"net/url"
	"strings"
)

// Wrap applies the gateway's S3-compatibility HTTP middlewares in front of the
// gofakes3 handler: X-Amz-Copy-Source normalization, on-the-fly image
// processing, and XML-response Content-Type normalization (for the strict
// Huawei OBS Java SDK). main.go and the integration harness both use it so the
// middleware stack can't drift between production and tests.
func (b *Backend) Wrap(next http.Handler) http.Handler {
	return b.FixCopySourceHandler(b.ImageProcessHandler(b.FixXMLContentTypeHandler(next)))
}

// FixCopySourceHandler rewrites the X-Amz-Copy-Source header into the exact form
// gofakes3 expects before it parses the request.
//
// gofakes3's copyObject splits the raw header on "/" *before* url-decoding it
// (it does `SplitN(TrimPrefix(source,"/"), "/", 2)` then indexes `parts[1]`).
// The Huawei OBS SDK percent-encodes the whole copy-source value, including the
// "/" separators, so a fully-encoded source has no literal "/" to split on and
// gofakes3 panics with an index-out-of-range. We normalize the header to
// "/<bucket>/<query-escaped key>" — what gofakes3 then url.QueryUnescape's back —
// which is idempotent for already-correct requests (e.g. the AWS SDK), so it is
// safe to apply unconditionally.
func (b *Backend) FixCopySourceHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			if norm, ok := normalizeCopySource(src); ok {
				r.Header.Set("X-Amz-Copy-Source", norm)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeCopySource converts any encoding of an S3 copy source into the
// "/<bucket>/<query-escaped key>" form gofakes3 parses. It fully decodes the
// input, drops a leading "/" and any "?versionId" subresource (gofakes3 ignores
// versions), splits bucket/key on the first "/", and re-escapes the key so
// gofakes3's url.QueryUnescape recovers it exactly. ok is false (leave the
// header untouched) when the value can't be decoded or has no key segment.
func normalizeCopySource(src string) (string, bool) {
	// Drop a "?versionId=..." subresource on the RAW header — SDKs append it as a
	// literal "?". Doing this BEFORE decoding is deliberate: a key that legitimately
	// contains an encoded "%3F" must survive (gofakes3 likewise splits only on a
	// literal "?"); cutting after decoding would truncate such a key.
	if i := strings.IndexByte(src, '?'); i >= 0 {
		src = src[:i]
	}
	dec, err := url.PathUnescape(strings.TrimPrefix(src, "/"))
	if err != nil {
		return "", false
	}
	// splitBucketKey strips a leading "/" (covering a %2F-encoded one that decoded
	// to a literal slash) and enforces a non-empty key — the same split the rest
	// of the S3 path handling uses.
	bucket, key, ok := splitBucketKey(dec)
	if !ok {
		return "", false
	}
	return "/" + bucket + "/" + url.QueryEscape(key), true
}
