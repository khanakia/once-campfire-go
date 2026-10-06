package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basecamp/once-campfire-go/internal/database"
	"github.com/basecamp/once-campfire-go/internal/front"
)

// A recorded page and a plain buffered body sent through Rack::Deflater must decode to exactly the
// identity bytes, with the same ETag, on the first (compressing) and repeated (spliced) requests.
func TestDeflatedResponsesMatchIdentity(t *testing.T) {
	app, _, _, user := testApp(t)
	ctx := context.Background()
	rooms, _ := app.DB.Rooms(ctx, user.ID)
	var list []database.Message
	for i := range 30 {
		message, err := app.DB.CreateMessage(ctx, user.ID, rooms[0].ID, "gz", strings.Repeat("<p>shared markup &amp; text</p>", 20+i), "text")
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, message)
	}
	fragment, err := app.messageList(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	layout := strings.Repeat("<nav>layout</nav>", 3000)
	handlers := map[string]http.Handler{
		"recorded": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buffered := &responseBuffer{ResponseWriter: w}
			writeRecorded(buffered, 200, layout+"\x00m\x00"+layout[:5000], "\x00m\x00", fragment)
			buffered.finish(r)
		}),
		// messages#index: fresh_when sets Last-Modified, which becomes the gzip header's mtime.
		"last-modified": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buffered := &responseBuffer{ResponseWriter: w}
			w = buffered
			w.Header().Set("Last-Modified", "Mon, 02 Mar 2026 15:55:00 GMT")
			writeRecorded(w, 200, layout+"\x00m\x00", "\x00m\x00", fragment)
			buffered.finish(r)
		}),
		"whole": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buffered := &responseBuffer{ResponseWriter: w}
			buffered.Write([]byte(layout))
			buffered.finish(r)
		}),
		"empty": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buffered := &responseBuffer{ResponseWriter: w}
			buffered.finish(r)
		}),
	}
	for name, h := range handlers {
		handler := front.Deflate(h)
		identity := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/", nil)
		request.Header.Set("Accept-Encoding", "identity")
		handler.ServeHTTP(identity, request)
		for round := range 3 {
			gz := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			handler.ServeHTTP(gz, request)
			if gz.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("%s: not gzipped", name)
			}
			reader, err := gzip.NewReader(bytes.NewReader(gz.Body.Bytes()))
			if err != nil {
				t.Fatalf("%s round %d: %v", name, round, err)
			}
			if want, _ := http.ParseTime(identity.Header().Get("Last-Modified")); !reader.ModTime.Equal(want) && !(want.IsZero() && reader.ModTime.Unix() == 0) {
				t.Fatalf("%s round %d: gzip mtime %v, want %v", name, round, reader.ModTime, want)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("%s round %d: %v", name, round, err)
			}
			if !bytes.Equal(body, identity.Body.Bytes()) {
				t.Fatalf("%s round %d: gzip body differs from identity", name, round)
			}
			if gz.Header().Get("ETag") != identity.Header().Get("ETag") || gz.Header().Get("Content-Length") != "" {
				t.Fatalf("%s round %d: headers %v vs %v", name, round, gz.Header(), identity.Header())
			}
		}
	}
}
