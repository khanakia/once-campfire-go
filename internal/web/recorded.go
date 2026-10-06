package web

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"html/template"

	"net/http"
	"strings"
	"sync"

	"github.com/basecamp/once-campfire-go/internal/database"
	"github.com/basecamp/once-campfire-go/internal/gzsplice"
)

// The marker exists only during template execution. The actual response inserts
// the cached message list without copying it through template/fmt/page buffers.

func (s *Server) messageList(ctx context.Context, messages []database.Message) (fragmentEntry, error) {
	var key strings.Builder
	key.WriteString("message-list/")
	for _, message := range messages {
		key.WriteString(messageCacheKey(message))
		key.WriteByte('/')
	}
	if entry, ok := s.fragments.entry(key.String()); ok {
		return entry, nil
	}
	views, err := s.messageItems(ctx, messages)
	if err != nil {
		return fragmentEntry{}, err
	}
	var body strings.Builder
	for _, view := range views {
		body.WriteString(string(view.Fragment))
	}
	html := template.HTML(body.String())
	s.fragments.put(key.String(), html)
	if entry, ok := s.fragments.entry(key.String()); ok {
		return entry, nil
	}
	return fragmentEntry{html: html, digest: sha256.Sum256([]byte(html))}, nil
}

// textDigests remembers the SHA-256 of the layout text around recorded fragments. A room page's
// text is the same for a person and room until something it shows changes, so hashing it on
// every request (~34 KB a page) costs more than looking it up; Go's map hash plus one compare is
// several times cheaper than SHA-256 over the same bytes.
var textDigests = newDigestMemo(16 << 20)

type digestMemo struct {
	mu           sync.Mutex
	current, old map[string]gzsplice.Digest
	used, budget int
}

func newDigestMemo(budget int) *digestMemo {
	return &digestMemo{current: map[string]gzsplice.Digest{}, old: map[string]gzsplice.Digest{}, budget: budget}
}

// maxMemoText bounds one remembered text so a single huge page cannot rotate the generations.
const maxMemoText = 1 << 20

func (m *digestMemo) digest(text string) gzsplice.Digest {
	if len(text) > maxMemoText {
		return sha256.Sum256([]byte(text))
	}
	m.mu.Lock()
	d, ok := m.current[text]
	if !ok {
		if d, ok = m.old[text]; ok {
			m.storeLocked(strings.Clone(text), d)
		}
	}
	m.mu.Unlock()
	if ok {
		return d
	}
	d = sha256.Sum256([]byte(text))
	m.mu.Lock()
	// Clone: text is a slice of this request's rendered page and would keep all of it alive.
	m.storeLocked(strings.Clone(text), d)
	m.mu.Unlock()
	return d
}

func (m *digestMemo) storeLocked(text string, d gzsplice.Digest) {
	if _, exists := m.current[text]; exists {
		return
	}
	if m.used+len(text) > m.budget {
		m.old, m.current, m.used = m.current, map[string]gzsplice.Digest{}, 0
	}
	m.current[text] = d
	m.used += len(text)
}

func writeRecorded(w http.ResponseWriter, status int, rendered, marker string, fragment fragmentEntry) {
	before, after, found := strings.Cut(rendered, marker)
	if !found {
		http.Error(w, "Missing message insertion point", 500)
		return
	}
	writeRecordedParts(w, status, before, textDigests.digest(before), after, textDigests.digest(after), fragment)
}

// writeRecordedParts is writeRecorded for a page already split around its message list, with the
// SHA-256 of each side (the room shell keeps them with the cached shell).
func writeRecordedParts(w http.ResponseWriter, status int, before string, beforeDigest gzsplice.Digest, after string, afterDigest gzsplice.Digest, fragment fragmentEntry) {
	payload := fragment.payload
	if payload == nil {
		payload = []byte(fragment.html)
	}
	parts := [][]byte{[]byte(before), payload, []byte(after)}
	digests := []gzsplice.Digest{beforeDigest, fragment.digest, afterDigest}
	if w.Header().Get("ETag") == "" {
		// Like Rust's Body::Parts, digest boundaries and cached fragment hashes.
		hash := sha256.New()
		for i, part := range parts {
			var size [8]byte
			binary.LittleEndian.PutUint64(size[:], uint64(len(part)))
			hash.Write(size[:])
			hash.Write(digests[i][:])
		}
		w.Header().Set("ETag", fmt.Sprintf("W/\"%x\"", hash.Sum(nil)[:16]))
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
	}
	w.WriteHeader(status)
	if sw, ok := w.(*sessionWriter); ok && sw.failed {
		return
	}
	target := w
	for {
		if buffered, ok := target.(*responseBuffer); ok {
			buffered.parts, buffered.digests = parts, digests
			return
		}
		if wrapper, ok := target.(interface{ Unwrap() http.ResponseWriter }); ok {
			target = wrapper.Unwrap()
		} else {
			break
		}
	}
	for _, part := range parts {
		w.Write(part)
	}
}
