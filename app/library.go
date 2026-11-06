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

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Shared-library features declared in the schema: which rows a link may point
// at (belongs_to link), filtering lists by related values (filters), ranking by
// past use (usage), and copy actions (actions).

// searchParam carries the filter values a record was found with from the
// list, through its page, to the form or action that uses it.
const searchParam = "_search"

// ownLinksOnly reports whether rel may only point at the linker's own rows:
// targets with an owner, unless the relationship says link: readable.
func ownLinksOnly(rel *model.Relationship, target *model.Class) bool {
	return ownerOf(target) != "" && rel.Link != "readable"
}

// canLinkChild reports whether p may add a child of class child under parent
// through FK via, as far as the parent's ownership goes.
func canLinkChild(child *model.Class, via string, parent *runtime.Record, p *runtime.Principal) bool {
	if p == nil {
		return false
	}
	br := child.BelongsToVia(via)
	if p.Admin || br == nil || isAccessParent(child, via) || !ownLinksOnly(br, parent.Class) {
		return true
	}
	return parent.Int(ownerOf(parent.Class)) == p.ID
}

// selfParentFK is a class's belongs_to pointing at the class itself (e.g. a
// tag's parent tag), or "".
func selfParentFK(c *model.Class) string {
	if r := c.BelongsToTarget(c.Name); r != nil {
		return r.Via
	}
	return ""
}

// optionLabel is a record's title, prefixed with its parent's title when the
// class nests (e.g. "Race specific › 800").
func (a *App) optionLabel(ctx context.Context, h *runtime.Handle, r *runtime.Record, cache map[int64]*runtime.Record) string {
	fk := selfParentFK(h.Class)
	if fk == "" || r.Int(fk) == 0 {
		return r.Title()
	}
	pid := r.Int(fk)
	pr, ok := cache[pid]
	if !ok {
		pr, _ = h.Load(ctx, pid)
		cache[pid] = pr
	}
	if pr == nil {
		return r.Title()
	}
	return pr.Title() + " › " + r.Title()
}

