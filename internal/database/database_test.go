package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.sqlite3"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func TestSchemaAndMessageTransaction(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	source, err := os.ReadFile("../../reference/crates/db/src/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != schema {
		t.Fatal("schema diverged from pinned reference")
	}
	u, err := d.Setup(ctx, "David", "david@example.test", "digest")
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := d.Rooms(ctx, u.ID)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms: %v %v", rooms, err)
	}
	if _, err = d.Setup(ctx, "Other", "other@example.test", "digest"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("repeated setup: %v", err)
	}
	m, err := d.CreateMessage(ctx, u.ID, rooms[0].ID, "", "<p>running dogs</p>", "running dogs")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := d.Messages(ctx, rooms[0].ID, 0)
	if err != nil || len(messages) != 1 || messages[0].ID != m.ID || messages[0].Body != "<p>running dogs</p>" {
		t.Fatalf("messages: %v %v", messages, err)
	}
	hits, err := d.Search(ctx, u.ID, "run")
	if err != nil || len(hits) != 1 {
		t.Fatalf("porter search: %v %v", hits, err)
	}
	if _, err = d.CreateMessage(ctx, u.ID, 12345, "", "hidden", "hidden"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized write: %v", err)
	}
	if hits, err = d.Search(ctx, u.ID+1, "run"); err != nil || len(hits) != 0 {
		t.Fatalf("private search leaked: %v %v", hits, err)
	}
	// An FTS failure must roll back the message and its rich text together.
	if _, err = d.Write.Exec("DROP TABLE message_search_index"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CreateMessage(ctx, u.ID, rooms[0].ID, "", "rollback", "rollback"); err == nil {
		t.Fatal("expected failed index write")
	}
	var count int
	if err = d.Read.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial write: %d %v", count, err)
	}
}
func TestSessionRevocation(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	u, err := d.Setup(ctx, "User", "u@test", "digest")
	if err != nil {
		t.Fatal(err)
	}
	token, err := d.StartSession(ctx, u.ID, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.SessionUser(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatal(got, err)
	}
	if _, err = d.Write.Exec("UPDATE users SET status=2 WHERE id=?", u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SessionUser(ctx, token); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("banned user session accepted: %v", err)
	}
}

// Authentication reads the session's activity with its user and refreshes it at most once an
// hour, like Rails' Authentication concern; the second refresh in the same hour is a no-op.
func TestSessionActivityRefresh(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	u, err := d.Setup(ctx, "User", "u@test", "digest")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return start }
	token, err := d.StartSession(ctx, u.ID, "old agent", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	got, active, err := d.SessionUserActivity(ctx, token)
	if err != nil || got.ID != u.ID || !active.Equal(start) {
		t.Fatal(got, active, err)
	}
	d.Now = func() time.Time { return start.Add(30 * time.Minute) }
	if refreshed, err := d.RefreshSessionAt(ctx, token, active, "new agent", "10.0.0.1"); err != nil || refreshed {
		t.Fatal("refreshed within the hour", refreshed, err)
	}
	later := start.Add(2 * time.Hour)
	d.Now = func() time.Time { return later }
	if refreshed, err := d.RefreshSessionAt(ctx, token, active, "new agent", "10.0.0.1"); err != nil || !refreshed {
		t.Fatal("not refreshed after an hour", refreshed, err)
	}
	// A caller holding the stale activity cannot refresh twice: the UPDATE re-checks it.
	if refreshed, err := d.RefreshSessionAt(ctx, token, active, "other", "10.0.0.2"); err != nil || refreshed {
		t.Fatal("refreshed twice", refreshed, err)
	}
	if _, active, err = d.SessionUserActivity(ctx, token); err != nil || !active.Equal(later) {
		t.Fatal(active, err)
	}
	var agent string
	if err = d.Read.QueryRow("SELECT user_agent FROM sessions WHERE token=?", token).Scan(&agent); err != nil || agent != "new agent" {
		t.Fatal(agent, err)
	}
	if refreshed, err := d.RefreshSession(ctx, token, "x", "y"); err != nil || refreshed {
		t.Fatal("RefreshSession refreshed a fresh session", refreshed, err)
	}
}

// The sidebar's batched member query must match RoomMembers room by room, including order
// (it decides how a direct room's name joins its members), and must leave out other rooms.
func TestDirectRoomMembersMatchesRoomMembers(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	owner, err := d.Setup(ctx, "Owner", "owner@test", "digest")
	if err != nil {
		t.Fatal(err)
	}
	var others []int64
	for _, name := range []string{"Zed", "Amy", "Bob"} {
		u, err := d.CreateUser(ctx, name, strings.ToLower(name)+"@test", "digest", "", 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, u.ID)
	}
	var direct []int64
	for _, members := range [][]int64{{others[2], others[0]}, {others[1]}, {others[0], others[1], others[2]}} {
		room, err := d.CreateRoom(ctx, owner.ID, "Rooms::Direct", "", members)
		if err != nil {
			t.Fatal(err)
		}
		direct = append(direct, room.ID)
	}
	open, err := d.CreateRoom(ctx, owner.ID, "Rooms::Open", "Open", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A direct room the owner is not in.
	if _, err = d.CreateRoom(ctx, others[0], "Rooms::Direct", "", []int64{others[1]}); err != nil {
		t.Fatal(err)
	}
	batched, err := d.DirectRoomMembers(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batched) != len(direct) {
		t.Fatalf("rooms %v, want only the owner's %d direct rooms", len(batched), len(direct))
	}
	if _, ok := batched[open.ID]; ok {
		t.Fatal("open room included")
	}
	for _, room := range direct {
		want, err := d.RoomMembers(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) != len(batched[room]) {
			t.Fatalf("room %d: %d members, want %d", room, len(batched[room]), len(want))
		}
		for i := range want {
			if want[i] != batched[room][i] {
				t.Fatalf("room %d member %d: %+v, want %+v", room, i, batched[room][i], want[i])
			}
		}
	}
}

// SearchReferences must find exactly Search's messages, in the same order, with the fields the
// fragment cache keys on, and must not leak other people's rooms.
func TestSearchReferencesMatchSearch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	u, err := d.Setup(ctx, "User", "u@test", "digest")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateUser(ctx, "Other", "other@test", "digest", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := d.Rooms(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	private, err := d.CreateRoom(ctx, u.ID, "Rooms::Closed", "Private", []int64{other.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Leave only the other member, so the searcher cannot reach this room.
	if _, err = d.Write.Exec("DELETE FROM memberships WHERE room_id=? AND user_id=?", private.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	for i := range 130 { // more than the 100-hit limit
		d.Now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		body := fmt.Sprintf("coffee number %d", i)
		if _, err = d.CreateMessage(ctx, u.ID, rooms[0].ID, "", "<p>"+body+"</p>", body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.CreateMessage(ctx, other.ID, private.ID, "", "<p>coffee secret</p>", "coffee secret"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"coffee", "number 7", "\"quoted\" coffee", "   ", "nothing-matches"} {
		full, err := d.Search(ctx, u.ID, query)
		if err != nil {
			t.Fatal(err)
		}
		refs, err := d.SearchReferences(ctx, u.ID, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != len(full) {
			t.Fatalf("%q: %d references, %d messages", query, len(refs), len(full))
		}
		for i := range full {
			if refs[i].ID != full[i].ID || refs[i].RoomID != full[i].RoomID || !refs[i].UpdatedAt.Equal(full[i].UpdatedAt) {
				t.Fatalf("%q hit %d: %+v, want %+v", query, i, refs[i], full[i])
			}
			if refs[i].RoomID == private.ID {
				t.Fatal("search leaked another member's room")
			}
		}
	}
	if refs, _ := d.SearchReferences(ctx, u.ID, "coffee"); len(refs) != 100 {
		t.Fatalf("%d references, want the 100-hit limit", len(refs))
	}
}
func TestPendingMigrationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sqlite3")
	d, err := Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Write.Exec("DELETE FROM schema_migrations WHERE version=?", migrations[0]); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if d, err = Open(path, 1); err == nil {
		d.Close()
		t.Fatal("missing migration accepted")
	} else if !strings.Contains(err.Error(), "pending migration") {
		t.Fatal(err)
	}
}
