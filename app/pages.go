package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

const pageSize = 25

func (a *App) requireLogin(rq *request) error {
	if rq.p == nil {
		return errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	return nil
}

// ---- home ----

type homeSection struct {
	Label string
	URL   string
	Table table
}

func (a *App) home(rq *request) error {
	if rq.r.Method != http.MethodGet {
		return errStatus(http.StatusMethodNotAllowed, "Method not allowed")
	}
	var sections []homeSection
	today := time.Now().Format("2006-01-02")
	for _, hs := range a.homeSections() {
		c := a.Model.Class(hs.Class)
		h := a.Reg.MustClass(c.Name)
		if hs.Mine && (rq.p == nil || ownerOf(c) == "") {
			continue
		}
		q := runtime.Query{Limit: hs.Limit}
		if q.Limit <= 0 {
			q.Limit = 5
		}
		if hs.Mine {
			q.Where = append(q.Where, runtime.Eq(ownerOf(c), rq.p.ID))
		}
		q, ok := h.Scope(q, rq.p)
		if !ok {
			continue
		}
		recs, err := a.upcomingOrRecent(rq.ctx(), h, q, today)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			continue
		}
		t, err := a.buildTable(rq.ctx(), rq.p, c, recs, "")
		if err != nil {
			return err
		}
		u := a.classURL(c)
		if hs.Mine {
			u += "?mine=1"
		}
		label := hs.Title
		if label == "" {
			label = plural(c.EffectiveLabel())
		}
		sections = append(sections, homeSection{Label: label, URL: u, Table: t})
	}
	a.render(rq, "home", "", map[string]any{"Sections": sections, "Description": a.Model.Description})
	return nil
}

// homeSections returns the manifest's home sections, or by default one per
// top-level class with a calendar slot.
func (a *App) homeSections() []HomeSection {
	if len(a.man.Home) > 0 {
		return a.man.Home
	}
	var out []HomeSection
	for _, c := range a.Model.ConcreteClasses() {
		if a.navigable(c) && c.Calendar != "" {
			out = append(out, HomeSection{Class: c.Name})
		}
	}
	return out
}

// upcomingOrRecent lists rows dated today or later (soonest first), or, when
// there are none, the most recent ones.
func (a *App) upcomingOrRecent(ctx context.Context, h *runtime.Handle, q runtime.Query, today string) ([]*runtime.Record, error) {
	c := h.Class
	if c.Calendar == "" {
		return h.List(ctx, q)
	}
	up := q
	up.Where = append(append([]runtime.Cond(nil), q.Where...), runtime.Cond{Field: c.Calendar, Op: ">=", Value: today})
	up.OrderBy = c.Calendar + " asc"
	recs, err := h.List(ctx, up)
	if err != nil || len(recs) > 0 {
		return recs, err
	}
	q.OrderBy = c.Calendar + " desc"
	return h.List(ctx, q)
}

// ---- list ----

type listData struct {
	Class      *model.Class
	Label      string
	Desc       string
	Table      table
	Total      int
	Page       int
	Pages      int
	PrevURL    string
	NextURL    string
	NewURL     string
	Query      string
	Mine       bool
	MineURL    string
	AllURL     string
	HasOwner   bool
	Calendar   string
	Filters    []link
	FilterNote string
	Facets     []facet
	RankNote   string
	Search     bool // free-text search offered (off with the class's search: false)
}

