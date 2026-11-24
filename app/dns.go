package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/cloudflare"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// DNS: the admin page of an app keeping its domain's DNS at Cloudflare. A
// cloudflare listener no class is routed to — a live app's routing/dns.yaml,
// which the builder adds at publish — is the zone itself: once an admin gives
// its zone and API token on Service Configuration and Cloudflare takes them,
// the admin area has a DNS section showing the zone's records as Cloudflare
// holds them (read on every visit) and changing them there directly.
//
// The listener's `records` are what the app needs in the zone (its mail's
// SPF and DKIM, its domain's address). The leader puts each in place once
// the token works, and notes it (FrameworkDnsAdded, dd_dns_added), so a
// record an admin deleted since is not added back but offered on the page.
//
// An app routing a class of its own to a cloudflare listener (records it
// keeps itself) has no DNS page: the listener is the app's.

const (
	dnsPath      = "/admin/dns"
	dnsPathTaken = "/admin/_dns"
	dnsEvery     = 10 * time.Minute
)

// dnsTypes are the types the page's form offers.
var dnsTypes = []string{"A", "AAAA", "CNAME", "TXT", "MX", "SRV", "CAA", "NS"}

func (a *App) dnsAdded() *runtime.Handle { return a.Reg.MustClass("FrameworkDnsAdded") }

