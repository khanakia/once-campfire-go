package database

import (
	"context"
	"testing"
)

// Reads ignore their context's cancellation (see detached): a request whose client has gone
// still completes the reads it started, and no per-query watcher goroutine is needed.
func TestReadsIgnoreCancellation(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var n int
	if err := d.Read.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("cancelled QueryRowContext: %d %v", n, err)
	}
	rows, err := d.Read.QueryContext(ctx, "SELECT 1 UNION ALL SELECT 2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if rows.Err() != nil || count != 2 {
		t.Fatalf("cancelled QueryContext: %d rows, %v", count, rows.Err())
	}
}
