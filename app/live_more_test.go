package app

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// A list with style: thread is drawn as a conversation: one item per row
// carrying the id and the row's flags, the first field as the viewer's own
// line, the rest as replies — each line named by data-dd-field.
func TestThreadPresentation(t *testing.T) {
	_, srv := newTestApp(t)
	ann := newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	loc := ann.create("/c/routes/new", url.Values{"name": {"Hill"}, "distance_km": {"5"}})
	id := loc[strings.LastIndex(loc, "/")+1:]
	code, body, _ := ann.get("/c/routes/thread")
	if code != 200 {
		t.Fatalf("thread page: %d", code)
	}
	for _, want := range []string{
		`<section class="view list thread" data-view="thread" data-dd-push>`,
		`<ol class="thread">`,
		`<li data-dd-id="` + id + `" data-dd-visibility="private">`,
		`<p class="mine" data-dd-field="name">`,
		`<p class="theirs num" data-dd-field="distance_km">`,
		`<p class="theirs" data-dd-field="visibility"><span class="tag tag-private">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("thread page lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "<table") {
		t.Error("a thread is not a table")
	}
}

// coalesce keeps one event per view and record — the newest — and every
// scope event.
func TestCoalesce(t *testing.T) {
	ev := func(view string, id int64, kind string, v int64) map[string]any {
		return map[string]any{"view": view, "id": id, "kind": kind, "version": v}
	}
	in := []map[string]any{
		ev("a", 1, "create", 1), ev("b", 1, "create", 1), ev("a", 1, "update", 2),
		{"view": "a", "id": 0, "kind": "scope"}, ev("a", 2, "create", 3), ev("a", 1, "delete", 4),
		{"view": "a", "id": 0, "kind": "scope"},
	}
	out := coalesce(in)
	got := make([]string, 0, len(out))
	for _, e := range out {
		got = append(got, fmt.Sprintf("%s:%v:%s", e["view"], e["id"], e["kind"]))
	}
	want := "a:1:delete b:1:create a:0:scope a:2:create a:0:scope"
	if strings.Join(got, " ") != want {
		t.Errorf("coalesced %v; want %s", got, want)
	}
}

// The memo remembers a record and a viewer's right to read it for a
// moment: a second check of the same change costs no load.
func TestLiveMemo(t *testing.T) {
	a, srv := newTestApp(t)
	ann := newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	loc := ann.create("/c/routes/new", url.Values{"name": {"Hill"}, "distance_km": {"5"}})
	id := loc[strings.LastIndex(loc, "/")+1:]
	rid, _ := strconv.ParseInt(id, 10, 64)
	ctx := context.Background()
	h := a.Reg.MustClass("Route")
	m := &liveMemo{}
	rec := m.load(ctx, h, rid, 1)
	if rec == nil || rec.String("name") != "Hill" {
		t.Fatalf("load: %v", rec)
	}
	owner := &runtime.Principal{ID: rec.Int("member_id"), Name: "Ann"}
	other := &runtime.Principal{ID: rec.Int("member_id") + 1, Name: "Ben"}
	if !m.readable(ctx, a.Reg, owner, rec, 1) {
		t.Error("the owner may read her route")
	}
	if m.readable(ctx, a.Reg, other, rec, 1) {
		t.Error("another member may not read a private route")
	}
	if m.readable(ctx, a.Reg, nil, rec, 1) {
		t.Error("a visitor may not read a private route")
	}
	if len(m.recs) != 1 || len(m.read) != 3 {
		t.Errorf("memo holds %d records and %d answers; want 1 and 3", len(m.recs), len(m.read))
	}
	// The same change again is answered from the memo, even once the
	// record is gone from the registry.
	if err := h.Delete(ctx, rid); err != nil {
		t.Fatal(err)
	}
	if rec := m.load(ctx, h, rid, 1); rec == nil {
		t.Error("the memo forgot the record within memoFor")
	}
	// Past memoFor an entry is swept.
	for k, e := range m.recs {
		e.at = time.Now().Add(-2 * memoFor)
		m.recs[k] = e
	}
	m.swept = time.Time{}
	if rec := m.load(ctx, h, rid, 1); rec != nil {
		t.Error("a stale entry was answered")
	}
}
