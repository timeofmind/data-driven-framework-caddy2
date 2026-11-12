package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/query"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// The page's event stream. GET <page>?_live=1 keeps the connection open
// (server-sent events) and sends a "change" event for every change applied
// on this node — its own or replicated — to what the page's live: push
// views show, for this viewer: the page is built as it would be served, so
// each pushed view's class and conditions are known, and every change is
// checked against them and against the viewer's right to read the record
// before it is sent. The event carries the view's name, the class, the
// record's id, the kind of change, and the fields the view follows (their
// values now) — or, with no fields named, the changed values. A "lost"
// event says changes were dropped (the stream fell behind): read again.
// The client library (dd.js) opens the stream for a page with a pushed view
// and redraws the view on every change.
//
// Changes that arrive together (one save's parts, a burst of answers) are
// gathered for coalesceFor and sent as one flush, one event per record and
// view (the newest); a view's subqueries are resolved to ids once per
// connection and again when a change names a class they read, and a change
// of membership redraws the view ("scope", id 0). What a change costs to
// check — the record's load and the viewer's right to read it — is
// remembered briefly across connections (liveMemo), so a page open on many
// screens does the work once per change, not once per screen.
func (a *App) live(rq *request, path string) error {
	p, params := a.Pages.Match(path)
	if p == nil {
		return errStatus(http.StatusNotFound, "Not found")
	}
	q := rq.r.URL.Query()
	q.Del("_live")
	ur := &ux.Request{Ctx: rq.ctx(), Viewer: rq.p, Path: params, Query: q, Base: a.cfg.BasePath, Session: rq.session}
	pd, err := a.engine.Build(ur, p)
	if err != nil {
		return err
	}
	if len(pd.Live) == 0 {
		rq.w.WriteHeader(http.StatusNoContent)
		return nil
	}
	rc := http.NewResponseController(rq.w) // flushes through Caddy's wrapping of the writer
	h := rq.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: send each event on
	rq.w.WriteHeader(http.StatusOK)
	fmt.Fprint(rq.w, "retry: 2000\n\n")
	if err := rc.Flush(); err != nil {
		return errStatus(http.StatusInternalServerError, "streaming not supported")
	}
	ctx := rq.ctx()
	follow := make([]*followed, len(pd.Live))
	for i, sc := range pd.Live {
		follow[i] = a.follow(ctx, sc)
	}
	ch, stop := a.Reg.View.Watch(256)
	defer stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ping.C:
			fmt.Fprint(rq.w, ": ping\n\n")
			if rc.Flush() != nil {
				return nil
			}
		case n := <-ch:
			batch, ok := gather(ctx, ch, n)
			if !ok {
				return nil
			}
			lost := false
			var events []map[string]any
			for _, n := range batch {
				lost = lost || n.Lost
			}
			// A change to a class a view's subqueries read: resolve them
			// again; a change of what they select redraws the view.
			for _, f := range follow {
				if f.touched(batch) && f.resolve(ctx, a.Reg) {
					events = append(events, map[string]any{"view": f.View, "class": f.Class, "id": 0, "kind": "scope"})
				}
			}
			for _, n := range batch {
				for _, f := range follow {
					if ev := a.liveEvent(ctx, rq.p, f, n); ev != nil {
						events = append(events, ev)
					}
				}
			}
			if lost {
				fmt.Fprint(rq.w, "event: lost\ndata: {}\n\n")
			}
			for _, ev := range coalesce(events) {
				b, _ := json.Marshal(ev)
				fmt.Fprintf(rq.w, "event: change\ndata: %s\n\n", b)
			}
			if rc.Flush() != nil {
				return nil
			}
		}
	}
}

// coalesceFor is how long a stream gathers the changes that follow one
// before sending them together.
const coalesceFor = 50 * time.Millisecond

// gather takes the notices that arrive within coalesceFor of the first
// (bounded, so a flood is sent on in parts); ok is false when ctx ended.
func gather(ctx context.Context, ch <-chan runtime.Notice, first runtime.Notice) (batch []runtime.Notice, ok bool) {
	batch = []runtime.Notice{first}
	t := time.NewTimer(coalesceFor)
	defer t.Stop()
	for len(batch) < 256 {
		select {
		case n := <-ch:
			batch = append(batch, n)
		case <-t.C:
			return batch, true
		case <-ctx.Done():
			return nil, false
		}
	}
	return batch, true
}