// dnsListenerNames are the routing's cloudflare listeners no class is routed
// to, sorted.
func dnsListenerNames(reg *runtime.Registry) []string {
	if reg.Routing == nil {
		return nil
	}
	used := map[string]bool{}
	for _, rt := range reg.Routing.Routes {
		used[rt.Source] = true
		for _, d := range rt.FlowsTo {
			used[d.Listener] = true
		}
	}
	var out []string
	for name, cfg := range reg.Routing.Listeners {
		if cfg.Type == cloudflare.Type && !used[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// dnsState is what this node last learnt of each zone: nil for a token that
// works, else why not.
type dnsState struct {
	mu   sync.Mutex
	errs map[string]error
	kick chan struct{}
}

func (a *App) dnsSet(name string, err error) {
	a.dns.mu.Lock()
	a.dns.errs[name] = err
	a.dns.mu.Unlock()
}

// dnsWorking says whether some zone's token works: the DNS section is in the
// menu then.
func (a *App) dnsWorking() bool {
	if a.dns == nil {
		return false
	}
	a.dns.mu.Lock()
	defer a.dns.mu.Unlock()
	for _, err := range a.dns.errs {
		if err == nil {
			return true
		}
	}
	return false
}

// adminSectionShown keeps the DNS section out of the menu until a token works.
func (a *App) adminSectionShown(u string) bool {
	return a.dnsPath == "" || u != a.dnsPath || a.dnsWorking()
}

// dnsListener is the zone's listener, open, or why it is not.
func (a *App) dnsListener(name string) (*cloudflare.Listener, error) {
	if err := a.Reg.Unconfigured(name); err != nil {
		return nil, err
	}
	l, ok := listener.Unwrap(a.Reg.Listeners[name]).(*cloudflare.Listener)
	if !ok {
		return nil, fmt.Errorf("listener %s is not open", name)
	}
	return l, nil
}

// startDNS keeps the zones: at once, every dnsEvery, and when a zone's
// settings are saved (kickDNS).
func (a *App) startDNS(ctx context.Context) {
	go func() {
		for {
			wait := dnsEvery
			if !a.keepDNS(ctx) {
				wait = dnsRetry // no leader yet after a start, or Cloudflare failed: soon again
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			case <-a.dns.kick:
				t.Stop()
			}
		}
	}()
}

// dnsRetry is how soon the zones are kept again when they could not be.
var dnsRetry = time.Minute

func (a *App) kickDNS() {
	if a.dns == nil {
		return
	}
	select {
	case a.dns.kick <- struct{}{}:
	default:
	}
}

// keepDNS checks each zone's token, and on the leader puts the records the
// app needs in place. It reports whether that was done for every zone whose
// token works (false: not the leader yet, or Cloudflare failed).
func (a *App) keepDNS(ctx context.Context) bool {
	done := true
	for _, name := range dnsListenerNames(a.Reg) {
		l, err := a.dnsListener(name)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			err = l.Verify(cctx)
			switch {
			case err != nil:
			case !a.Reg.Leading():
				done = false // the leader does it; a node just started may not know it yet
			default:
				if msgs, ferr := a.fulfilDNS(cctx, name, l); ferr != nil {
					done = false
					a.log.Warn("DNS: the records the app needs were not all put in place", "listener", name, "zone", l.Zone(), "error", ferr)
				} else if len(msgs) > 0 {
					a.log.Warn("DNS: put the records the app needs in place", "listener", name, "zone", l.Zone(), "done", msgs)
				}
			}
			cancel()
		}
		a.dnsSet(name, err)
	}
	return done
}

// fulfilDNS puts in place each needed record not put there before.
func (a *App) fulfilDNS(ctx context.Context, name string, l *cloudflare.Listener) ([]string, error) {
	if len(l.Needed()) == 0 {
		return nil, nil
	}
	zone, err := l.List(ctx)
	if err != nil {
		return nil, err
	}
	added, err := a.dnsNoted(ctx, name)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, st := range l.Check(zone) {
		if st.State == "outside" || added[dnsKey(st)] {
			continue
		}
		if st.State == "missing" || st.State == "differs" {
			msg, err := l.Fulfil(ctx, st, zone)
			if err != nil {
				return done, fmt.Errorf("%s %s: %w", st.Type, st.FQDN, err)
			}
			if msg != "" {
				done = append(done, msg)
			}
		} else if st.State != "in place" {
			continue // a name pointing elsewhere: for an admin to decide, on the page
		}
		if err := a.dnsNote(ctx, name, st, "the server"); err != nil {
			return done, err
		}
	}
	return done, nil
}

func dnsKey(st cloudflare.NeedState) string {
	return st.Type + " " + st.FQDN + " " + st.Content
}

// dnsNoted are the needed records put in place for a zone before.
func (a *App) dnsNoted(ctx context.Context, name string) (map[string]bool, error) {
	recs, err := a.dnsAdded().List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", name)}, Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range recs {
		out[r.String("type")+" "+r.String("name")+" "+r.String("content")] = true
	}
	return out, nil
}

func (a *App) dnsNote(ctx context.Context, name string, st cloudflare.NeedState, by string) error {
	h := a.dnsAdded()
	return h.Save(ctx, h.New(map[string]any{"listener": name, "type": st.Type, "name": st.FQDN, "content": st.Content, "added_by": by}))
}

// The page.

type dnsRow struct {
	ID, Type, Name, Content string
	TTL, Priority           string
	Proxied, ProxiedShown   bool
	Needed                  string // why the app needs it, when it does
}

type dnsNeedRow struct {
	cloudflare.NeedState
	Class   string // ok, todo, off
	Fixable bool
}

type dnsZoneView struct {
	Listener, Zone, Problem string
	// Done is what the page just put in place, FulfilError why it could not,
	// NeedsNote how the zone stands with what the app needs, in words.
	Done         []string
	FulfilError  string
	NeedsNote    string
	Needs        []dnsNeedRow
	NeedProblems []string
	Records      []dnsRow
}

type dnsEdit struct {
	Listener, ID, Type, Name, Content, Priority string
	TTL                                         int
	Proxied                                     bool
}

type dnsDelete struct {
	Listener string
	dnsRow
}

type dnsPage struct {
	Message, Error   string
	Action, Services string
	Zones            []dnsZoneView
	Types            []string
	Edit             *dnsEdit
	Delete           *dnsDelete
}

// EditFor is the form of a zone: the record being edited, else a new one.
func (p *dnsPage) EditFor(name string) dnsEdit {
	if p.Edit != nil && p.Edit.Listener == name {
		return *p.Edit
	}
	return dnsEdit{Type: "A"}
}

func (a *App) dnsAdmin(rq *request) error {
	if ok, err := a.adminSection(rq, a.dnsPath); !ok {
		return err
	}
	ctx := rq.ctx()
	pg := &dnsPage{Action: a.url(a.dnsPath), Services: a.url(servicesPath), Types: dnsTypes}
	if rq.r.Method == http.MethodPost {
		msg, err := a.saveDNS(rq)
		var he *httpError
		switch {
		case errors.As(err, &he):
			return err
		case err != nil:
			pg.Error = err.Error()
			if op := rq.r.PostFormValue("op"); op == "add" || op == "save" {
				// the form as sent, to put right
				e := dnsEditFromForm(rq.r)
				if op == "save" {
					e.ID = rq.r.PostFormValue("id")
				}
				pg.Edit = &e
			}
		default:
			http.Redirect(rq.w, rq.r, a.url(a.dnsPath)+"?done="+url.QueryEscape(msg), http.StatusSeeOther)
			return nil
		}
	} else if done := rq.r.URL.Query().Get("done"); done != "" {
		pg.Message = done
	}
	q := rq.r.URL.Query()
	editL, editID, _ := strings.Cut(q.Get("edit"), ":")
	delL, delID, _ := strings.Cut(q.Get("delete"), ":")
	for _, name := range dnsListenerNames(a.Reg) {
		zv := dnsZoneView{Listener: name}
		l, err := a.dnsListener(name)
		if err != nil {
			zv.Problem = "Not set up yet: " + err.Error()
			pg.Zones = append(pg.Zones, zv)
			continue
		}
		zv.Zone = l.Zone()
		zone, err := l.List(ctx)
		a.dnsSet(name, err)
		if err != nil {
			zv.Problem = err.Error()
			pg.Zones = append(pg.Zones, zv)
			continue
		}
		noted, _ := a.dnsNoted(ctx, name)
		if rq.r.Method == http.MethodGet && dnsPending(l.Check(zone), noted) {
			// The token works and a record the app needs was never put in
			// place: do it now, rather than show it missing until the
			// leader's next pass.
			done, ferr := a.fulfilDNS(ctx, name, l)
			zv.Done = done
			if ferr != nil {
				zv.FulfilError = "A record the app needs could not be put in place: " + ferr.Error()
			}
			if len(done) > 0 {
				a.log.Warn("DNS: put the records the app needs in place", "listener", name, "zone", l.Zone(), "done", done)
				for i, d := range done {
					zv.Done[i] = "Just " + d + " at Cloudflare."
				}
			}
			if z, err := l.List(ctx); err == nil {
				zone = z
			}
			noted, _ = a.dnsNoted(ctx, name)
		}
		needs := l.Check(zone)
		zv.NeedsNote = dnsNeedsNote(needs, noted)
		why := map[string]string{}
		for _, st := range needs {
			row := dnsNeedRow{NeedState: st}
			switch st.State {
			case "in place":
				row.Class = "ok"
				why[st.ID] = orWord(st.Purpose, "the app")
			case "missing", "differs":
				row.Class, row.Fixable = "todo", true
				if st.ID != "" {
					why[st.ID] = orWord(st.Purpose, "the app")
				}
				if noted[dnsKey(st)] && row.Detail == "" {
					row.Detail = "changed or deleted at Cloudflare since it was put in place; " + map[bool]string{true: "Add", false: "Update"}[st.State == "missing"] + " puts it back"
				}
			default:
				row.Class = "off"
			}
			zv.Needs = append(zv.Needs, row)
		}
		zv.NeedProblems = l.Problems()
		for _, r := range zone {
			row := dnsRow{ID: r.ID, Type: r.Type, Name: r.Name, Content: r.Content, TTL: "auto", Needed: why[r.ID]}
			if r.TTL > 1 {
				row.TTL = strconv.Itoa(r.TTL)
			}
			if r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME" {
				row.ProxiedShown, row.Proxied = true, r.Proxied != nil && *r.Proxied
			}
			if r.Priority != nil && (r.Type == "MX" || r.Type == "SRV" || r.Type == "URI") {
				row.Priority = strconv.Itoa(*r.Priority)
			}
			zv.Records = append(zv.Records, row)
			if name == editL && r.ID == editID && pg.Edit == nil {
				pg.Edit = &dnsEdit{Listener: name, ID: r.ID, Type: r.Type, Name: r.Name, Content: r.Content, TTL: r.TTL, Proxied: row.Proxied, Priority: row.Priority}
			}
			if name == delL && r.ID == delID {
				pg.Delete = &dnsDelete{Listener: name, dnsRow: row}
			}
		}
		pg.Zones = append(pg.Zones, zv)
	}
	a.site.Render(a.webRequest(rq), "dns", "DNS", pg)
	return nil
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func dnsEditFromForm(r *http.Request) dnsEdit {
	ttl, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("ttl")))
	return dnsEdit{Listener: r.PostFormValue("listener"), Type: strings.ToUpper(strings.TrimSpace(r.PostFormValue("type"))),
		Name: strings.TrimSpace(r.PostFormValue("name")), Content: strings.TrimSpace(r.PostFormValue("content")),
		TTL: ttl, Proxied: r.PostFormValue("proxied") != "", Priority: strings.TrimSpace(r.PostFormValue("priority"))}
}