func (a *App) listQuery(rq *request) (runtime.Query, []link, string, error) {
	c := rq.handle.Class
	vals := rq.r.URL.Query()
	var q runtime.Query
	var active []link
	note := ""
	for key, vs := range vals {
		s := c.Slot(key)
		if s == nil || len(vs) == 0 || vs[0] == "" {
			continue
		}
		rel := c.BelongsToVia(key)
		if rel == nil && s.Type != "enum" && s.Type != "boolean" && s.Type != "date" {
			continue
		}
		if s.Hidden && rel == nil {
			continue
		}
		v, err := runtime.Coerce(s, vs[0])
		if err != nil {
			continue
		}
		q.Where = append(q.Where, runtime.Cond{Field: key, Op: "=", Value: v})
		label := s.EffectiveLabel() + ": " + runtime.DisplayValue(s, v)
		if rel != nil {
			if th, err := a.Reg.Class(rel.Target); err == nil {
				if t, err := th.Load(rq.ctx(), asInt(v)); err == nil {
					if ok, _ := a.Reg.Readable(rq.ctx(), t, rq.p); ok {
						label = rel.EffectiveLabel() + ": " + t.Title()
					}
				}
			}
		}
		without := cloneValues(vals)
		without.Del(key)
		without.Del("page")
		active = append(active, link{Label: label, URL: a.classURL(c) + encodeQuery(without)})
	}
	if search := strings.TrimSpace(vals.Get("q")); search != "" && !c.NoSearch {
		if ds := c.DisplaySlot(); ds != nil && (ds.Type == "string" || ds.Type == "text") {
			q.Where = append(q.Where, runtime.Cond{Field: ds.Name, Op: "like", Value: "%" + search + "%"})
		}
	}
	if vals.Get("mine") == "1" {
		if rq.p == nil {
			return q, nil, "", errStatus(http.StatusUnauthorized, "Please sign in first")
		}
		if o := ownerOf(c); o != "" {
			q.Where = append(q.Where, runtime.Eq(o, rq.p.ID))
			note = "Showing only yours"
		}
	}
	if ob := vals.Get("order"); ob != "" {
		f := strings.Fields(ob)
		if s := c.Slot(f[0]); s != nil && !s.Hidden {
			q.OrderBy = ob
		}
	}
	chips, err := a.applyFilters(rq, &q, vals)
	if err != nil {
		return q, nil, "", err
	}
	return q, append(active, chips...), note, nil
}

func (a *App) list(rq *request) error {
	if rq.r.Method != http.MethodGet {
		return errStatus(http.StatusMethodNotAllowed, "Method not allowed")
	}
	c := rq.handle.Class
	if c.Calendar != "" && rq.r.URL.Query().Get("view") == "calendar" {
		return a.classCalendar(rq)
	}
	q, filters, note, err := a.listQuery(rq)
	if err != nil {
		return err
	}
	q, ok := rq.handle.Scope(q, rq.p)
	if !ok {
		if rq.p == nil {
			return errStatus(http.StatusUnauthorized, "Please sign in first")
		}
		return errForbidden
	}
	q, rankNote, err := a.rankByUsage(rq, q, rq.r.URL.Query())
	if err != nil {
		return err
	}
	total, err := rq.handle.Count(rq.ctx(), q)
	if err != nil {
		return err
	}
	pageNo, _ := strconv.Atoi(rq.r.URL.Query().Get("page"))
	pages := max(1, (total+pageSize-1)/pageSize)
	pageNo = min(max(pageNo, 1), pages)
	q.Limit, q.Offset = pageSize, (pageNo-1)*pageSize
	recs, err := rq.handle.List(rq.ctx(), q)
	if err != nil {
		return err
	}
	t, err := a.buildTable(rq.ctx(), rq.p, c, recs, "")
	if err != nil {
		return err
	}
	vals := rq.r.URL.Query()
	// Rows remember the filter values they were found with (for usage ranking).
	if ctxVals, _ := searchContext(c, vals); ctxVals != nil {
		for i := range t.Rows {
			t.Rows[i].URL += "?" + ctxVals.Encode()
		}
	}
	facets, err := a.facets(rq, vals, q)
	if err != nil {
		return err
	}
	d := listData{
		Class: c, Label: plural(c.EffectiveLabel()), Desc: c.Description, Table: t, Total: total,
		Page: pageNo, Pages: pages, Query: vals.Get("q"), Mine: vals.Get("mine") == "1",
		HasOwner: ownerOf(c) != "" && rq.p != nil, Filters: filters, FilterNote: note,
		Facets: facets, RankNote: rankNote, Search: !c.NoSearch,
	}
	if pageNo > 1 {
		v := cloneValues(vals)
		v.Set("page", strconv.Itoa(pageNo-1))
		d.PrevURL = a.classURL(c) + encodeQuery(v)
	}
	if pageNo < pages {
		v := cloneValues(vals)
		v.Set("page", strconv.Itoa(pageNo+1))
		d.NextURL = a.classURL(c) + encodeQuery(v)
	}
	mv := cloneValues(vals)
	mv.Del("page")
	mv.Set("mine", "1")
	d.MineURL = a.classURL(c) + encodeQuery(mv)
	mv.Del("mine")
	d.AllURL = a.classURL(c) + encodeQuery(mv)
	if c.Calendar != "" {
		cv := cloneValues(vals)
		cv.Del("page")
		cv.Set("view", "calendar")
		d.Calendar = a.classURL(c) + encodeQuery(cv)
	}
	if runtime.CanCreate(c, rq.p) || (rq.p == nil && c.Auth == nil && runtime.WriteRule(c) != "nobody") {
		d.NewURL = a.classURL(c) + "/new"
	}
	a.render(rq, "list", d.Label, d)
	return nil
}

