// Package gzsplice writes gzip members for bodies made of parts that repeat from one response to
// the next (a page's layout and its cached message list), without compressing a part again once it
// has been compressed after the same predecessor.
//
// Each part is deflated once, with the tail of the part before it as the preset dictionary, and
// ended with a sync flush so it finishes on a byte boundary. Deflate back-references reach at most
// 32 KB back, and with that dictionary they never reach past the predecessor, so the stored piece
// is valid wherever the same predecessor comes right before it. A member is then the gzip header,
// the stored pieces in order, an empty final block, and the CRC-32 and size of the whole body,
// whose CRC is combined from the parts' own CRCs instead of being computed over the body again.
// Chaining parts this way keeps the output within a few percent of compressing the body whole;
// compressing each part independently would lose what neighbouring parts share.
//
// A part is identified by its SHA-256 and its predecessor's, the only bytes a piece depends on.
// Callers already hold those digests (they build the ETag from them), so a cached response costs
// a map lookup and a copy per part.
package gzsplice

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sync"
	"time"

	"github.com/klauspost/compress/flate"
)

// PartsWriter is implemented by a compressing response writer that can take a body as digested
// parts. The caller sets its headers first and calls WriteParts instead of Write; false means the
// writer declined and the caller must write the bytes itself.
type PartsWriter interface {
	WriteParts(parts []Part) bool
}

// Digest is a part's SHA-256. The zero Digest stands for "no predecessor".
type Digest [32]byte

// Part is one run of a body's bytes with the SHA-256 of exactly those bytes. A wrong Digest makes
// the cache serve another body's compressed bytes, so callers must pass the real digest.
type Part struct {
	Bytes  []byte
	Digest Digest
}

const (
	// window is deflate's maximum back-reference distance, the most of a predecessor a dictionary needs.
	window = 32 << 10
	// level matches Rack::Deflater's zlib default (6), so output size is comparable to the reference's.
	level = 6
	// DefaultBudget bounds each of the two cache generations. A room page's pieces are ~60 KB, so a
	// generation holds the pages of a few hundred rooms as their members see them.
	DefaultBudget = 32 << 20
	// entryOverhead charges each stored piece for its map slot, key and allocation header.
	entryOverhead = 128
)

// header is a gzip member header with no name, default flags and OS 3 (Unix), as Rack::Deflater
// writes it. Bytes 4–7 hold the mtime (WriteGzip fills them); zero means none.
var header = [10]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}

// finalBlock is an empty fixed-Huffman block with BFINAL set: it ends a stream of sync-flushed pieces.
var finalBlock = [2]byte{0x03, 0x00}

type key struct{ part, previous Digest }

type piece struct {
	deflated []byte
	crc      uint32
	// shift is Shift(len(part)), kept so joining the piece costs one multiply (see crc.go).
	shift uint32
}

// Cache holds compressed pieces in two generations: lookups promote from the old generation, and
// when the current one passes its budget it becomes the old one and the previous old one is
// dropped. That bounds memory at about twice the budget with no per-entry bookkeeping.
type Cache struct {
	mu       sync.Mutex
	current  map[key]piece
	old      map[key]piece
	used     int
	budget   int
	writers  sync.Pool
	maxPiece int
}

// New returns a cache whose generations each hold about budget bytes. Pieces larger than a
// sixteenth of the budget are compressed every time rather than stored, so one huge body cannot
// flush out the pieces every page keeps using.
func New(budget int) *Cache {
	if budget <= 0 {
		budget = DefaultBudget
	}
	return &Cache{current: map[key]piece{}, old: map[key]piece{}, budget: budget, maxPiece: budget / 16}
}

// WriteGzip writes one gzip member holding the parts' bytes in order, with mtime in its header
// (Rack::Deflater uses the response's Last-Modified; the zero time writes none). Empty parts are
// skipped; a body with no bytes still gets a valid (empty) member. The header is per call, so the
// mtime never affects which stored pieces are reused.
func (c *Cache) WriteGzip(w io.Writer, parts []Part, mtime time.Time) error {
	out := make([][]byte, 0, len(parts)+3)
	head := header
	if !mtime.IsZero() && mtime.Unix() > 0 {
		binary.LittleEndian.PutUint32(head[4:8], uint32(mtime.Unix()))
	}
	out = append(out, head[:])
	var (
		crc      uint32
		size     int
		previous Part
	)
	for _, part := range parts {
		if len(part.Bytes) == 0 {
			continue
		}
		p, err := c.piece(part, previous)
		if err != nil {
			return err
		}
		out = append(out, p.deflated)
		crc = CombineShift(crc, p.crc, p.shift)
		size += len(part.Bytes)
		previous = part
	}
	fin := finalBlock
	out = append(out, fin[:])
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:4], crc)
	binary.LittleEndian.PutUint32(trailer[4:], uint32(size))
	out = append(out, trailer[:])
	// One Write: through net/http each Write is a chunk, and every chunk costs syscalls through
	// its 4 KB buffered writer, so writing the six pieces separately tripled the syscalls per page.
	total := 0
	for _, b := range out {
		total += len(b)
	}
	buf := members.Get().(*[]byte)
	body := (*buf)[:0]
	if cap(body) < total {
		body = make([]byte, 0, total)
	}
	for _, b := range out {
		body = append(body, b...)
	}
	_, err := w.Write(body)
	if cap(body) <= maxPooledMember {
		*buf = body
		members.Put(buf)
	}
	return err
}

// maxPooledMember bounds the member buffers kept for reuse, so one huge body is not pinned.
const maxPooledMember = 1 << 20

var members = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}

func (c *Cache) piece(part, previous Part) (piece, error) {
	k := key{part.Digest, previous.Digest}
	c.mu.Lock()
	p, ok := c.current[k]
	if !ok {
		if p, ok = c.old[k]; ok {
			c.storeLocked(k, p)
		}
	}
	c.mu.Unlock()
	if ok {
		return p, nil
	}
	deflated, err := c.deflate(part.Bytes, tail(previous.Bytes))
	if err != nil {
		return piece{}, err
	}
	p = piece{deflated: deflated, crc: crc32.ChecksumIEEE(part.Bytes), shift: Shift(int64(len(part.Bytes)))}
	if len(deflated) <= c.maxPiece {
		c.mu.Lock()
		c.storeLocked(k, p)
		c.mu.Unlock()
	}
	return p, nil
}

func (c *Cache) storeLocked(k key, p piece) {
	if _, exists := c.current[k]; exists {
		return
	}
	cost := len(p.deflated) + entryOverhead
	if c.used+cost > c.budget {
		c.old, c.current, c.used = c.current, map[key]piece{}, 0
	}
	c.current[k] = p
	c.used += cost
}

func tail(b []byte) []byte { return b[max(0, len(b)-window):] }

// deflate compresses data with dict as the preset dictionary and ends it with a sync flush, so the
// result ends on a byte boundary with no final block and can be followed by another piece.
func (c *Cache) deflate(data, dict []byte) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(data)/4 + 64)
	fw, _ := c.writers.Get().(*flate.Writer)
	if fw == nil {
		var err error
		if fw, err = flate.NewWriterDict(&out, level, dict); err != nil {
			return nil, err
		}
	} else {
		fw.ResetDict(&out, dict)
	}
	defer func() { fw.Reset(io.Discard); c.writers.Put(fw) }()
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	if err := fw.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
