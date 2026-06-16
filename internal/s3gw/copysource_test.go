package s3gw

import "testing"

func TestNormalizeCopySource(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		// Huawei OBS SDK encodes the whole value, including the "/" separators —
		// the case that panics raw gofakes3.
		{"obs fully encoded", "demo%2Fdocs%2Fhello.txt", "/demo/docs%2Fhello.txt", true},
		{"obs encoded with leading slash", "%2Fdemo%2Fdocs%2Fhello.txt", "/demo/docs%2Fhello.txt", true},
		// AWS SDK form (leading slash, key query-escaped) must round-trip unchanged.
		{"aws form idempotent", "/demo/docs%2Fhello.txt", "/demo/docs%2Fhello.txt", true},
		{"plain key no slash", "/demo/hello.txt", "/demo/hello.txt", true},
		{"nested key", "/demo/a/b/c.txt", "/demo/a%2Fb%2Fc.txt", true},
		{"strips version subresource", "/demo/hello.txt?versionId=abc", "/demo/hello.txt", true},
		// an encoded "?" in the key must NOT be treated as a subresource delimiter
		// (regression: must round-trip like raw gofakes3 does, not get truncated).
		{"encoded question mark in key", "/demo/a%3Fb.txt", "/demo/a%3Fb.txt", true},
		{"no key segment", "demo", "", false},
		{"only bucket with slash", "/demo/", "", false},
		{"invalid percent escape", "demo%2Fsrc%2zkey", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizeCopySource(c.in)
			if ok != c.ok || (ok && got != c.want) {
				t.Fatalf("normalizeCopySource(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}