// coalesce keeps one event per view and record — the newest — in the
// order the records were first seen. A "scope" event (id 0) stands.
func coalesce(events []map[string]any) []map[string]any {
	if len(events) < 2 {
		return events
	}
	type key struct {
		view string
		id   any
	}
	at := map[key]int{}
	out := events[:0:0]
	for _, ev := range events {
		k := key{fmt.Sprint(ev["view"]), fmt.Sprint(ev["id"])}
		if i, ok := at[k]; ok && k.id != "0" {
			out[i] = ev
			continue
		}
		at[k] = len(out)
		out = append(out, ev)
	}
	return out
}

// followed is a pushed view's scope as one connection follows it: its
// conditions with every subquery resolved to the ids it selects (when the
// set is small enough; else the subquery stands and is taken as holding),
// and the classes those subqueries read.
type followed struct {
	ux.LiveScope
	where []runtime.Cond
	subs  map[string]bool
	set   string // the resolved conditions, to tell a change of membership
}

func (a *App) follow(ctx context.Context, sc ux.LiveScope) *followed {
	f := &followed{LiveScope: sc, where: sc.Where}
	if subs := runtime.SubClasses(sc.Where); len(subs) > 0 {
		f.subs = map[string]bool{}
		for _, c := range subs {
			f.subs[c] = true
		}
		f.resolve(ctx, a.Reg)
	}
	return f
}

// touched reports whether a change in the batch names a class the scope's
// subqueries read.
func (f *followed) touched(batch []runtime.Notice) bool {
	if len(f.subs) == 0 {
		return false
	}
	for _, n := range batch {
		if f.subs[n.Class] {
			return true
		}
	}
	return false
}

// resolve resolves the scope's subqueries now; it reports whether what they
// select changed since the last time.
func (f *followed) resolve(ctx context.Context, reg *runtime.Registry) bool {
	where, ok, err := reg.ResolveConds(ctx, f.Where)
	if err != nil || !ok {
		where = f.Where
	}
	set, _ := json.Marshal(where)
	changed := f.set != "" && string(set) != f.set
	f.where, f.set = where, string(set)
	return changed
}

// liveEvent is the event for a change to what a pushed view shows, or nil:
// the change is to the view's class, the record (as it is now) matches the
// view's conditions — or, changed in a field they name, may have just left
// them — and the viewer may read it. A deletion is reported when the
// conditions don't name another record (there is nothing left to match).
func (a *App) liveEvent(ctx context.Context, p *runtime.Principal, f *followed, n runtime.Notice) map[string]any {
	if n.Class != f.Class {
		return nil
	}
	ev := map[string]any{"view": f.View, "class": n.Class, "id": n.ID, "kind": string(n.Kind), "version": n.Version}
	h, err := a.Reg.Class(f.Class)
	if err != nil {
		return nil
	}
	pk := h.Class.PKSlot()
	if n.Kind == listener.Delete {
		for _, c := range f.where {
			if pk != nil && c.Field == pk.Name && c.Op == "=" && !sameID(c.Value, n.ID) {
				return nil
			}
		}
		// Only a viewer who could see the record is told it went, by its last
		// values; when memory no longer held them, the view is told something
		// changed without the id, so nothing is revealed.
		if n.Last == nil {
			ev["id"] = 0
			return ev
		}
		last := runtime.NewRecord(h.Class, n.Last)
		if !query.Match(query.Query{Where: f.where}, last.Values) && !mayHaveLeft(f.where, n) {
			return nil
		}
		if !a.liveMemo.readable(ctx, a.Reg, p, last, n.Version) {
			return nil
		}
		return ev
	}
	rec := a.liveMemo.load(ctx, h, n.ID, n.Version)
	if rec == nil {
		return nil
	}
	if !a.liveMemo.readable(ctx, a.Reg, p, rec, n.Version) {
		return nil
	}
	if !query.Match(query.Query{Where: f.where}, rec.Values) && !mayHaveLeft(f.where, n) {
		return nil
	}
	if len(f.Fields) == 0 {
		ev["fields"] = n.Values
		return ev
	}
	if n.Kind == listener.Update {
		touched := false
		for _, fld := range f.Fields {
			if _, ok := n.Values[fld]; ok {
				touched = true
				break
			}
		}
		if !touched {
			return nil
		}
	}
	fields := map[string]any{}
	for _, fld := range f.Fields {
		fields[fld] = rec.Values[fld]
	}
	ev["fields"] = fields
	return ev
}

