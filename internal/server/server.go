// Package server exposes the gateway HTTP API:
//
//	PUT/POST /objects             multipart ("file" field) or raw body → {root, status, ...}
//	GET      /objects/{root}        object bytes (signed-token auth, Range supported)
//	HEAD     /objects/{root}        headers only
//	GET      /objects/{root}/status lifecycle status (backend-facing, unauthenticated)
//	DELETE   /objects/{root}        logical delete (admin token)
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zgs-gateway/internal/auth"
	"zgs-gateway/internal/object"
	"zgs-gateway/internal/store"
)

type Config struct {
	AuthSecret  string // empty = object reads are unauthenticated
	AdminSecret string // empty = DELETE disabled
}

type handler struct {
	svc *object.Service
	st  *store.Store
	cfg Config
}

func New(svc *object.Service, st *store.Store, cfg Config) http.Handler {
	h := &handler{svc: svc, st: st, cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /objects", h.put)
	mux.HandleFunc("POST /objects", h.put)
	mux.HandleFunc("GET /objects/{root}", h.get)
	mux.HandleFunc("GET /objects/{root}/status", h.status)
	mux.HandleFunc("DELETE /objects/{root}", h.del)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// put accepts either multipart/form-data with a "file" field or a raw body
// (filename via X-Filename, type via Content-Type).
func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	var (
		body        io.Reader
		filename    string
		contentType string
	)
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		mr, err := r.MultipartReader()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad multipart body: "+err.Error())
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				writeErr(w, http.StatusBadRequest, `multipart field "file" missing`)
				return
			}
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad multipart body: "+err.Error())
				return
			}
			if part.FormName() == "file" {
				body = part
				filename = part.FileName()
				contentType = part.Header.Get("Content-Type")
				break
			}
		}
	} else {
		body = r.Body
		filename = r.Header.Get("X-Filename")
		contentType = ct
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta, dedup, err := h.svc.Put(r.Context(), body, filename, contentType)
	switch {
	case errors.Is(err, object.ErrEmpty):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, object.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		slog.Error("put object", "err", err)
		writeErr(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root":   meta.Root,
		"status": string(meta.Status),
		"sha256": meta.SHA256,
		"size":   meta.Size,
		"dedup":  dedup,
	})
}

func (h *handler) authorized(r *http.Request, root string) bool {
	if h.cfg.AuthSecret == "" {
		return true
	}
	expUnix, err := strconv.ParseInt(r.URL.Query().Get("e"), 10, 64)
	if err != nil {
		return false
	}
	return auth.Verify(h.cfg.AuthSecret, root, time.Unix(expUnix, 0), r.URL.Query().Get("t"), time.Now())
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	root := r.PathValue("root")
	if !h.authorized(r, root) {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	f, meta, err := h.svc.Open(r.Context(), root)
	switch {
	case errors.Is(err, object.ErrNotFound):
		writeErr(w, http.StatusNotFound, "object not found")
		return
	case errors.Is(err, object.ErrGone):
		writeErr(w, http.StatusGone, "object deleted")
		return
	case err != nil:
		slog.Error("open object", "root", root, "err", err)
		writeErr(w, http.StatusBadGateway, "object restore failed")
		return
	}
	defer f.Close()

	ct := meta.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if meta.Filename != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.Filename}))
	}
	w.Header().Set("X-Object-Status", string(meta.Status))
	// ServeContent supplies Range/If-Modified-Since/HEAD handling; the name is
	// left empty so the Content-Type set above is authoritative.
	http.ServeContent(w, r, "", meta.UpdatedAt, f)
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	root := r.PathValue("root")
	m, ok, err := h.st.Get(root)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "object not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root":        m.Root,
		"status":      string(m.Status),
		"txHash":      m.TxHash,
		"sha256":      m.SHA256,
		"size":        m.Size,
		"filename":    m.Filename,
		"contentType": m.ContentType,
		"retries":     m.Retries,
		"failReason":  m.FailReason,
		"deleted":     m.Deleted,
		"createdAt":   m.CreatedAt,
		"updatedAt":   m.UpdatedAt,
	})
}

// del performs a logical delete: the gateway refuses to serve the object from
// now on. Data already settled on 0G cannot be erased (content-addressed,
// immutable) — document this clearly for compliance workflows.
func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AdminSecret == "" {
		writeErr(w, http.StatusForbidden, "delete disabled")
		return
	}
	if r.Header.Get("X-Admin-Token") != h.cfg.AdminSecret {
		writeErr(w, http.StatusUnauthorized, "bad admin token")
		return
	}
	root := r.PathValue("root")
	if _, ok, err := h.st.Get(root); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		writeErr(w, http.StatusNotFound, "object not found")
		return
	}
	if err := h.st.MarkDeleted(root); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
