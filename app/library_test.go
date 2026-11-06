package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// TestSharedLibrary covers link: readable, filters (nesting, grouping, all),
// usage logging from links and copy actions with the search context, and
// ranking by usage — all declared in the fixture's schema.
func TestSharedLibrary(t *testing.T) {
	a, srv := newTestApp(t)
	ctx := t.Context()
	seed := func(class string, vals map[string]any) int64 {
		h := a.Reg.MustClass(class)
		r := h.New(vals)
		if err := h.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r.ID()
	}
	coast := seed("Region", map[string]any{"name": "Coast"})
	beach := seed("Terrain", map[string]any{"name": "Beach", "region_id": coast})
	sand := seed("Terrain", map[string]any{"name": "Sand", "region_id": coast, "parent_id": beach})
	forest := seed("Terrain", map[string]any{"name": "Forest", "region_id": coast})

	ann := newBrowser(t, srv)
	annID := mustID(ann.signup("ann@example.org", "Ann"))
	route := func(b *browser, name string, terrains ...int64) int64 {
		id := mustID(b.create("/c/routes/new", url.Values{"name": {name}, "visibility": {"public"}}))
		for _, tr := range terrains {
			b.create("/c/route-terrains/new?route_id="+itoa(id), url.Values{"route_id": {itoa(id)}, "terrain_id": {itoa(tr)}})
		}
		return id
	}
	shore := route(ann, "Shore loop", sand)
	woods := route(ann, "Woods walk", forest)
	dune := route(ann, "Dune path", sand, forest)

	ben := newBrowser(t, srv)
	ben.signup("ben@example.org", "Ben")

	// link: readable — ben may plan an outing on ann's public route, and its
	// picker lists it; a Walk's route link stays own-only.
	if _, form, _ := ben.get("/c/outings/new"); !strings.Contains(form, "Shore loop") {
		t.Error("outing route picker should list other people's public routes")
	}
	code, body, _ := ben.post("/c/walks/new", url.Values{"title": {"x"}, "date": {"2026-11-01"}, "route_id": {itoa(shore)}})
	if code == http.StatusSeeOther || !strings.Contains(body, "must be one of your own") {
		t.Errorf("walk → someone else's route should be refused (%d): %s", code, extractAlert(body))
	}

	// Filters: a parent value matches its children; several values must all match.
	list := func(b *browser, q string) string {
		_, body, _ := b.get("/c/routes" + q)
		return body
	}
	got := list(ben, "?terrain="+itoa(beach))
	if !strings.Contains(got, "Shore loop") || !strings.Contains(got, "Dune path") || strings.Contains(got, "Woods walk") {
		t.Error("terrain=Beach should match Shore loop and Dune path (Sand is under Beach)")
	}
	got = list(ben, "?terrain="+itoa(sand)+"&terrain="+itoa(forest))
	if !strings.Contains(got, "Dune path") || strings.Contains(got, "Shore loop") || strings.Contains(got, "Woods walk") {
		t.Error("Sand AND Forest should match only Dune path")
	}
	if !strings.Contains(got, `<details class="facet-group" open><summary class="facet-name">Coast</summary>`) || !strings.Contains(got, "Beach › Sand") ||
		!strings.Contains(got, `class="facet sub on" href="/c/routes?terrain=`+itoa(forest)+`" aria-pressed="true"`) {
		t.Error("facets: grouped by region, nested values marked sub, a chosen value links to removing it")
	}
	// Counts: with Forest chosen, adding Beach leaves 1 (Dune path); Sand is
	// chosen; nothing else remains to add, so other values would be disabled.
	got = list(ben, "?terrain="+itoa(forest))
	if !strings.Contains(got, `href="/c/routes?terrain=`+itoa(forest)+`&amp;terrain=`+itoa(beach)+`">Beach <span class="count">1</span>`) {
		t.Error("facet should link to adding Beach and count the 1 route left")
	}
	if strings.Contains(got, `type="search"`) {
		t.Error("routes have search: false — no search box")
	}
	if got := list(ben, "?q=Shore"); !strings.Contains(got, "Woods walk") {
		t.Error("q must be ignored when search is off")
	}
	got = list(ben, "?terrain="+itoa(sand)+"&terrain="+itoa(forest))
	if !strings.Contains(got, "/c/routes/"+itoa(dune)+"?terrain=") {
		t.Error("rows should carry the search into the record link")
	}

	// The record page passes the search on to its copy action and to "add outing".
	_, page, _ := ben.get("/c/routes/" + itoa(dune) + "?terrain=" + itoa(forest))
	if !strings.Contains(page, "/c/routes/"+itoa(dune)+"/copy?_search="+itoa(forest)) {
		t.Error("copy link should carry _search")
	}
	if !strings.Contains(page, "/c/outings/new?route_id="+itoa(dune)+"&amp;_search="+itoa(forest)) {
		t.Error("add-outing link should carry _search")
	}

	// Copy: confirm, then a private copy owned by ben with the terrains.
	code, _, loc := ben.post("/c/routes/"+itoa(dune)+"/copy?_search="+itoa(forest), url.Values{})
	if code != http.StatusSeeOther {
		t.Fatalf("copy: %d", code)
	}
	cp, err := a.Reg.MustClass("Route").Load(ctx, mustID(loc))
	if err != nil {
		t.Fatal(err)
	}
	if cp.String("visibility") != "private" || cp.Int("based_on_id") != dune || cp.Int("member_id") == 0 || cp.Int("member_id") == annID {
		t.Errorf("copy: %v", cp.Values)
	}
	if n, _ := a.Reg.MustClass("RouteTerrain").Count(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("route_id", cp.ID())}}); n != 2 {
		t.Errorf("copied terrains: %d", n)
	}
	if _, p, _ := ben.get(loc); strings.Contains(p, "Copy route") {
		t.Error("copy action offered on a private route despite its when-condition")
	}

	// Outings: ben on Woods (searching Forest), cat on Dune (searching Forest).
	ben.create("/c/outings/new?route_id="+itoa(woods)+"&_search="+itoa(forest), url.Values{"route_id": {itoa(woods)}, "day": {"2026-11-02"}})
	cat := newBrowser(t, srv)
	cat.signup("cat@example.org", "Cat")
	cat.create("/c/outings/new?route_id="+itoa(dune)+"&_search="+itoa(forest), url.Values{"route_id": {itoa(dune)}, "day": {"2026-11-03"}})

	uses, _ := a.Reg.MustClass("RouteUse").List(ctx, runtime.Query{OrderBy: "id asc"})
	if len(uses) != 3 || uses[0].String("kind") != "copied" || uses[0].Int("copy_id") != cp.ID() || uses[1].String("kind") != "planned" || uses[1].Int("outing_id") == 0 {
		t.Fatalf("usage log: %d rows", len(uses))
	}
	if n, _ := a.Reg.MustClass("RouteUseTerrain").Count(ctx, runtime.Query{}); n != 3 {
		t.Errorf("searched terrains logged: %d", n)
	}
	if code, _, _ := ben.get("/c/route-uses/new"); code != http.StatusForbidden {
		t.Errorf("usage log must not be creatable by hand: %d", code)
	}

	// Ranking (anonymous visitor): for Forest, Dune path (2 people) beats Woods walk (1).
	anon := newBrowser(t, srv)
	order := func(body string, names ...string) bool {
		last := -1
		for _, n := range names {
			i := strings.Index(body, ">"+n+"<")
			if i < last || i < 0 {
				return false
			}
			last = i
		}
		return true
	}
	got = list(anon, "?terrain="+itoa(forest))
	if !order(got, "Dune path", "Woods walk") || !strings.Contains(got, "Most chosen first") {
		t.Error("ranking for Forest: Dune path should come before Woods walk")
	}
	got = list(anon, "")
	if !order(got, "Dune path", "Woods walk", "Shore loop") {
		t.Error("overall ranking: Dune path, Woods walk, Shore loop")
	}
	// The owner column comes first and links to the owner; the title still
	// links to the route.
	if !strings.Contains(got, `<a href="/c/routes/`+itoa(dune)+`">Dune path</a>`) || strings.Contains(got, `class="open"`) {
		t.Error("route titles should link to the route even with the owner column first")
	}
	if got = list(anon, "?order=name+asc"); !order(got, "Dune path", "Shore loop", "Woods walk") {
		t.Error("an explicit order should replace ranking")
	}
}