// mayHaveLeft: an update to a field the conditions name may have taken the
// record out of them (a piece taken: captured == false no longer holds), so
// the view must still redraw; and a condition the matcher can't decide
// (a subquery) is taken as holding.
func mayHaveLeft(where []query.Cond, n runtime.Notice) bool {
	for _, c := range where {
		if c.Op == "insub" {
			return true
		}
		if n.Kind == listener.Update {
			if _, ok := n.Values[c.Field]; ok {
				return true
			}
		}
	}
	return false
}

func sameID(v any, id int64) bool {
	switch x := v.(type) {
	case int64:
		return x == id
	case int:
		return int64(x) == id
	case float64:
		return int64(x) == id
	}
	return fmt.Sprint(v) == fmt.Sprint(id)
}

// liveMemo remembers, for a moment, what checking a change cost: the record
// as loaded for a change (by class, id and version) and whether a viewer
// may read it — so the streams of many screens showing the same view do
// the work once per change. An entry lives memoFor; the memo is swept when
// it grows past memoCap or memoFor has passed since the last sweep.
type liveMemo struct {
	mu    sync.Mutex
	recs  map[string]memoEntry
	read  map[string]memoEntry
	swept time.Time
}

type memoEntry struct {
	rec *runtime.Record
	ok  bool
	at  time.Time
}

const (
	memoFor = 2 * time.Second
	memoCap = 4096
)

func (m *liveMemo) sweep(now time.Time) {
	if len(m.recs)+len(m.read) < memoCap && now.Sub(m.swept) < memoFor {
		return
	}
	m.swept = now
	for k, e := range m.recs {
		if now.Sub(e.at) > memoFor {
			delete(m.recs, k)
		}
	}
	for k, e := range m.read {
		if now.Sub(e.at) > memoFor {
			delete(m.read, k)
		}
	}
}

// load is the record of a change as this node holds it, or nil when it is
// gone (or can't be read).
func (m *liveMemo) load(ctx context.Context, h *runtime.Handle, id, version int64) *runtime.Record {
	k := fmt.Sprintf("%s/%d/%d", h.Class.Name, id, version)
	now := time.Now()
	m.mu.Lock()
	if m.recs == nil {
		m.recs, m.read = map[string]memoEntry{}, map[string]memoEntry{}
	}
	m.sweep(now)
	if e, ok := m.recs[k]; ok && now.Sub(e.at) <= memoFor {
		m.mu.Unlock()
		return e.rec
	}
	m.mu.Unlock()
	rec, err := h.Load(ctx, id)
	if err != nil {
		rec = nil
	}
	m.mu.Lock()
	m.recs[k] = memoEntry{rec: rec, at: now}
	m.mu.Unlock()
	return rec
}

// readable is whether p may read rec, as of the change's version.
func (m *liveMemo) readable(ctx context.Context, reg *runtime.Registry, p *runtime.Principal, rec *runtime.Record, version int64) bool {
	who := "-"
	if p != nil {
		who = fmt.Sprintf("%d:%t:%t", p.ID, p.Guest, p.Admin)
	}
	k := fmt.Sprintf("%s|%s/%d/%d", who, rec.Class.Name, rec.ID(), version)
	now := time.Now()
	m.mu.Lock()
	if m.recs == nil {
		m.recs, m.read = map[string]memoEntry{}, map[string]memoEntry{}
	}
	m.sweep(now)
	if e, ok := m.read[k]; ok && now.Sub(e.at) <= memoFor {
		m.mu.Unlock()
		return e.ok
	}
	m.mu.Unlock()
	ok, err := reg.Readable(ctx, rec, p)
	ok = ok && err == nil
	m.mu.Lock()
	m.read[k] = memoEntry{ok: ok, at: now}
	m.mu.Unlock()
	return ok
}
