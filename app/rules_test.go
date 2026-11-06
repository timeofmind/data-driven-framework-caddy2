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
	for _, want := range []string{"Canal towpath", "Cycleway", "Boots (walk)", "Bike (cycle)"} {
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

	// Edit form: options follow the saved mode.
	_, form, _ = b.get("/c/trips/" + itoa(trip) + "/edit")
	if strings.Contains(form, ">Cycleway<") || strings.Contains(form, "Bike (cycle)") || !strings.Contains(form, "Boots (walk)") {
		t.Error("edit form for a walk should offer only walking paths and kit")
	}

	// Leg: path options come through the trip; when / required_when; check.
	_, form, _ = b.get("/c/stages/new?trip_id=" + itoa(trip))
	if strings.Contains(form, ">Road<") || !strings.Contains(form, "Canal towpath") {
		t.Error("a leg of a walk should offer only walking paths")
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
