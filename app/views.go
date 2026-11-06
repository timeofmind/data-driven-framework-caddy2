package app

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

//go:embed templates/*.html static/*
var embedded embed.FS

var pageNames = []string{"home", "list", "calendar", "detail", "form", "confirm", "login", "signup", "upload", "error", "feed"}

type views struct {
	pages     map[string]*template.Template
	staticDir string // the theme's static/ directory ("" = use the built-in default)
}

// loadViews parses templates. components.html (generic, schema-driven
// fragments) is built in; layout.html and the page templates come from the
// app's theme directory when it has them, else from the built-in default theme.
func loadViews(themeDir string) (*views, error) {
	read := func(name string) ([]byte, error) {
		if themeDir != "" {
			if b, err := os.ReadFile(filepath.Join(themeDir, name)); err == nil {
				return b, nil
			}
		}
		return fs.ReadFile(embedded, "templates/"+name)
	}
	v := &views{pages: map[string]*template.Template{}}
	comps, err := fs.ReadFile(embedded, "templates/components.html")
	if err != nil {
		return nil, err
	}
	layout, err := read("layout.html")
	if err != nil {
		return nil, err
	}
	for _, name := range pageNames {
		body, err := read(name + ".html")
		if err != nil {
			return nil, err
		}
		t := template.New("layout").Funcs(funcs)
		for _, part := range []struct {
			name string
			src  []byte
		}{{"components.html", comps}, {"layout.html", layout}, {name + ".html", body}} {
			if _, err := t.Parse(string(part.src)); err != nil {
				return nil, fmt.Errorf("%s: %w", part.name, err)
			}
		}
		v.pages[name] = t
	}
	if themeDir != "" {
		if st, err := os.Stat(filepath.Join(themeDir, "static")); err == nil && st.IsDir() {
			v.staticDir = filepath.Join(themeDir, "static")
		}
	}
	return v, nil
}

var funcs = template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"eq2": func(a, b string) bool { return a == b },
}

// page is the data every template receives.
type page struct {
	Site    string
	Tagline string
	Title   string
	Base    string
	P       *runtime.Principal
	CSRF    string
	Nav     []link
	Upload  *link // the upload page, when the app has one and the viewer is signed in
	Signup  bool
	Data    any
	Path    string
}

type link struct {
	Label  string `json:"label"`
	URL    string `json:"url"`
	Active bool   `json:"-"`
}

func (a *App) render(rq *request, name, title string, data any) {
	t := a.views.pages[name]
	pg := page{
		Site: a.man.Name, Tagline: a.man.Tagline, Title: title, Base: a.cfg.BasePath,
		P: rq.p, CSRF: rq.csrf, Nav: a.nav(rq), Signup: a.man.signupEnabled() && a.auth != nil, Upload: a.uploadLink(rq),
		Data: data, Path: rq.r.URL.Path,
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pg); err != nil {
		a.log.Error("template", "page", name, "error", err)
		http.Error(rq.w, "template error", http.StatusInternalServerError)
		return
	}
	rq.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	rq.w.Header().Set("Cache-Control", "no-store")
	rq.w.Write(buf.Bytes())
}

func (a *App) uploadLink(rq *request) *link {
	if a.uploads == nil || rq.p == nil {
		return nil
	}
	return &link{Label: firstNonEmpty(a.man.Uploads.Title, "Upload"), URL: a.url("/upload")}
}

