package admin

import (
	"bytes"
	"io"
	"net/http"
	"path"
	"strings"
)

var uiMimeTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".json": "application/json; charset=utf-8",
	".png":  "image/png",
}

// handleStatic serves embedded admin UI assets with explicit MIME types and no-store caching.
func (s *Server) handleStatic() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/admin/")
		rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
		if rel == "" {
			rel = "index.html"
		}
		f, err := s.ui.Open(rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}

		if ct, ok := uiMimeTypes[strings.ToLower(path.Ext(rel))]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		rs, ok := f.(io.ReadSeeker)
		if !ok {
			data, err := io.ReadAll(f)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			rs = bytes.NewReader(data)
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
	}
}
