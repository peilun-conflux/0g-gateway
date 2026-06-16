package s3gw

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"zgs-gateway/internal/imageproc"
)

// ImageProcessHandler wraps next (the gofakes3 handler) with Huawei-OBS-style
// on-the-fly image processing. A GET carrying ?x-image-process=image/resize,...
// is served as a resized rendering; every other request passes through to next
// unchanged. gofakes3's Backend interface can't see arbitrary query params, so
// this has to sit in front of it as HTTP middleware.
//
// Only the `image/resize` action is supported (w_, h_, m_); other actions or
// specs fall through and return the original object.
func (b *Backend) ImageProcessHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := r.URL.Query().Get("x-image-process")
		if r.Method != http.MethodGet || spec == "" {
			next.ServeHTTP(w, r)
			return
		}
		rw, rh, mode, ok := parseHuaweiResize(spec)
		if !ok {
			next.ServeHTTP(w, r) // unsupported spec → serve the original object
			return
		}
		bucket, key, ok := splitBucketKey(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		root, found, err := b.st.S3GetObjectKey(bucket, key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			next.ServeHTTP(w, r) // let gofakes3 produce the proper NoSuchKey/NoSuchBucket
			return
		}

		f, m, err := b.svc.Open(r.Context(), root)
		if err != nil {
			http.Error(w, "object restore failed", http.StatusBadGateway)
			return
		}
		defer f.Close()

		out, ct, err := imageproc.ResizeReader(f, m.Size, rw, rh, mode)
		if err != nil {
			if errors.Is(err, imageproc.ErrTooLarge) {
				http.Error(w, "image too large to process", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "not a resizable image", http.StatusBadRequest)
			}
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})
}

const maxResizeDim = 4096

// parseHuaweiResize parses the `image/resize,w_200,h_100,m_lfit` action from an
// x-image-process spec. ok is false unless a resize action with a positive
// dimension is present. Modes: fill→Fill, fixed→Fixed, everything else
// (lfit default, mfit, pad) → Lfit (fit inside, preserve aspect).
func parseHuaweiResize(spec string) (w, h int, mode imageproc.Mode, ok bool) {
	mode = imageproc.Lfit
	spec = strings.TrimPrefix(spec, "image/")
	for _, action := range strings.Split(spec, "/") {
		parts := strings.Split(action, ",")
		if parts[0] != "resize" {
			continue
		}
		for _, p := range parts[1:] {
			kv := strings.SplitN(p, "_", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "w":
				w = clampDim(atoi(kv[1]))
			case "h":
				h = clampDim(atoi(kv[1]))
			case "m":
				switch kv[1] {
				case "fill":
					mode = imageproc.Fill
				case "fixed":
					mode = imageproc.Fixed
				}
			}
		}
		return w, h, mode, w > 0 || h > 0
	}
	return 0, 0, mode, false
}

// splitBucketKey extracts bucket and key from a path-style URL path
// ("/bucket/key..."). ok is false when there is no key segment.
func splitBucketKey(p string) (bucket, key string, ok bool) {
	p = strings.TrimPrefix(p, "/")
	i := strings.IndexByte(p, '/')
	if i < 0 || i+1 >= len(p) {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func clampDim(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxResizeDim {
		return maxResizeDim
	}
	return n
}
