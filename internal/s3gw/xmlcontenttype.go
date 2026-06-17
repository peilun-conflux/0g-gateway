package s3gw

import (
	"bytes"
	"net/http"
	"strings"
)

// FixXMLContentTypeHandler normalizes the Content-Type of gofakes3's XML
// protocol responses to "application/xml".
//
// gofakes3 does not set a Content-Type on its XML responses; it lets net/http's
// content sniffer label them "text/xml; charset=utf-8" at write time. The Huawei
// OBS Java SDK strictly verifies the response Content-Type (default
// setVerifyResponseContentType(true)) and rejects that "; charset=utf-8"
// parameter, failing list/copy/bucket calls with "Expected XML document response
// ... but received content type text/xml; charset=utf-8". Setting
// "application/xml" before the body is written pre-empts the sniffer and lets the
// SDK work with its default settings (no client-side workaround required).
//
// Object reads (GET/HEAD of a key) are passed through UNWRAPPED: their body is
// the object itself (its own Content-Type, possibly large — this preserves
// net/http's sendfile/ReadFrom fast path). Only the small XML responses from
// bucket/list/copy operations are wrapped.
func (b *Backend) FixXMLContentTypeHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isObjectRead(r) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&xmlContentTypeWriter{ResponseWriter: w}, r)
	})
}

// isObjectRead reports whether the request is a GET/HEAD of an object key (as
// opposed to a bucket-level or write operation), using the same path split as
// the image middleware.
func isObjectRead(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	_, _, ok := splitBucketKey(r.URL.Path)
	return ok
}

type xmlContentTypeWriter struct {
	http.ResponseWriter
	committed bool
}

// normalize runs once, at the moment the Content-Type is decided (first Write,
// or WriteHeader if there is no body). It rewrites an explicit text/xml type and,
// when gofakes3 left the type unset, sets application/xml for an XML body before
// net/http's sniffer would label it "text/xml; charset=utf-8".
func (x *xmlContentTypeWriter) normalize(body []byte) {
	if x.committed {
		return
	}
	x.committed = true
	h := x.Header()
	switch ct := h.Get("Content-Type"); {
	case strings.HasPrefix(ct, "text/xml"):
		h.Set("Content-Type", "application/xml")
	case ct == "" && bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("<?xml")):
		h.Set("Content-Type", "application/xml")
	}
}

func (x *xmlContentTypeWriter) WriteHeader(code int) {
	x.normalize(nil)
	x.ResponseWriter.WriteHeader(code)
}

func (x *xmlContentTypeWriter) Write(p []byte) (int, error) {
	x.normalize(p)
	return x.ResponseWriter.Write(p)
}
