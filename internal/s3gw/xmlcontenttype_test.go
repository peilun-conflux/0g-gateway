package s3gw

import (
	"net/http/httptest"
	"testing"
)

func TestIsObjectRead(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/bucket/key", true},
		{"HEAD", "/bucket/dir/key.txt", true},
		{"GET", "/bucket", false},     // list
		{"GET", "/bucket/", false},    // list (trailing slash, no key)
		{"PUT", "/bucket/key", false}, // object write, not a read
		{"GET", "/", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isObjectRead(r); got != c.want {
			t.Errorf("isObjectRead(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestXMLContentTypeWriter(t *testing.T) {
	// gofakes3 leaves Content-Type unset and writes an XML body; we must set
	// application/xml before the body so net/http won't sniff text/xml;charset.
	t.Run("sniffed xml body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &xmlContentTypeWriter{ResponseWriter: rec}
		_, _ = w.Write([]byte(`<?xml version="1.0"?><ListBucketResult/>`))
		if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
			t.Fatalf("content-type = %q, want application/xml", ct)
		}
	})

	t.Run("explicit text/xml rewritten", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &xmlContentTypeWriter{ResponseWriter: rec}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.WriteHeader(200)
		if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
			t.Fatalf("content-type = %q, want application/xml", ct)
		}
	})

	t.Run("non-xml left alone", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &xmlContentTypeWriter{ResponseWriter: rec}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("binary"))
		if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("content-type = %q, want application/octet-stream", ct)
		}
	})
}
