package web

import (
	"bytes"
	"crypto/sha256"
	"github.com/basecamp/once-campfire-go/internal/database"
	"html/template"
	"testing"
)

func TestRoomShellPreservesBytesAndRequestData(t *testing.T) {
	app, _, _, user := testApp(t)
	base := page{User: user, Room: database.Room{ID: 1, Name: "Room & <name>", Type: "Rooms::Open"}, Chat: true, Screen: "room", Origin: "https://example.test", LoadedAt: "1234567890", MessagesHTML: template.HTML("<div>message one</div>")}
	check := func(p page) {
		t.Helper()
		var expected bytes.Buffer
		if err := app.templates.ExecuteTemplate(&expected, "room", p); err != nil {
			t.Fatal(err)
		}
		shell, err := app.roomShell(p)
		if err != nil {
			t.Fatal(err)
		}
		actual := shell.before + string(p.MessagesHTML) + shell.after
		if actual != expected.String() {
			t.Fatal("cached room shell differs from uncached template")
		}
		if shell.beforeDigest != sha256.Sum256([]byte(shell.before)) || shell.afterDigest != sha256.Sum256([]byte(shell.after)) {
			t.Fatal("room shell digests do not match its parts")
		}
	}
	check(base)
	check(base)
	changes := map[string]func(*page){
		"timestamp and messages": func(p *page) { p.LoadedAt = "1234567999"; p.MessagesHTML = "<p>new message</p>" },
		"user":                   func(p *page) { p.User.Name = "Other <person>"; p.User.ID++ },
		"role":                   func(p *page) { p.User.Role = 0 },
		"room":                   func(p *page) { p.Room.Name = "Renamed" },
		"flash":                  func(p *page) { p.Notice = "Saved" },
		"error":                  func(p *page) { p.Error = "Failed" },
		"styles":                 func(p *page) { p.CustomStyles = "<style>body{color:red}</style>" },
		"origin":                 func(p *page) { p.Origin = "https://other.test" },
		"frame":                  func(p *page) { p.Frame = true },
		"invitation":             func(p *page) { p.Invitation = true; p.JoinCode = "new-code" },
		"stream":                 func(p *page) { p.Stream = "new-stream" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) { p := base; change(&p); check(p); check(base) })
	}
	// Oversized entries bypass the bounded cache but must still render correctly.
	app.fragments = newFragmentCache(1)
	check(base)
}