// ---- detail ----

type detailData struct {
	Class    *model.Class
	Label    string
	Title    string
	Fields   []field
	Children []child
	EditURL  string
	DelURL   string
	ListURL  string
	Private  bool
	IsMe     bool
	Actions  []link
}

func (a *App) loadReadable(rq *request) (*runtime.Record, error) {
	rec, err := rq.handle.Load(rq.ctx(), rq.id)
	if err != nil {
		return nil, err
	}
	ok, err := a.Reg.Readable(rq.ctx(), rec, rq.p)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return nil, err
	}
	if !ok {
		return nil, errNotFound // don't reveal that a private record exists
	}
	return rec, nil
}

func (a *App) detail(rq *request) error {
	if rq.r.Method != http.MethodGet {
		return errStatus(http.StatusMethodNotAllowed, "Method not allowed")
	}
	rec, err := a.loadReadable(rq)
	if err != nil {
		return err
	}
	c := rec.Class
	fields, err := a.detailFields(rq.ctx(), rq.p, rec)
	if err != nil {
		return err
	}
	d := detailData{Class: c, Label: c.EffectiveLabel(), Title: rec.Title(), Fields: fields, ListURL: a.classURL(c)}
	if !a.navigable(c) {
		if pr, err := a.Reg.MustClass(c.Name).Parent(rq.ctx(), rec, c.Access.Parent); err == nil {
			d.ListURL = a.recordURL(pr.Class, pr.ID())
		}
	}
	writable, err := a.Reg.Writable(rq.ctx(), rec, rq.p)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if writable {
		d.EditURL = a.recordURL(c, rec.ID()) + "/edit"
		d.DelURL = a.recordURL(c, rec.ID()) + "/delete"
	}
	if c.Access != nil && len(c.Access.PublicWhen) > 0 && !runtime.CanRead(rec, nil) {
		d.Private = true
	}
	d.IsMe = c.Auth != nil && rq.p != nil && rq.p.ID == rec.ID()
	_, rq.search = searchContext(c, rq.r.URL.Query())
	d.Actions = append(d.Actions, a.recordActions(rq, rec, rq.search)...)
	for _, rel := range c.AllRelationships() {
		if !rel.IsHasMany() {
			continue
		}
		ch, err := a.childSection(rq, rec, rel, writable)
		if err != nil {
			return err
		}
		if ch != nil {
			d.Children = append(d.Children, *ch)
		}
	}
	if d.IsMe && a.uploads != nil {
		d.Actions = append(d.Actions, link{Label: firstNonEmpty(a.man.Uploads.Title, "Upload"), URL: a.url("/upload")})
	}
	a.render(rq, "detail", d.Title, d)
	return nil
}

func (a *App) childSection(rq *request, parent *runtime.Record, rel *model.Relationship, parentWritable bool) (*child, error) {
	h, err := a.Reg.Class(rel.Target)
	if err != nil {
		return nil, nil
	}
	via := rel.Via
	if via == "" {
		return nil, nil
	}
	q, ok := h.Scope(runtime.Query{Where: []runtime.Cond{runtime.Eq(via, parent.ID())}, OrderBy: rel.OrderBy}, rq.p)
	if !ok {
		return nil, nil
	}
	total, err := h.Count(rq.ctx(), q)
	if err != nil {
		return nil, err
	}
	ranked := rel.Rank != nil
	q.Limit = 50
	if ranked {
		q.Limit = 1000
	}
	recs, err := h.List(rq.ctx(), q)
	if err != nil {
		return nil, err
	}
	// A ranked relationship is a leaderboard: numbered, and with best_per only
	// the first (best, by order_by) row per value of that slot.
	if ranked && rel.Rank.BestPer != "" {
		seen := map[string]bool{}
		best := recs[:0:0]
		for _, r := range recs {
			if k := r.String(rel.Rank.BestPer); !seen[k] {
				seen[k] = true
				best = append(best, r)
			}
		}
		recs, total = best, len(best)
		if len(recs) > 100 {
			recs = recs[:100]
		}
	}
	t, err := a.buildTable(rq.ctx(), rq.p, h.Class, recs, via)
	if err != nil {
		return nil, err
	}
	if ranked {
		t.Columns = append([]string{"#"}, t.Columns...)
		for i := range t.Rows {
			t.Rows[i].Cells = append([]cell{{Text: strconv.Itoa(i + 1), Class: "num rank", NoLink: true}}, t.Rows[i].Cells...)
		}
	}
	ch := &child{Label: firstNonEmpty(rel.Label, plural(h.Class.EffectiveLabel())), Table: t}
	if total > len(recs) && a.navigable(h.Class) {
		ch.More = fmt.Sprintf("%s?%s=%d", a.classURL(h.Class), via, parent.ID())
	}
	// "Add" is offered when the viewer may create the child and link it here:
	// children with their own owner need only read access to the parent;
	// ownerless children need write access to it.
	// Profile pages (the auth class) only list; records are added elsewhere.
	profile := parent.Class.Auth != nil
	canLink := parentWritable || (ownerOf(h.Class) != "" && canLinkChild(h.Class, via, parent, rq.p))
	if !profile && canLink && runtime.CanCreate(h.Class, rq.p) && !a.alreadyHasOne(rq, h, via, parent.ID()) {
		ch.AddURL = fmt.Sprintf("%s/new?%s=%d", a.classURL(h.Class), via, parent.ID())
		if len(a.Model.UsesByLink(h.Class.Name, via)) > 0 {
			ch.AddURL = withSearch(ch.AddURL, rq.search)
		}
		ch.AddLbl = firstNonEmpty(rel.AddLabel, "Add "+strings.ToLower(h.Class.EffectiveLabel()))
		if !runtime.AddAllowed(rel, parent) {
			ch.AddURL = ""
		}
	}
	if len(recs) == 0 {
		if profile || ch.AddURL == "" {
			return nil, nil
		}
		ch.Empty = "None yet."
	}
	return ch, nil
}