// saveDNS carries out the page's form at Cloudflare: add, save (an edit),
// delete, or fulfil (put a needed record in place).
func (a *App) saveDNS(rq *request) (string, error) {
	ctx := rq.ctx()
	if err := rq.r.ParseForm(); err != nil {
		return "", err
	}
	name := rq.r.PostFormValue("listener")
	known := false
	for _, n := range dnsListenerNames(a.Reg) {
		known = known || n == name
	}
	if !known {
		return "", errStatus(http.StatusBadRequest, "no such zone")
	}
	l, err := a.dnsListener(name)
	if err != nil {
		return "", err
	}
	actor := rq.p.Name
	if a.auth != nil {
		if acc, err := a.auth.Load(ctx, rq.p.ID); err == nil {
			actor = acc.String(a.auth.Class.Auth.Login)
		}
	}
	op := rq.r.PostFormValue("op")
	switch op {
	case "add", "save":
		e := dnsEditFromForm(rq.r)
		if !contains(dnsTypes, e.Type) {
			return "", fmt.Errorf("%q is not a type this page adds", e.Type)
		}
		rec := cloudflare.Record{Type: e.Type, Name: e.Name, Content: e.Content, TTL: e.TTL, Proxied: &e.Proxied}
		if e.Priority != "" {
			p, err := strconv.Atoi(e.Priority)
			if err != nil || p < 0 || p > 65535 {
				return "", errors.New("the priority is a number from 0 to 65535")
			}
			rec.Priority = &p
		}
		id := ""
		if op == "save" {
			id = rq.r.PostFormValue("id")
			if id == "" {
				return "", errors.New("which record?")
			}
		}
		got, err := l.Put(ctx, id, rec)
		if err != nil {
			return "", err
		}
		a.log.Info("DNS: a record "+map[string]string{"add": "added", "save": "changed"}[op], "actor", actor, "zone", l.Zone(), "type", got.Type, "name", got.Name)
		return fmt.Sprintf("%s %s %s at Cloudflare.", got.Type, got.Name, map[string]string{"add": "added", "save": "saved"}[op]), nil
	case "delete":
		id := rq.r.PostFormValue("id")
		if id == "" {
			return "", errors.New("which record?")
		}
		if err := l.Delete(ctx, id); err != nil {
			return "", err
		}
		a.log.Info("DNS: a record deleted", "actor", actor, "zone", l.Zone(), "id", id)
		return "Deleted from Cloudflare.", nil
	case "fulfil":
		i, err := strconv.Atoi(rq.r.PostFormValue("need"))
		zone, lerr := l.List(ctx)
		if lerr != nil {
			return "", lerr
		}
		states := l.Check(zone)
		if err != nil || i < 0 || i >= len(states) {
			return "", errors.New("which record?")
		}
		st := states[i]
		msg, err := l.Fulfil(ctx, st, zone)
		if err != nil {
			return "", err
		}
		added, _ := a.dnsNoted(ctx, name)
		if !added[dnsKey(st)] {
			if err := a.dnsNote(ctx, name, st, actor); err != nil {
				return "", err
			}
		}
		a.log.Info("DNS: a needed record put in place", "actor", actor, "zone", l.Zone(), "done", msg)
		if msg == "" {
			return "Already in place.", nil
		}
		return strings.ToUpper(msg[:1]) + msg[1:] + " at Cloudflare.", nil
	}
	return "", errStatus(http.StatusBadRequest, "unknown action")
}

