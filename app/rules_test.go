package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRules covers the schema rules in forms and pages: options narrowed by
// another field (own or through a link), match on relation pickers, enum
// labels, title expressions, when / required_when and class checks.
func TestRules(t *testing.T) {
	a, srv := newTestApp(t)
	ctx := t.Context()
	kits := a.Reg.MustClass("Kit")
	boots, bike := kits.New(map[string]any{"name": "Boots", "mode": "walk"}), kits.New(map[string]any{"name": "Bike", "mode": "cycle"})
	if err := kits.Save(ctx, boots); err != nil {
		t.Fatal(err)
	}
	if err := kits.Save(ctx, bike); err != nil {
		t.Fatal(err)
	}

	b := newBrowser(t, srv)
	b.signup("ann@example.org", "Ann")

	// New form: no mode yet, so every path and kit is offered, with labels and titles.
	_, form, _ := b.get("/c/trips/new")
	for _, want := range []string{"Canal towpath", "Cycleway", "Boots (walk)", "Bike (cycle)", `<option value="">No kit</option>`} {
		if !strings.Contains(form, want) {
			t.Errorf("new trip form should offer %q", want)
		}
	}
	// A walk on a road with the bike is refused on both counts.
	code, body, _ := b.post("/c/trips/new", url.Values{"name": {"Canal"}, "mode": {"walk"}, "path": {"road"}, "kit_id": {itoa(bike.ID())}})
	if code == http.StatusSeeOther || !strings.Contains(body, "must be one of Footpath, Canal towpath") || !strings.Contains(body, "fit the chosen mode") {
		t.Errorf("walk on road with a bike should be refused (%d): %s", code, extractAlert(body))
	}
	trip := mustID(b.create("/c/trips/new", url.Values{"name": {"Canal"}, "mode": {"walk"}, "path": {"towpath"}, "kit_id": {itoa(boots.ID())}}))

	// Edit form: options follow the saved mode — the rest are rendered hidden
	// and disabled, for the browser rules to bring back if the mode changes.
	_, form, _ = b.get("/c/trips/" + itoa(trip) + "/edit")
	if !strings.Contains(form, `value="cycleway" hidden disabled>Cycleway<`) || !strings.Contains(form, `hidden disabled>Bike (cycle)<`) ||
		!strings.Contains(form, `data-dd-m="{&#34;mode&#34;:&#34;cycle&#34;}" hidden disabled>Bike`) || strings.Contains(form, `disabled>Boots (walk)<`) {
		t.Error("edit form for a walk should offer only walking paths and kit")
	}
	if !strings.Contains(form, `data-dd-form="Trip"`) || !strings.Contains(form, `<script src="/_dd/rules.`) {
		t.Error("forms with rules load the rules script")
	}
	src := form[strings.Index(form, `/_dd/rules.`):]
	src = src[:strings.IndexByte(src, '"')]
	code, js, hdr := b.get(src)
	if code != http.StatusOK || !strings.Contains(js, "ddRules.start(") || !strings.Contains(hdr.Get("Cache-Control"), "immutable") ||
		!strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("rules script: %d %v", code, hdr)
	}
	if code, _, _ := b.get("/_dd/rules.000000000000.js"); code != http.StatusNotFound {
		t.Error("an old rules name is not found")
	}

	// Leg: path options come through the trip; when / required_when; check.
	_, form, _ = b.get("/c/stages/new?trip_id=" + itoa(trip))
	if !strings.Contains(form, `<option value="effort" selected>`) {
		t.Error("a walk's new stage should default to effort (default_expr through the trip)")
	}
	if !strings.Contains(form, `hidden disabled>Road<`) || strings.Contains(form, `disabled>Canal towpath<`) ||
		!strings.Contains(form, `data-dd-given="{&#34;trip.mode&#34;:&#34;walk&#34;}"`) {
		t.Error("a stage of a walk should offer only walking paths, and be given the trip's mode")
	}
	code, body, _ = b.post("/c/stages/new?trip_id="+itoa(trip), url.Values{"trip_id": {itoa(trip)}, "by": {"time"}, "effort": {"7"}})
	if code == http.StatusSeeOther || !strings.Contains(body, "is required") || !strings.Contains(body, "Give the stage") {
		t.Errorf("timed leg without minutes or distance should be refused (%d): %s", code, extractAlert(body))
	}
	stage := mustID(b.create("/c/stages/new?trip_id="+itoa(trip), url.Values{"trip_id": {itoa(trip)}, "km": {"3"}, "by": {"time"}, "minutes": {"40"}, "effort": {"7"}}))
	rec, err := a.Reg.MustClass("Stage").Load(ctx, stage)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Values["effort"] != nil || rec.Int("minutes") != 40 {
		t.Errorf("effort should be cleared for a timed leg: %v", rec.Values)
	}
	_, page, _ := b.get("/c/stages/" + itoa(stage))
	if strings.Contains(page, "Effort") || !strings.Contains(page, "Minutes") {
		t.Error("leg page should show minutes, not effort")
	}
}