// alreadyHasOne reports whether a unique (via, owner) constraint means the
// viewer cannot add another child here (e.g. a second registration).
func (a *App) alreadyHasOne(rq *request, h *runtime.Handle, via string, parentID int64) bool {
	owner := ownerOf(h.Class)
	if owner == "" || rq.p == nil {
		return false
	}
	for _, k := range h.Class.UniqueConstraints() {
		if len(k.Columns) == 2 && ((k.Columns[0] == via && k.Columns[1] == owner) || (k.Columns[1] == via && k.Columns[0] == owner)) {
			n, err := h.Count(rq.ctx(), runtime.Query{Where: []runtime.Cond{runtime.Eq(via, parentID), runtime.Eq(owner, rq.p.ID)}})
			return err == nil && n > 0
		}
	}
	return false
}

// ---- create / edit / delete ----

type formData struct {
	Label   string
	Title   string
	Groups  []formGroup
	Action  string
	Cancel  string
	Submit  string
	Message string
	Rules   *formRules // schema rules applied in the browser, if the class has any
}

func (a *App) create(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	c := rq.handle.Class
	if !runtime.CanCreate(c, rq.p) {
		return errForbidden
	}
	rec := rq.handle.New(nil)
	// Prefill from the query string (e.g. ?plan_id=3&date=2026-11-05).
	prefill := map[string]any{}
	for k, vs := range rq.r.URL.Query() {
		if s := c.Slot(k); s != nil && len(vs) > 0 && s.Writable() {
			prefill[k] = vs[0]
		}
	}
	rec.Assign(prefill, ownerOf(c))
	a.Reg.ApplyDefaults(rq.ctx(), rec) // defaults computed from links, e.g. the parent's sport
	var errs runtime.FieldErrors
	if rq.r.Method == http.MethodPost {
		if err := rq.r.ParseForm(); err != nil {
			return errStatus(http.StatusBadRequest, "Bad form")
		}
		errs = a.applyForm(rq, rec)
		if errs == nil {
			if err := a.saveChecked(rq, rec); err != nil {
				var fe runtime.FieldErrors
				if !errors.As(err, &fe) {
					return err
				}
				errs = fe
			} else {
				a.logLinkUses(rq.ctx(), rq.p, rec, ids([]string{rq.r.URL.Query().Get(searchParam)}))
				http.Redirect(rq.w, rq.r, a.recordURL(c, rec.ID()), http.StatusSeeOther)
				return nil
			}
		}
	}
	groups, err := a.formFor(rq.ctx(), rq.p, c, rec, errs)
	if err != nil {
		return err
	}
	title := "New " + strings.ToLower(c.EffectiveLabel())
	a.render(rq, "form", title, formData{
		Label: c.EffectiveLabel(), Title: title, Groups: groups, Action: rq.r.URL.RequestURI(),
		Cancel: a.classURL(c), Submit: "Save", Message: generalError(errs), Rules: a.formRules(rq.ctx(), rec),
	})
	return nil
}

