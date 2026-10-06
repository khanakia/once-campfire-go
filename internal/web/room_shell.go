package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

// roomShellParts is a room page without its message list: the HTML before and after the list,
// each with its SHA-256 (for the recorded-page ETag and gzip splicing), all computed once when
// the shell is cached rather than on every request.
type roomShellParts struct {
	before, after             string
	beforeDigest, afterDigest [32]byte
}

// Cache only the surrounding HTML. Authorization and page data are read afresh;
// messages are inserted separately for each request.
func (s *Server) roomShell(p page) (roomShellParts, error) {
	p.Messages, p.MessagesHTML = nil, ""
	// Key the entire remaining page so user, room, account, flash, origin, platform
	// and future template inputs cannot accidentally share an incompatible shell.
	// LoadedAt (room.updated_at) is part of the key, so it is rendered into the shell.
	raw, err := json.Marshal(p)
	if err != nil {
		return roomShellParts{}, err
	}
	key := fmt.Sprintf("room-shell/%x", sha256.Sum256(raw))
	if entry, ok := s.fragments.entry(key); ok && entry.shell != nil {
		return *entry.shell, nil
	}
	// A random marker, so user content can never be mistaken for the insertion point.
	marker := "\x00campfire-" + rand.Text() + "\x00"
	p.MessagesHTML = template.HTML(marker)
	b := borrowBuffer()
	defer releaseBuffer(b)
	if err := s.templates.ExecuteTemplate(b, "room", p); err != nil {
		return roomShellParts{}, err
	}
	html := b.String()
	before, after, found := strings.Cut(html, marker)
	if !found {
		return roomShellParts{}, fmt.Errorf("room template lost its message insertion point")
	}
	shell := &roomShellParts{before: before, after: after, beforeDigest: sha256.Sum256([]byte(before)), afterDigest: sha256.Sum256([]byte(after))}
	s.fragments.putEntry(fragmentEntry{key: key, html: template.HTML(html), shell: shell})
	return *shell, nil
}