// dnsPending says whether a record the app needs was never put in place
// though it could be.
func dnsPending(needs []cloudflare.NeedState, noted map[string]bool) bool {
	for _, st := range needs {
		if (st.State == "missing" || st.State == "differs") && !noted[dnsKey(st)] {
			return true
		}
	}
	return false
}

// dnsNeedsNote says in words how the zone stands with what the app needs.
func dnsNeedsNote(needs []cloudflare.NeedState, noted map[string]bool) string {
	var in, back, taken, outside int
	for _, st := range needs {
		switch st.State {
		case "in place":
			in++
		case "missing", "differs":
			back++
		case "taken":
			taken++
		case "outside":
			outside++
		}
	}
	var out []string
	switch {
	case in == len(needs):
		return "Every record the app needs is in place."
	case in > 0:
		out = append(out, fmt.Sprintf("%d of the %d records the app needs %s in place.", in, len(needs), map[bool]string{true: "is", false: "are"}[in == 1]))
	}
	if back > 0 {
		out = append(out, "A record the app needs was changed or deleted here or at Cloudflare since it was put in place, so it isn't put back by itself: its button puts it back.")
	}
	if taken > 0 {
		out = append(out, "A name the app needs points elsewhere: the app's record is not put over it. Edit or delete the record there below if the name should be the app's.")
	}
	if outside > 0 {
		out = append(out, "A record the app needs is not in this zone: it belongs in another zone.")
	}
	return strings.Join(out, " ")
}