func (a *App) edit(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	rec, err := a.loadReadable(rq)
	if err != nil {
		return err
	}
	if ok, err := a.Reg.Writable(rq.ctx(), rec, rq.p); err != nil || !ok {
		return errForbidden
	}
	c := rec.Class
	var include []string
	if c.Auth != nil {
		include = []string{c.Auth.Login}
	}
	var errs runtime.FieldErrors
	if rq.r.Method == http.MethodPost {
		if err := rq.r.ParseForm(); err != nil {
			return errStatus(http.StatusBadRequest, "Bad form")
		}
		errs = a.applyForm(rq, rec, include...)
		if errs == nil {
			if err := a.saveChecked(rq, rec); err != nil {
				var fe runtime.FieldErrors
				if !errors.As(err, &fe) {
					return err
				}
				errs = fe
			} else {
				http.Redirect(rq.w, rq.r, a.recordURL(c, rec.ID()), http.StatusSeeOther)
				return nil
			}
		}
	}
	groups, err := a.formFor(rq.ctx(), rq.p, c, rec, errs, include...)
	if err != nil {
		return err
	}
	title := "Edit " + rec.Title()
	a.render(rq, "form", title, formData{
		Label: c.EffectiveLabel(), Title: title, Groups: groups, Action: rq.r.URL.Path,
		Cancel: a.recordURL(c, rec.ID()), Submit: "Save changes", Message: generalError(errs), Rules: a.formRules(rq.ctx(), rec),
	})
	return nil
}

func (a *App) remove(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	rec, err := a.loadReadable(rq)
	if err != nil {
		return err
	}
	if ok, err := a.Reg.Writable(rq.ctx(), rec, rq.p); err != nil || !ok {
		return errForbidden
	}
	c := rec.Class
	if rq.r.Method == http.MethodPost {
		self := c.Auth != nil && rq.p.ID == rec.ID()
		if err := rq.handle.Delete(rq.ctx(), rec.ID()); err != nil {
			return errStatus(http.StatusConflict, "This can't be deleted while other records depend on it.")
		}
		if self {
			a.endAllSessions(rq.ctx(), rec.ID())
			a.endSession(rq)
			http.Redirect(rq.w, rq.r, a.url("/"), http.StatusSeeOther)
			return nil
		}
		http.Redirect(rq.w, rq.r, a.classURL(c), http.StatusSeeOther)
		return nil
	}
	msg := fmt.Sprintf("Delete %s “%s”? This cannot be undone.", strings.ToLower(c.EffectiveLabel()), rec.Title())
	var cascades []string
	for _, rel := range c.AllRelationships() {
		if rel.IsHasMany() {
			if t := a.Model.Class(rel.Target); t != nil {
				if br := t.BelongsToVia(rel.Via); br != nil && br.OnDelete == "cascade" {
					cascades = append(cascades, strings.ToLower(firstNonEmpty(rel.Label, plural(t.EffectiveLabel()))))
				}
			}
		}
	}
	if c.Auth != nil {
		msg = "Delete your account? This cannot be undone."
		if len(cascades) > 0 {
			msg += " Your " + strings.Join(cascades, ", ") + " will be permanently deleted too."
		}
	} else if len(cascades) > 0 {
		msg += " Its " + strings.Join(cascades, ", ") + " will be deleted too."
	}
	a.render(rq, "confirm", "Delete "+rec.Title(), map[string]any{
		"Message": msg, "Action": rq.r.URL.Path, "Cancel": a.recordURL(c, rec.ID()), "Submit": "Delete",
	})
	return nil
}

// applyForm copies posted values onto rec. Unchecked checkboxes arrive as
// absent fields, so boolean form slots are set false when missing.
func (a *App) applyForm(rq *request, rec *runtime.Record, include ...string) runtime.FieldErrors {
	c := rec.Class
	in := map[string]any{}
	allowed := map[string]bool{}
	for _, s := range c.FormSlots() {
		allowed[s.Name] = true
	}
	for _, n := range include {
		allowed[n] = true
	}
	for _, s := range c.AllSlots() {
		if c.BelongsToVia(s.Name) != nil && s.Writable() {
			allowed[s.Name] = true
		}
	}
	delete(allowed, ownerOf(c))
	for name := range allowed {
		s := c.Slot(name)
		if s == nil {
			continue
		}
		if vs, ok := rq.r.PostForm[name]; ok {
			in[name] = vs[len(vs)-1]
		} else if s.Type == "boolean" {
			in[name] = false
		}
	}
	if c.Auth != nil {
		if v, ok := in[c.Auth.Login].(string); ok {
			in[c.Auth.Login] = strings.ToLower(strings.TrimSpace(v))
		}
	}
	errs := rec.Assign(in, ownerOf(c))
	if errs == nil {
		return nil
	}
	// Report every problem at once: add rule violations for the fields that did convert.
	for k, v := range rec.Validate() {
		if _, seen := errs[k]; !seen && k != ownerOf(c) {
			if _, posted := in[k]; posted {
				errs[k] = v
			}
		}
	}
	return errs
}

