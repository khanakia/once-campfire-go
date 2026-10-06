package gzsplice

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func part(s string) Part { return Part{Bytes: []byte(s), Digest: sha256.Sum256([]byte(s))} }

// gunzip decodes with the standard library, which checks the CRC-32 and size trailer.
func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r.Multistream(false)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(r); len(rest) != 0 {
		t.Fatal("trailing data after the member")
	}
	return string(out)
}

func TestCombineMatchesCRCOfConcatenation(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, sizes := range [][2]int{{0, 0}, {0, 5}, {5, 0}, {1, 1}, {3, 70000}, {40000, 1}, {123456, 654321}} {
		a, b := make([]byte, sizes[0]), make([]byte, sizes[1])
		for i := range a {
			a[i] = byte(rng.Uint32())
		}
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		want := crc32.ChecksumIEEE(append(append([]byte{}, a...), b...))
		if got := Combine(crc32.ChecksumIEEE(a), crc32.ChecksumIEEE(b), int64(len(b))); got != want {
			t.Fatalf("sizes %v: got %08x want %08x", sizes, got, want)
		}
	}
}

func layout(user int) string {
	return strings.Repeat(fmt.Sprintf(`<nav class="sidebar" data-user="%d">room links</nav>`, user), 900)
}

func messages(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `<div class="message" id="message_%d"><p>Message body %d with shared markup</p></div>`, i, i*i)
	}
	return b.String()
}

func TestSplicedMemberDecodesToTheBody(t *testing.T) {
	c := New(0)
	cases := [][]Part{
		nil,
		{part("")},
		{part("only")},
		{part(layout(1)), part(messages(500)), part("</body></html>")},
		{part(layout(2)), part(messages(500)), part("</body></html>")}, // same fragment, new predecessor
		{part(layout(1)), part(""), part(messages(500)), part("</body></html>")},
	}
	for i, parts := range cases {
		var want strings.Builder
		for _, p := range parts {
			want.Write(p.Bytes)
		}
		for round := range 2 { // second round is served from the cache
			var out bytes.Buffer
			if err := c.WriteGzip(&out, parts, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if got := gunzip(t, out.Bytes()); got != want.String() {
				t.Fatalf("case %d round %d: decoded body differs", i, round)
			}
		}
	}
}

// The mtime only changes the header: the same pieces serve both, and gzip readers see the stamp.
func TestMtimeIsPerMember(t *testing.T) {
	c := New(0)
	parts := []Part{part(layout(1)), part(messages(50))}
	stamp := time.Date(2026, 3, 2, 15, 55, 0, 0, time.UTC)
	var plain, stamped bytes.Buffer
	c.WriteGzip(&plain, parts, time.Time{})
	c.WriteGzip(&stamped, parts, stamp)
	r, err := gzip.NewReader(bytes.NewReader(stamped.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !r.ModTime.Equal(stamp) {
		t.Fatalf("mtime %v, want %v", r.ModTime, stamp)
	}
	if gunzip(t, stamped.Bytes()) != gunzip(t, plain.Bytes()) || !bytes.Equal(plain.Bytes()[10:], stamped.Bytes()[10:]) {
		t.Fatal("mtime changed more than the header")
	}
}

func TestCachedPiecesAreReused(t *testing.T) {
	c := New(0)
	parts := []Part{part(layout(1)), part(messages(300)), part("tail")}
	var first, second bytes.Buffer
	c.WriteGzip(&first, parts, time.Time{})
	stored := len(c.current)
	c.WriteGzip(&second, parts, time.Time{})
	if len(c.current) != stored || stored != 3 {
		t.Fatalf("pieces stored: %d then %d, want 3 both times", stored, len(c.current))
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("cache hit changed the member")
	}
}

// Chaining against the predecessor must keep the member close to whole-body gzip; compressing each
// part on its own would lose what the parts share.
func TestSplicedSizeStaysCloseToWholeBody(t *testing.T) {
	parts := []Part{part(layout(1)), part(messages(500)), part(layout(1))}
	var whole bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&whole, level)
	for _, p := range parts {
		zw.Write(p.Bytes)
	}
	zw.Close()
	var spliced bytes.Buffer
	New(0).WriteGzip(&spliced, parts, time.Time{})
	if limit := whole.Len() * 115 / 100; spliced.Len() > limit {
		t.Fatalf("spliced %d bytes, whole-body gzip %d", spliced.Len(), whole.Len())
	}
}

func TestGenerationsBoundMemory(t *testing.T) {
	c := New(64 << 10)
	for i := range 200 {
		var out bytes.Buffer
		c.WriteGzip(&out, []Part{part(fmt.Sprintf("body %d %s", i, messages(20)))}, time.Time{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used > c.budget {
		t.Fatalf("current generation uses %d of %d", c.used, c.budget)
	}
	total := 0
	for _, p := range c.old {
		total += len(p.deflated) + entryOverhead
	}
	if total > c.budget {
		t.Fatalf("old generation holds %d of %d", total, c.budget)
	}
}

func TestOversizedPiecesAreNotStored(t *testing.T) {
	c := New(1 << 10)
	var out bytes.Buffer
	big := part(messages(2000))
	if err := c.WriteGzip(&out, []Part{big}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(c.current) != 0 {
		t.Fatal("oversized piece was cached")
	}
	if gunzip(t, out.Bytes()) != string(big.Bytes) {
		t.Fatal("oversized body decoded wrongly")
	}
}

func BenchmarkRoomPage(b *testing.B) {
	c := New(0)
	parts := []Part{part(layout(1)), part(messages(4000)), part(layout(1)[:20000])}
	size := 0
	for _, p := range parts {
		size += len(p.Bytes)
	}
	b.SetBytes(int64(size))
	for b.Loop() {
		c.WriteGzip(io.Discard, parts, time.Time{})
	}
}
