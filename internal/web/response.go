package web

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"net/http"
	"strconv"
	"sync"

	"github.com/basecamp/once-campfire-go/internal/gzsplice"
)

var responseBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func borrowBuffer() *bytes.Buffer { return responseBuffers.Get().(*bytes.Buffer) }
func releaseBuffer(b *bytes.Buffer) {
	if b.Cap() <= 1<<20 {
		b.Reset()
		responseBuffers.Put(b)
	}
}

// Rack::ETag and Rack::ConditionalGet operate on completed, non-streaming bodies.
// Disk/representation downloads and upgraded sockets retain their streaming writers.
type responseBuffer struct {
	http.ResponseWriter
	body      *bytes.Buffer
	status    int
	exception bool
	parts     [][]byte
	// digests holds each part's SHA-256 when writeRecorded computed them, so a gzip writer can
	// splice cached compressed pieces instead of deflating the page again.
	digests []gzsplice.Digest
}

// partsWriter finds a gzsplice.PartsWriter under w's wrappers, or nil.
func partsWriter(w http.ResponseWriter) gzsplice.PartsWriter {
	for {
		if pw, ok := w.(gzsplice.PartsWriter); ok {
			return pw
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = wrapper.Unwrap()
	}
}

func (w *responseBuffer) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *responseBuffer) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body == nil {
		w.body = borrowBuffer()
	}
	return w.body.Write(body)
}
func (w *responseBuffer) finish(r *http.Request) {
	if w.body == nil {
		w.body = borrowBuffer()
	}
	defer releaseBuffer(w.body)
	if w.status == 0 {
		w.status = 200
	}
	h := w.Header()
	digested := false
	var bodyDigest gzsplice.Digest
	if !w.exception && (w.status == 200 || w.status == 201) && w.body.Len() > 0 && h.Get("ETag") == "" && h.Get("Last-Modified") == "" {
		bodyDigest = sha256.Sum256(w.body.Bytes())
		h.Set("ETag", fmt.Sprintf("W/\"%x\"", bodyDigest[:16]))
		digested = true
	}
	if !w.exception && h.Get("Cache-Control") == "" {
		value := "no-cache"
		if digested {
			value = "max-age=0, private, must-revalidate"
		}
		h.Set("Cache-Control", value)
	}
	if w.status == 200 {
		modified, _ := http.ParseTime(h.Get("Last-Modified"))
		if notModified(w.ResponseWriter, r, h.Get("ETag"), modified) {
			return
		}
	}
	if h.Get("Content-Type") == "" && w.status != 204 && w.status != 304 {
		h.Set("Content-Type", "text/html; charset=utf-8")
	}
	if len(w.parts) > 0 && w.status != 204 && w.status != 304 {
		size := 0
		for _, part := range w.parts {
			size += len(part)
		}
		h.Set("Content-Length", strconv.Itoa(size))
	}
	w.ResponseWriter.WriteHeader(w.status)
	if r.Method != "HEAD" && w.status != 204 && w.status != 304 {
		var spliceable []gzsplice.Part
		if len(w.parts) > 0 && len(w.digests) == len(w.parts) {
			spliceable = make([]gzsplice.Part, len(w.parts))
			for i, part := range w.parts {
				spliceable[i] = gzsplice.Part{Bytes: part, Digest: w.digests[i]}
			}
		} else if len(w.parts) == 0 && digested {
			// Rust's BodyDigest: a repeated body (the sidebar) is gzipped from the cache.
			spliceable = []gzsplice.Part{{Bytes: w.body.Bytes(), Digest: bodyDigest}}
		}
		if spliceable != nil {
			if pw := partsWriter(w.ResponseWriter); pw != nil && pw.WriteParts(spliceable) {
				return
			}
		}
		if len(w.parts) > 0 {
			for _, part := range w.parts {
				w.ResponseWriter.Write(part)
			}
		} else {
			w.ResponseWriter.Write(w.body.Bytes())
		}
	}
}