// saveChecked fills the owner, verifies every FK points at something the
// principal may link to, then saves, translating constraint errors.
func (a *App) saveChecked(rq *request, rec *runtime.Record) error {
	c := rec.Class
	if o := ownerOf(c); o != "" && !rec.Persisted() && c.Auth == nil {
		rec.Values[o] = rq.p.ID
	}
	if errs := a.checkLinks(rq.ctx(), rq.p, rec); errs != nil {
		return errs
	}
	h := a.Reg.MustClass(c.Name)
	if err := h.Save(rq.ctx(), rec); err != nil {
		return friendlyDBError(err)
	}
	return nil
}

func (a *App) checkLinks(ctx context.Context, p *runtime.Principal, rec *runtime.Record) runtime.FieldErrors {
	c := rec.Class
	errs := runtime.FieldErrors{}
	for _, rel := range c.AllRelationships() {
		if !rel.IsBelongsTo() || rel.Via == ownerOf(c) {
			continue
		}
		id := rec.Int(rel.Via)
		if id == 0 {
			continue
		}
		h, err := a.Reg.Class(rel.Target)
		if err != nil {
			continue
		}
		target, err := h.Load(ctx, id)
		if err != nil {
			errs[rel.Via] = "does not exist"
			continue
		}
		ok, _ := a.Reg.Readable(ctx, target, p)
		switch {
		case !ok:
			errs[rel.Via] = "does not exist"
		case isAccessParent(c, rel.Via) && ownerOf(c) == "":
			if w, _ := a.Reg.Writable(ctx, target, p); !w {
				errs[rel.Via] = "is not yours to change"
			}
		case !isAccessParent(c, rel.Via) && ownLinksOnly(rel, target.Class) && !p.Admin:
			if target.Int(ownerOf(target.Class)) != p.ID {
				errs[rel.Via] = "must be one of your own"
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func friendlyDBError(err error) error {
	var fe runtime.FieldErrors
	if errors.As(err, &fe) {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate") {
		return runtime.FieldErrors{"": "That already exists."}
	}
	if strings.Contains(msg, "foreign key") {
		return runtime.FieldErrors{"": "A linked record doesn't exist (or no longer exists)."}
	}
	return err
}

func generalError(errs runtime.FieldErrors) string {
	if errs == nil {
		return ""
	}
	if m := errs[""]; m != "" {
		return m
	}
	return "Please fix the highlighted fields."
}

// ---- calendars ----

type calDay struct {
	Date    string
	Day     int
	InMonth bool
	Today   bool
	Items   []calItem
	AddURL  string
}

type calItem struct {
	Text, URL, Class string
}

type calendarData struct {
	Title   string
	Month   string
	Weeks   [][]calDay
	PrevURL string
	NextURL string
	ListURL string
	Legend  []link
	Mine    bool
}

func monthParam(rq *request) time.Time {
	now := time.Now()
	m, err := time.Parse("2006-01", rq.r.URL.Query().Get("month"))
	if err != nil {
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return m
}

// calendarGrid returns the Monday-start weeks covering the month.
func calendarGrid(month time.Time) [][]calDay {
	monday := func(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }
	start := month.AddDate(0, 0, -monday(month))
	last := month.AddDate(0, 1, -1)
	end := last.AddDate(0, 0, 6-monday(last))
	today := time.Now().Format("2006-01-02")
	var weeks [][]calDay
	for d := start; !d.After(end); {
		var week []calDay
		for i := 0; i < 7; i++ {
			ds := d.Format("2006-01-02")
			week = append(week, calDay{Date: ds, Day: d.Day(), InMonth: d.Month() == month.Month(), Today: ds == today})
			d = d.AddDate(0, 0, 1)
		}
		weeks = append(weeks, week)
	}
	return weeks
}

func (a *App) fillCalendar(rq *request, weeks [][]calDay, h *runtime.Handle, extra []runtime.Cond, cls string) error {
	c := h.Class
	from, to := weeks[0][0].Date, weeks[len(weeks)-1][6].Date
	q := runtime.Query{Where: append([]runtime.Cond{
		{Field: c.Calendar, Op: ">=", Value: from},
		{Field: c.Calendar, Op: "<=", Value: to + " 23:59:59"},
	}, extra...), Limit: 1000, OrderBy: c.Calendar + " asc"}
	q, ok := h.Scope(q, rq.p)
	if !ok {
		return nil
	}
	recs, err := h.List(rq.ctx(), q)
	if err != nil {
		return err
	}
	byDate := map[string][]calItem{}
	for _, r := range recs {
		d := r.String(c.Calendar)
		if len(d) > 10 {
			d = d[:10]
		}
		// Every enum value becomes a CSS class (<slot>-<value>) for the theme to style.
		ic := cls + " class-" + strings.ToLower(c.Name)
		for _, s := range c.AllSlots() {
			if s.Type == "enum" && r.String(s.Name) != "" {
				ic += " " + s.Name + "-" + r.String(s.Name)
			}
		}
		byDate[d] = append(byDate[d], calItem{Text: runtime.CalendarText(r), URL: a.recordURL(c, r.ID()), Class: ic})
	}
	for wi := range weeks {
		for di := range weeks[wi] {
			weeks[wi][di].Items = append(weeks[wi][di].Items, byDate[weeks[wi][di].Date]...)
		}
	}
	return nil
}

func (a *App) monthNav(rq *request, base string, month time.Time) (string, string) {
	v := cloneValues(rq.r.URL.Query())
	v.Set("month", month.AddDate(0, -1, 0).Format("2006-01"))
	prev := base + encodeQuery(v)
	v.Set("month", month.AddDate(0, 1, 0).Format("2006-01"))
	return prev, base + encodeQuery(v)
}

func (a *App) classCalendar(rq *request) error {
	c := rq.handle.Class
	month := monthParam(rq)
	weeks := calendarGrid(month)
	var extra []runtime.Cond
	mine := rq.r.URL.Query().Get("mine") == "1"
	if mine {
		if rq.p == nil {
			return errStatus(http.StatusUnauthorized, "Please sign in first")
		}
		if o := ownerOf(c); o != "" {
			extra = append(extra, runtime.Eq(o, rq.p.ID))
		}
	}
	if err := a.fillCalendar(rq, weeks, rq.handle, extra, "item"); err != nil {
		return err
	}
	if runtime.CanCreate(c, rq.p) {
		for wi := range weeks {
			for di := range weeks[wi] {
				weeks[wi][di].AddURL = fmt.Sprintf("%s/new?%s=%s", a.classURL(c), c.Calendar, weeks[wi][di].Date)
			}
		}
	}
	d := calendarData{Title: plural(c.EffectiveLabel()), Month: month.Format("January 2006"), Weeks: weeks, Mine: mine}
	d.PrevURL, d.NextURL = a.monthNav(rq, a.classURL(c), month)
	lv := cloneValues(rq.r.URL.Query())
	lv.Del("view")
	lv.Del("month")
	d.ListURL = a.classURL(c) + encodeQuery(lv)
	a.render(rq, "calendar", d.Title, d)
	return nil
}

// calendar shows every owned calendar class for the signed-in person on one
// month grid — the training planner view: planned workouts next to what was
// actually logged.
func (a *App) calendar(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	month := monthParam(rq)
	weeks := calendarGrid(month)
	d := calendarData{Title: "Your calendar", Month: month.Format("January 2006"), Weeks: weeks, Mine: true}
	var addClass *model.Class
	for _, c := range a.Model.ConcreteClasses() {
		if c.Calendar == "" || ownerOf(c) == "" {
			continue
		}
		cls := fmt.Sprintf("item series-%d", len(d.Legend)%6)
		if err := a.fillCalendar(rq, weeks, a.Reg.MustClass(c.Name), []runtime.Cond{runtime.Eq(ownerOf(c), rq.p.ID)}, cls); err != nil {
			return err
		}
		d.Legend = append(d.Legend, link{Label: plural(c.EffectiveLabel()), URL: a.classURL(c) + "?mine=1"})
		if addClass == nil && runtime.CanCreate(c, rq.p) {
			addClass = c
		}
	}
	if addClass != nil {
		for wi := range weeks {
			for di := range weeks[wi] {
				weeks[wi][di].AddURL = fmt.Sprintf("%s/new?%s=%s", a.classURL(addClass), addClass.Calendar, weeks[wi][di].Date)
			}
		}
	}
	d.PrevURL, d.NextURL = a.monthNav(rq, a.url("/calendar"), month)
	a.render(rq, "calendar", d.Title, d)
	return nil
}

// ---- accounts ----

func (a *App) signup(rq *request) error {
	c := a.auth.Class
	au := c.Auth
	rec := a.auth.New(nil)
	var errs runtime.FieldErrors
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "signup", 5, time.Hour) {
			return errStatus(http.StatusTooManyRequests, "Too many sign-ups from your network. Please try again later.")
		}
		f := rq.r.PostForm
		in := map[string]any{
			au.Login:    strings.ToLower(strings.TrimSpace(f.Get(au.Login))),
			au.Password: f.Get(au.Password),
		}
		if au.Display != "" {
			in[au.Display] = f.Get(au.Display)
		}
		errs = rec.Assign(in)
		if errs == nil && f.Get(au.Password) != f.Get("password_confirm") {
			errs = runtime.FieldErrors{"password_confirm": "doesn't match the password"}
		}
		if errs == nil {
			if err := a.auth.Save(rq.ctx(), rec); err != nil {
				err = friendlyDBError(err)
				if !errors.As(err, &errs) {
					return err
				}
				if errs[""] != "" {
					errs = runtime.FieldErrors{au.Login: "already has an account — sign in instead"}
				}
			} else {
				if err := a.startSession(rq, rec.ID()); err != nil {
					return err
				}
				a.log.Info("signup", "id", rec.ID())
				http.Redirect(rq.w, rq.r, a.recordURL(c, rec.ID()), http.StatusSeeOther)
				return nil
			}
		}
	}
	fields := []formField{}
	for _, n := range []string{au.Login, au.Password, "password_confirm", au.Display} {
		if n == "" {
			continue
		}
		if n == "password_confirm" {
			fields = append(fields, formField{Name: n, Label: "Confirm password", Type: "password", Required: true, Error: errs[n]})
			continue
		}
		s := c.Slot(n)
		ff := formField{Name: n, Label: s.EffectiveLabel(), Type: s.EffectiveControlType(), Required: true,
			Placeholder: s.Placeholder, Help: s.Description, Error: errs[n]}
		if n == au.Login {
			ff.Type = "email"
			if !strings.Contains(n, "email") {
				ff.Type = "text"
			}
			ff.Value = rec.String(n)
		} else if n != au.Password {
			ff.Value = rec.String(n)
		}
		fields = append(fields, ff)
	}
	a.render(rq, "signup", "Create your account", map[string]any{"Fields": fields, "Message": generalError(errs), "Intro": a.man.Signup.Intro})
	return nil
}

func (a *App) login(rq *request) error {
	au := a.auth.Class.Auth
	next := safeNext(rq.r.FormValue("next"), a.cfg.BasePath)
	msg := ""
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "login", 10, 15*time.Minute) {
			return errStatus(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes.")
		}
		rec, err := a.Reg.Authenticate(rq.ctx(), rq.r.PostFormValue("login"), rq.r.PostFormValue("password"))
		if err == nil {
			if err := a.startSession(rq, rec.ID()); err != nil {
				return err
			}
			if next == "" {
				next = a.url("/")
			}
			http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
			return nil
		}
		if !errors.Is(err, runtime.ErrBadCredentials) {
			return err
		}
		msg = "That " + strings.ToLower(a.auth.Class.Slot(au.Login).EffectiveLabel()) + " and password don't match an account."
	}
	label := a.auth.Class.Slot(au.Login).EffectiveLabel()
	a.render(rq, "login", "Sign in", map[string]any{"Message": msg, "Next": next, "LoginLabel": label, "Login": rq.r.PostFormValue("login")})
	return nil
}

func (a *App) logout(rq *request) error {
	if rq.r.Method != http.MethodPost {
		return errStatus(http.StatusMethodNotAllowed, "Use the sign-out button")
	}
	a.endSession(rq)
	http.Redirect(rq.w, rq.r, a.url("/"), http.StatusSeeOther)
	return nil
}

func (a *App) me(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	http.Redirect(rq.w, rq.r, a.recordURL(a.auth.Class, rq.p.ID), http.StatusSeeOther)
	return nil
}

// ---- small helpers ----

func safeNext(next, base string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return ""
	}
	if base != "" && !strings.HasPrefix(next, base) {
		return ""
	}
	return next
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func encodeQuery(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	return n
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