// static serves the theme's static/ files (or the default theme's).
func (a *App) static(rq *request, name string) {
	clean := path.Clean("/" + name)[1:]
	if clean == "" || strings.HasPrefix(clean, ".") || strings.Contains(clean, "/.") {
		http.NotFound(rq.w, rq.r)
		return
	}
	var b []byte
	var err error
	if a.views.staticDir != "" {
		b, err = os.ReadFile(filepath.Join(a.views.staticDir, filepath.FromSlash(clean)))
	} else {
		b, err = fs.ReadFile(embedded, "static/"+clean)
	}
	if err != nil {
		http.NotFound(rq.w, rq.r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(clean))
	if ct == "" {
		ct = "application/octet-stream"
	}
	rq.w.Header().Set("Content-Type", ct)
	rq.w.Header().Set("Cache-Control", "public, max-age=3600")
	rq.w.Write(b)
}

// nav lists the top-level classes: those not governed by a parent and not join tables.
func (a *App) nav(rq *request) []link {
	var out []link
	classes := a.Model.ConcreteClasses()
	if len(a.man.Nav) > 0 {
		classes = classes[:0:0]
		for _, n := range a.man.Nav {
			classes = append(classes, a.Model.Class(n))
		}
	}
	for _, c := range classes {
		if len(a.man.Nav) == 0 && !a.navigable(c) {
			continue
		}
		u := a.classURL(c)
		out = append(out, link{Label: plural(c.EffectiveLabel()), URL: u, Active: strings.HasPrefix(rq.r.URL.Path, u)})
	}
	return out
}

func (a *App) navigable(c *model.Class) bool {
	if c.Access != nil && c.Access.Parent != "" {
		return false
	}
	return c.PKSlot() != nil && c.Auth == nil
}

func (a *App) classURL(c *model.Class) string { return a.url("/c/" + generator.ResourcePath(c)) }

func (a *App) recordURL(c *model.Class, id int64) string {
	return fmt.Sprintf("%s/%d", a.classURL(c), id)
}

func plural(label string) string {
	switch {
	case strings.HasSuffix(label, "y") && !strings.HasSuffix(label, "ay") && !strings.HasSuffix(label, "ey"):
		return label[:len(label)-1] + "ies"
	case strings.HasSuffix(label, "s"):
		return label + "es"
	}
	return label + "s"
}

// ---- view models ----

type cell struct {
	Text   string
	URL    string
	Class  string
	Tag    string // enum value; rendered as a pill inside the cell
	NoLink bool   // first-column cells normally link to the row
	Open   bool   // the record's title: links to the row
}

type row struct {
	URL   string
	Cells []cell
	Open  bool // a cell other than the first is the title link
}

type table struct {
	Columns []string
	Rows    []row
}

type column struct {
	slot *model.Slot
	rel  *model.Relationship // set for FK columns
}

// listColumns picks the columns for a class in a table: the owner first, then
// FK and visible scalar slots by ui_order, leaving out long text, internal
// metadata and the FK named in skip (when shown under that parent).
func (a *App) listColumns(c *model.Class, skip string) []column {
	owner := ownerOf(c)
	var cols []column
	for _, s := range c.AllSlots() {
		if s.Name == skip || s == c.PKSlot() || s.Type == "text" || s.Type == "password" {
			continue
		}
		if rel := c.BelongsToVia(s.Name); rel != nil {
			if t := a.Model.Class(rel.Target); t != nil && t.Auth != nil && s.Name != owner && owner != "" {
				continue // a second FK to the principal class duplicating the owner column
			}
			cols = append(cols, column{slot: s, rel: rel})
			continue
		}
		if s.Hidden || s.ReadOnly || s.Autogenerated {
			continue
		}
		if c.Access != nil && c.Access.PublicWhen[s.Name] != "" {
			continue // the access slot shows as the record's public/private state
		}
		cols = append(cols, column{slot: s})
	}
	sort.SliceStable(cols, func(i, j int) bool {
		oi, oj := cols[i].slot.UIOrder, cols[j].slot.UIOrder
		if cols[i].slot.Name == owner {
			oi = -1
		}
		if cols[j].slot.Name == owner {
			oj = -1
		}
		return oi < oj
	})
	if len(cols) > 8 {
		cols = cols[:8]
	}
	return cols
}

func ownerOf(c *model.Class) string {
	if c.Access == nil {
		return ""
	}
	return c.Access.Owner
}

func columnLabel(col column) string {
	if col.rel != nil {
		if col.slot.Label != "" {
			return col.slot.Label
		}
		return col.rel.EffectiveLabel()
	}
	return col.slot.EffectiveLabel()
}

// titles resolves FK ids to record titles, only for rows p may read.
type titles map[string]map[int64]*runtime.Record

func (a *App) resolveTitles(ctx context.Context, p *runtime.Principal, recs []*runtime.Record, cols []column) (titles, error) {
	out := titles{}
	for _, col := range cols {
		if col.rel == nil {
			continue
		}
		ids := map[int64]bool{}
		for _, r := range recs {
			if id := r.Int(col.slot.Name); id != 0 {
				ids[id] = true
			}
		}
		if len(ids) == 0 {
			continue
		}
		h, err := a.Reg.Class(col.rel.Target)
		if err != nil {
			continue
		}
		list := make([]any, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		q, ok := h.Scope(runtime.Query{Where: []runtime.Cond{{Field: h.Class.PKSlot().Name, Op: "in", Value: list}}}, p)
		found := map[int64]*runtime.Record{}
		if ok {
			targets, err := h.List(ctx, q)
			if err != nil {
				return nil, err
			}
			for _, t := range targets {
				found[t.ID()] = t
			}
		}
		out[col.slot.Name] = found
	}
	return out, nil
}

func (a *App) buildTable(ctx context.Context, p *runtime.Principal, c *model.Class, recs []*runtime.Record, skip string) (table, error) {
	cols := a.listColumns(c, skip)
	tt, err := a.resolveTitles(ctx, p, recs, cols)
	if err != nil {
		return table{}, err
	}
	var t table
	for _, col := range cols {
		t.Columns = append(t.Columns, columnLabel(col))
	}
	// The record's title links to it wherever its column is (the owner
	// column usually comes first and links to the owner).
	titleCol := -1
	for i, col := range cols {
		if col.rel == nil && col.slot == c.DisplaySlot() {
			titleCol = i
		}
	}
	for _, r := range recs {
		rw := row{URL: a.recordURL(c, r.ID())}
		for i, col := range cols {
			ce := a.cellFor(r, col, tt)
			if i == titleCol && i > 0 && ce.Class != "muted" {
				ce.Open, rw.Open = true, true
			}
			rw.Cells = append(rw.Cells, ce)
		}
		t.Rows = append(t.Rows, rw)
	}
	return t, nil
}

func (a *App) cellFor(r *runtime.Record, col column, tt titles) cell {
	v := r.Get(col.slot.Name)
	if col.rel != nil {
		id := r.Int(col.slot.Name)
		if id == 0 {
			return cell{Text: "—", Class: "muted"}
		}
		if t, ok := tt[col.slot.Name][id]; ok {
			return cell{Text: t.Title(), URL: a.recordURL(t.Class, id)}
		}
		return cell{Text: "private", Class: "muted"}
	}
	txt := runtime.DisplayValue(col.slot, v)
	if txt == "" {
		return cell{Text: "—", Class: "muted"}
	}
	if col.slot.Type == "enum" {
		return cell{Text: txt, Tag: runtime.FormatValue(v)}
	}
	if col.slot.Type == "float" || col.slot.Type == "integer" || col.slot.Type == "duration" {
		return cell{Text: txt, Class: "num"}
	}
	return cell{Text: txt}
}

// field is one labelled value on a detail page.
type field struct {
	Label, Text, URL, Class, Help, Tag string
}

func (a *App) detailFields(ctx context.Context, p *runtime.Principal, r *runtime.Record) ([]field, error) {
	c := r.Class
	var cols []column
	for _, s := range c.AllSlots() {
		if s == c.PKSlot() || s.Type == "password" || s == c.DisplaySlot() {
			continue
		}
		if rel := c.BelongsToVia(s.Name); rel != nil {
			cols = append(cols, column{slot: s, rel: rel})
		} else if !s.Hidden {
			cols = append(cols, column{slot: s})
		}
	}
	sort.SliceStable(cols, func(i, j int) bool {
		oi, oj := cols[i].slot.UIOrder, cols[j].slot.UIOrder
		if cols[i].slot.Name == ownerOf(c) {
			oi = -1
		}
		if cols[j].slot.Name == ownerOf(c) {
			oj = -1
		}
		return oi < oj
	})
	tt, err := a.resolveTitles(ctx, p, []*runtime.Record{r}, cols)
	if err != nil {
		return nil, err
	}
	var out []field
	a.Reg.LoadLinks(ctx, r) // when rules may read the record's links
	for _, col := range cols {
		if !runtime.SlotApplies(r, col.slot) {
			continue // e.g. a zone field when intensity is set by race pace
		}
		ce := a.cellFor(r, col, tt)
		if ce.Text == "—" && col.slot.Type != "boolean" {
			continue // leave empty values off the page
		}
		out = append(out, field{Label: columnLabel(col), Text: ce.Text, URL: ce.URL, Class: ce.Class, Tag: ce.Tag})
		if col.slot.Type == "text" {
			out[len(out)-1].Class = "prose"
		}
	}
	for _, cv := range runtime.Computed(r) {
		out = append(out, field{Label: cv.Label, Text: cv.Text, Class: "num"})
	}
	return out, nil
}

// formField is one input in a generated form.
type formField struct {
	Name, Label, Type, Value, Placeholder, Help, Error string
	Required, Checked                                  bool
	Min, Max, Step, Pattern                            string
	Options                                            []option
}

type option struct {
	Value, Label string
	Selected     bool
	Depth        int    // facets: nesting below a broader value
	URL          string // facets: adds or removes this value
	Count        int    // facets: records left with this value added (-1: not counted)
	Disabled     bool   // facets: adding it would leave nothing
	Match        string // relation options: the target's matched values as JSON, for rules in the browser
}

type formGroup struct {
	Name   string
	Fields []formField
}

// formFor builds the form for a class. include lists hidden slots that should
// still be inputs (the auth class's login slot). Relation selectors offer the
// target records p may link to.
func (a *App) formFor(ctx context.Context, p *runtime.Principal, c *model.Class, rec *runtime.Record, errs runtime.FieldErrors, include ...string) ([]formGroup, error) {
	cfg := generator.ClassUI(c)
	var groups []formGroup
	incl := map[string]bool{}
	for _, n := range include {
		incl[n] = true
	}
	var extra []generator.UIField
	for _, n := range include {
		if s := c.Slot(n); s != nil && s.Hidden {
			f := generator.SlotUIField(s)
			f.Type = "email"
			if !strings.Contains(strings.ToLower(s.Name), "email") {
				f.Type = "text"
			}
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		cfg.Form.Groups = append([]generator.UIGroup{{Name: "Account", Fields: extra}}, cfg.Form.Groups...)
	}
	for _, g := range cfg.Form.Groups {
		fg := formGroup{Name: g.Name}
		if g.Name == "General" {
			fg.Name = ""
		}
		for _, f := range g.Fields {
			s := c.Slot(f.Name)
			ff := formField{
				Name: f.Name, Label: f.Label, Type: f.Type, Placeholder: f.Placeholder, Help: f.Description,
				Required: f.Required, Pattern: f.Pattern, Step: f.Step, Error: errs[f.Name],
			}
			if s.Type == "integer" || s.Type == "float" {
				if f.Min != nil {
					ff.Min = runtime.FormatValue(*f.Min)
				}
				if f.Max != nil {
					ff.Max = runtime.FormatValue(*f.Max)
				}
			}
			v := rec.Get(f.Name)
			ff.Value = runtime.InputValue(s, v)
			switch f.Type {
			case "checkbox":
				ff.Checked = rec.Bool(f.Name)
			case "select":
				// An options rule (e.g. surfaces for the chosen sport) narrows the
				// list: values that don't fit are rendered hidden and disabled, so
				// the browser rules can bring them back when the sport changes. The
				// current value stays so a mismatch is shown, not dropped.
				var allowed map[string]bool
				if len(s.Options) > 0 {
					if vs := a.Reg.AllowedValues(ctx, rec, s); vs != nil {
						allowed = map[string]bool{ff.Value: true}
						for _, v := range vs {
							allowed[v] = true
						}
					}
				}
				for _, o := range f.Options {
					ff.Options = append(ff.Options, option{Value: o.Value, Label: o.Label, Selected: o.Value == ff.Value,
						Disabled: allowed != nil && !allowed[o.Value]})
				}
				if !f.Required {
					ff.Options = append([]option{{Value: "", Label: "—"}}, ff.Options...)
				}
			case "relation":
				opts, err := a.relationOptions(ctx, p, c, f.Name, rec)
				if err != nil {
					return nil, err
				}
				ff.Type = "select"
				ff.Options = opts
			case "password":
				ff.Value = ""
				if rec.Persisted() {
					ff.Required = false
					ff.Help = "Leave blank to keep your current password"
				}
			case "duration":
				ff.Type = "text"
				ff.Pattern = `\d+(:[0-5]?\d){0,2}(\.\d+)?|\d+h(\d+m)?(\d+s)?|\d+m(\d+s)?`
			}
			fg.Fields = append(fg.Fields, ff)
		}
		if len(fg.Fields) > 0 {
			groups = append(groups, fg)
		}
	}
	return groups, nil
}

// relationOptions lists records that p may link to through FK slot. Targets
// with an owner are limited to p's own rows (you link your own records to a
// result, not someone else's); others to rows p can read.
func (a *App) relationOptions(ctx context.Context, p *runtime.Principal, c *model.Class, slot string, rec *runtime.Record) ([]option, error) {
	rel := c.BelongsToVia(slot)
	h, err := a.Reg.Class(rel.Target)
	if err != nil {
		return nil, err
	}
	current := rec.Int(slot)
	q := runtime.Query{Limit: 300}
	// match: targets that don't fit the record (e.g. sub-activities of another
	// sport) are rendered hidden and disabled, for the browser rules to show
	// again if the sport changes.
	conds := a.Reg.MatchConds(ctx, rec, rel)
	if owner := ownerOf(h.Class); ownLinksOnly(rel, h.Class) && p != nil && !p.Admin && !isAccessParent(c, slot) {
		q.Where = append(q.Where, runtime.Eq(owner, p.ID))
	}
	q, ok := h.Scope(q, p)
	var opts []option
	if s := c.Slot(slot); !rel.Required && !s.Required {
		blank := "— none —"
		if s.Placeholder != "" {
			blank = s.Placeholder // e.g. "Standard running" for an optional drill
		}
		opts = append(opts, option{Value: "", Label: blank})
	}
	if !ok {
		return opts, nil
	}
	recs, err := h.List(ctx, q)
	if err != nil {
		return nil, err
	}
	cache := map[int64]*runtime.Record{}
	for _, r := range recs {
		label := a.optionLabel(ctx, h, r, cache)
		if cs := h.Class.Calendar; cs != "" && r.String(cs) != "" {
			label = r.String(cs) + " · " + label
		}
		o := option{Value: fmt.Sprint(r.ID()), Label: label, Selected: r.ID() == current,
			Disabled: r.ID() != current && !runtime.Matches(r, conds)}
		if len(rel.Match) > 0 {
			m := map[string]any{}
			for _, as := range rel.Match {
				m[as.Slot] = r.Values[as.Slot]
			}
			if b, err := json.Marshal(m); err == nil {
				o.Match = string(b)
			}
		}
		opts = append(opts, o)
	}
	return opts, nil
}

func isAccessParent(c *model.Class, slot string) bool {
	return c.Access != nil && c.Access.Parent == slot
}

// child is a related-records section on a detail page.
type child struct {
	Label  string
	Table  table
	AddURL string
	AddLbl string
	More   string
	Empty  string
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
