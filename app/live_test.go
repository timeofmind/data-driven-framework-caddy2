package app

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A page with a live: push view streams a change event for every change to
// what the view shows that the viewer may read — the owner's own private
// route reaches the owner, not another member; a public one reaches both.
func TestLiveStream(t *testing.T) {
	_, srv := newTestApp(t)
	ann, ben := newBrowser(t, srv), newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	ben.signup("ben@example.com", "Ben")
	open := func(b *browser) (chan string, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/c/routes?_live=1", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("the stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		events := make(chan string, 16)
		go func() {
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			var ev, data string
			for sc.Scan() {
				line := sc.Text()
				switch {
				case strings.HasPrefix(line, "event: "):
					ev = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					data = strings.TrimPrefix(line, "data: ")
				case line == "" && ev != "":
					events <- ev + " " + data
					ev, data = "", ""
				}
			}
		}()
		return events, cancel
	}
	annEvents, stopAnn := open(ann)
	defer stopAnn()
	benEvents, stopBen := open(ben)
	defer stopBen()
	expect := func(events chan string, what string, want ...string) string {
		t.Helper()
		select {
		case ev := <-events:
			for _, w := range want {
				if !strings.Contains(ev, w) {
					t.Errorf("%s: event %q lacks %q", what, ev, w)
				}
			}
			return ev
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no event", what)
		}
		return ""
	}
	none := func(events chan string, what string) {
		t.Helper()
		select {
		case ev := <-events:
			t.Errorf("%s: unexpected event %q", what, ev)
		case <-time.After(300 * time.Millisecond):
		}
	}
	// Ann's private route: her stream says so, Ben's stays quiet.
	loc := ann.create("/c/routes/new", url.Values{"name": {"Hill"}, "distance_km": {"5"}})
	expect(annEvents, "Ann's private route", "change", `"view":"list"`, `"class":"Route"`, `"kind":"create"`, `"name":"Hill"`)
	none(benEvents, "Ben and Ann's private route")
	// Made public: an update, to both.
	id := loc[strings.LastIndex(loc, "/")+1:]
	ann.post("/c/routes/"+id+"/edit", url.Values{"name": {"Hill"}, "distance_km": {"5"}, "visibility": {"public"}})
	expect(annEvents, "Ann's route made public", `"kind":"update"`, `"visibility":"public"`)
	expect(benEvents, "Ben sees the route made public", `"kind":"update"`, `"id":`+id)
	// Deleted: both hear.
	ann.post("/c/routes/"+id, url.Values{"_csrf": {ann.csrf("/c/routes/new")}, "_view": {"detail1"}, "_op": {"delete"}})
	expect(annEvents, "the deletion", `"kind":"delete"`)
	expect(benEvents, "the deletion, for Ben", `"kind":"delete"`)
	// A page without a pushed view has no stream.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/c/walks?_live=1", nil)
	resp, err := ann.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("a page without a pushed view: %d", resp.StatusCode)
	}
}