// ids parses repeated or comma-separated positive integers, at most 20.
func ids(vs []string) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, v := range vs {
		for _, part := range strings.Split(v, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err == nil && n > 0 && !seen[n] && len(out) < 20 {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func joinIDs(list []int64) string {
	s := make([]string, len(list))
	for i, n := range list {
		s[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(s, ",")
}

// applyFilters adds the class's declared filters from the query string to q,
// with a removable chip per chosen value.
func (a *App) applyFilters(rq *request, q *runtime.Query, vals url.Values) ([]link, error) {
	c := rq.handle.Class
	var chips []link
	for _, f := range c.Filters {
		chosen := ids(vals[f.Name])
		if len(chosen) == 0 {
			continue
		}
		conds, err := rq.handle.FilterConds(rq.ctx(), f, chosen)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, conds...)
		val := a.Model.FilterValueClass(c, f)
		vh, err := a.Reg.Class(val.Name)
		if err != nil {
			return nil, err
		}
		cache := map[int64]*runtime.Record{}
		for _, id := range chosen {
			label := "#" + strconv.FormatInt(id, 10)
			if r, err := vh.Load(rq.ctx(), id); err == nil {
				if ok, _ := a.Reg.Readable(rq.ctx(), r, rq.p); ok {
					label = a.optionLabel(rq.ctx(), vh, r, cache)
				}
			}
			without := cloneValues(vals)
			without.Del(f.Name)
			without.Del("page")
			for _, other := range chosen {
				if other != id {
					without.Add(f.Name, strconv.FormatInt(other, 10))
				}
			}
			chips = append(chips, link{Label: label, URL: a.classURL(c) + encodeQuery(without)})
		}
	}
	return chips, nil
}

// rankByUsage orders q by past use when the class has a ranked filter and the
// viewer has not chosen an explicit order. It returns a note for the page.
func (a *App) rankByUsage(rq *request, q runtime.Query, vals url.Values) (runtime.Query, string, error) {
	c := rq.handle.Class
	if c.Usage == nil || vals.Get("order") != "" {
		return q, "", nil
	}
	f := c.FilterByName(c.Usage.Filter)
	if f == nil || !f.RankByUsage {
		return q, "", nil
	}
	q, err := rq.handle.RankByUsage(q, ids(vals[f.Name]))
	if err != nil {
		return q, "", err
	}
	note := "Most chosen first"
	if len(ids(vals[f.Name])) > 0 {
		note = "Most chosen first by people who searched for the same " + strings.ToLower(firstNonEmpty(f.Label, plural(a.Model.FilterValueClass(c, f).EffectiveLabel())))
	}
	return q, note, nil
}

// facet is a filter's choices on a list page, grouped.
type facet struct {
	Name   string
	Label  string
	Groups []facetGroup
}

type facetGroup struct {
	Name     string
	Options  []option
	Collapse bool // shown closed until opened…
	Open     bool // …unless it holds a chosen value
}

// facets builds each filter's choices as toggle links (no script needed: a
// click adds or removes the value). base is the list's current query, so each
// unchosen value can show how many records would remain with it added (for
// match: all); values that would leave none are disabled.
func (a *App) facets(rq *request, vals url.Values, base runtime.Query) ([]facet, error) {
	c := rq.handle.Class
	var out []facet
	base.Limit, base.Offset = 0, 0
	for _, f := range c.Filters {
		val := a.Model.FilterValueClass(c, f)
		vh, err := a.Reg.Class(val.Name)
		if err != nil {
			return nil, err
		}
		q, ok := vh.Scope(runtime.Query{Limit: 500}, rq.p)
		if !ok {
			continue
		}
		recs, err := vh.List(rq.ctx(), q)
		if err != nil {
			return nil, err
		}
		chosenIDs := ids(vals[f.Name])
		chosen := map[int64]bool{}
		for _, id := range chosenIDs {
			chosen[id] = true
		}
		counts := f.Match == "all" && len(recs) <= 150
		toggle := func(id int64) string {
			v := cloneValues(vals)
			v.Del(f.Name)
			v.Del("page")
			for _, other := range chosenIDs {
				if other != id {
					v.Add(f.Name, strconv.FormatInt(other, 10))
				}
			}
			if !chosen[id] {
				v.Add(f.Name, strconv.FormatInt(id, 10))
			}
			return a.classURL(c) + encodeQuery(v)
		}
		groupTitle := map[int64]string{}
		groupRank := map[string]int{} // groups follow their class's order_by
		if f.GroupBy != "" {
			if gh, err := a.Reg.Class(val.BelongsToVia(f.GroupBy).Target); err == nil {
				if gq, ok := gh.Scope(runtime.Query{Limit: 500}, rq.p); ok {
					gs, _ := gh.List(rq.ctx(), gq)
					for i, g := range gs {
						groupTitle[g.ID()] = g.Title()
						if _, seen := groupRank[g.Title()]; !seen {
							groupRank[g.Title()] = i + 1
						}
					}
				}
			}
		}
		// Each group lists its top-level values in the class's order, each
		// followed by the values nested below it.
		fk := selfParentFK(val)
		kids := map[int64][]*runtime.Record{}
		byGroup := map[string][]*runtime.Record{}
		for _, r := range recs {
			if fk != "" && r.Int(fk) != 0 {
				kids[r.Int(fk)] = append(kids[r.Int(fk)], r)
				continue
			}
			g := ""
			if f.GroupBy != "" {
				g = groupTitle[r.Int(f.GroupBy)]
			}
			byGroup[g] = append(byGroup[g], r)
		}
		fc := facet{Name: f.Name, Label: firstNonEmpty(f.Label, plural(val.EffectiveLabel()))}
		names := make([]string, 0, len(byGroup))
		for g := range byGroup {
			names = append(names, g)
		}
		sort.Slice(names, func(i, j int) bool {
			ri, rj := groupRank[names[i]], groupRank[names[j]]
			if ri != rj {
				return ri < rj
			}
			return names[i] < names[j]
		})
		for _, g := range names {
			var opts []option
			var walk func(r *runtime.Record, depth int)
			walk = func(r *runtime.Record, depth int) {
				o := option{Value: strconv.FormatInt(r.ID(), 10), Label: r.Title(), Selected: chosen[r.ID()], Depth: depth, URL: toggle(r.ID()), Count: -1}
				if counts && !o.Selected {
					conds, err := rq.handle.FilterConds(rq.ctx(), f, []int64{r.ID()})
					if err == nil {
						q := base
						q.Where = append(append([]runtime.Cond{}, base.Where...), conds...)
						if n, err := rq.handle.Count(rq.ctx(), q); err == nil {
							o.Count, o.Disabled = n, n == 0
						}
					}
				}
				opts = append(opts, o)
				if depth < 8 {
					for _, k := range kids[r.ID()] {
						walk(k, depth+1)
					}
				}
			}
			for _, r := range byGroup[g] {
				walk(r, 0)
			}
			fg := facetGroup{Name: g, Options: opts, Collapse: f.CollapseGroups && g != ""}
			for _, o := range opts {
				fg.Open = fg.Open || o.Selected
			}
			fc.Groups = append(fc.Groups, fg)
		}
		out = append(out, fc)
	}
	return out, nil
}

// searchContext is the usage filter's chosen values as query parameters
// (for links from a filtered list to a record page) and as a _search value
// (for links from a record page to forms and actions that use it).
func searchContext(c *model.Class, vals url.Values) (forRecord url.Values, search string) {
	if c.Usage == nil || c.Usage.Filter == "" {
		return nil, ""
	}
	chosen := ids(vals[c.Usage.Filter])
	if len(chosen) == 0 {
		return nil, ""
	}
	v := url.Values{}
	for _, id := range chosen {
		v.Add(c.Usage.Filter, strconv.FormatInt(id, 10))
	}
	return v, joinIDs(chosen)
}

// withSearch appends _search=<ids> to a URL.
func withSearch(u, search string) string {
	if search == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + searchParam + "=" + url.QueryEscape(search)
}

// recordActions are the links for a record's declared actions.
func (a *App) recordActions(rq *request, rec *runtime.Record, search string) []link {
	var out []link
	if rq.p == nil {
		return nil
	}
	for _, act := range rec.Class.Actions {
		if runtime.CanCreate(rec.Class, rq.p) && runtime.ActionAllowed(act, rec) {
			out = append(out, link{Label: actionLabel(act), URL: withSearch(a.recordURL(rec.Class, rec.ID())+"/"+act.Name, search)})
		}
	}
	return out
}

func actionLabel(act *model.Action) string {
	return firstNonEmpty(act.Label, generator.EnumLabel(act.Name))
}

// action runs a declared action: GET asks for confirmation, POST does it.
func (a *App) action(rq *request, name string) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	rec, err := a.loadReadable(rq)
	if err != nil {
		return err
	}
	c := rec.Class
	act := c.ActionByName(name)
	if act == nil || !runtime.CanCreate(c, rq.p) || !runtime.ActionAllowed(act, rec) {
		return errForbidden
	}
	if rq.r.Method != http.MethodPost {
		msg := fmt.Sprintf("%s “%s”? A new %s is created for you", actionLabel(act), rec.Title(), strings.ToLower(c.EffectiveLabel()))
		if len(act.Copy) > 0 {
			msg += " with its " + strings.ToLower(strings.Join(copyLabels(c, act), ", "))
		}
		a.render(rq, "confirm", actionLabel(act), map[string]any{
			"Message": msg + ", and you can change it freely.", "Action": rq.r.URL.RequestURI(),
			"Cancel": a.recordURL(c, rec.ID()), "Submit": actionLabel(act),
		})
		return nil
	}
	check := func(r *runtime.Record) error {
		if errs := a.checkLinks(rq.ctx(), rq.p, r); errs != nil {
			return errs
		}
		return nil
	}
	dst, err := a.Reg.Copy(rq.ctx(), rq.p, rec, act, check)
	if err != nil {
		var fe runtime.FieldErrors
		if errors.As(err, &fe) {
			return errStatus(http.StatusConflict, "This can't be copied: "+generalError(fe))
		}
		return err
	}
	if use := c.UseByAction(act.Name); use != nil {
		a.logUsage(rq.ctx(), rq.p, c, rec.ID(), use, dst.ID(), ids([]string{rq.r.URL.Query().Get(searchParam)}))
	}
	http.Redirect(rq.w, rq.r, a.recordURL(c, dst.ID()), http.StatusSeeOther)
	return nil
}

func copyLabels(c *model.Class, act *model.Action) []string {
	var out []string
	for _, p := range act.Copy {
		if strings.Contains(p, ".") {
			continue
		}
		if r := c.RelationshipByName(p); r != nil {
			out = append(out, firstNonEmpty(r.Label, p))
		}
	}
	return out
}

// logLinkUses logs a usage for every FK of a newly created record that the
// schema counts as using the linked record (e.g. scheduling a workout).
func (a *App) logLinkUses(ctx context.Context, p *runtime.Principal, rec *runtime.Record, search []int64) {
	for _, rel := range rec.Class.AllRelationships() {
		if !rel.IsBelongsTo() || rec.Int(rel.Via) == 0 {
			continue
		}
		for _, ut := range a.Model.UsesByLink(rec.Class.Name, rel.Via) {
			a.logUsage(ctx, p, ut.Class, rec.Int(rel.Via), ut.Use, rec.ID(), search)
		}
	}
}

func (a *App) logUsage(ctx context.Context, p *runtime.Principal, used *model.Class, usedID int64, use *model.UsageUse, newID int64, search []int64) {
	if err := a.Reg.LogUsage(ctx, p, used, usedID, use, newID, search); err != nil {
		a.log.Warn("usage log failed", "class", used.Name, "id", usedID, "use", use.Kind, "err", err)
	}
}
