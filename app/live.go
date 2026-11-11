package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	ch, stop := a.Reg.View.Watch(256)
	defer stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	ctx := rq.ctx()
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
			if n.Lost {
				fmt.Fprint(rq.w, "event: lost\ndata: {}\n\n")
			}
			for _, sc := range pd.Live {
				if ev := a.liveEvent(ctx, rq.p, sc, n); ev != nil {
					b, _ := json.Marshal(ev)
					fmt.Fprintf(rq.w, "event: change\ndata: %s\n\n", b)
				}
			}
			if rc.Flush() != nil {
				return nil
			}
		}
	}
}

// liveEvent is the event for a change to what a pushed view shows, or nil:
// the change is to the view's class, the record (as it is now) matches the
// view's conditions — or, changed in a field they name, may have just left
// them — and the viewer may read it. A deletion is reported when the
// conditions don't name another record (there is nothing left to match).
func (a *App) liveEvent(ctx context.Context, p *runtime.Principal, sc ux.LiveScope, n runtime.Notice) map[string]any {
	if n.Class != sc.Class {
		return nil
	}
	ev := map[string]any{"view": sc.View, "class": n.Class, "id": n.ID, "kind": string(n.Kind), "version": n.Version}
	h, err := a.Reg.Class(sc.Class)
	if err != nil {
		return nil
	}
	pk := h.Class.PKSlot()
	if n.Kind == listener.Delete {
		for _, c := range sc.Where {
			if pk != nil && c.Field == pk.Name && c.Op == "=" && !sameID(c.Value, n.ID) {
				return nil
			}
		}
		return ev
	}
	rec, err := h.Load(ctx, n.ID)
	if err != nil || rec == nil {
		return nil
	}
	if ok, _ := a.Reg.Readable(ctx, rec, p); !ok {
		return nil
	}
	if !query.Match(query.Query{Where: sc.Where}, rec.Values) && !mayHaveLeft(sc.Where, n) {
		return nil
	}
	if len(sc.Fields) == 0 {
		ev["fields"] = n.Values
		return ev
	}
	if n.Kind == listener.Update {
		touched := false
		for _, f := range sc.Fields {
			if _, ok := n.Values[f]; ok {
				touched = true
				break
			}
		}
		if !touched {
			return nil
		}
	}
	fields := map[string]any{}
	for _, f := range sc.Fields {
		fields[f] = rec.Values[f]
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
