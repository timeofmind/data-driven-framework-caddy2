package app

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// A visitor drafts a route before joining: the server makes a guest of
// them, the route is theirs, signing up turns the guest into the account
// (the route stays), and a guest who signs in instead has their rows moved
// to the account.
func TestGuests(t *testing.T) {
	a, srv := newTestApp(t)
	if !a.guestsOffered() {
		t.Fatal("the fixture's Member names a guest slot")
	}
	// A visitor sees the form (no sign-in asked) and creates a route.
	vis := newBrowser(t, srv)
	if code, body, _ := vis.get("/c/routes/guest"); code != 200 || !strings.Contains(body, `name="name"`) {
		t.Fatalf("the draft form for a visitor: %d", code)
	}
	draft := vis.create("/c/routes/guest", url.Values{"name": {"Guest loop"}, "distance_km": {"3"}})
	if !strings.HasPrefix(draft, "/c/routes/") {
		t.Fatalf("draft redirect %q", draft)
	}
	code, body, _ := vis.get(draft)
	if code != 200 || !strings.Contains(body, "Guest loop") {
		t.Fatalf("the guest's own route: %d", code)
	}
	if strings.Contains(body, `>Calendar<`) { // a signed_in page in the menu
		t.Error("a guest is shown the signed-in menu")
	}
	if code, _, h := vis.get("/c/routes/new"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Errorf("a guest on a signed-in page: %d %s", code, h.Get("Location"))
	}
	if code, _, _ := vis.get("/me"); code != 401 && code != 303 {
		t.Errorf("a guest's /me: %d", code)
	}
	// Another visitor can't see it; the guest finds it in their list.
	if code, _, _ := newBrowser(t, srv).get(draft); code != 404 {
		t.Errorf("a stranger sees the guest's route: %d", code)
	}
	if _, list, _ := vis.get("/c/routes"); !strings.Contains(list, "Guest loop") {
		t.Error("the guest's list misses their route")
	}
	// The API knows them as a guest.
	resp, _ := vis.c.Get(srv.URL + "/api/v1/me")
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if me["authenticated"] != true || me["guest"] != true || me["guests"] != true {
		t.Errorf("api me for a guest: %v", me)
	}

	// Signing up turns the guest into the account: same row, route kept,
	// landing where asked.
	code, body, loc := vis.post("/signup", url.Values{"email": {"gwen@example.com"}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {"Gwen"}, "next": {"/c/routes"}})
	if code != 303 || loc != "/c/routes" {
		t.Fatalf("guest signup: %d %q %s", code, loc, extractAlert(body))
	}
	if code, body, _ := vis.get(draft); code != 200 || !strings.Contains(body, "Gwen") || !strings.Contains(body, `>Calendar<`) {
		t.Errorf("after signing up the route is not the account's: %d %v", code, strings.Contains(body, "Gwen"))
	}
	if code, _, _ := vis.get(draft + "/edit"); code != 200 {
		t.Errorf("the account can't edit its route: %d", code)
	}
	if code, _, h := vis.get("/me"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/c/members/") {
		t.Errorf("the account's /me: %d %s", code, h.Get("Location"))
	}
	members, _ := a.Reg.Class("Member")
	if n, _ := members.Count(t.Context(), runtime.Query{}); n != 1 {
		t.Errorf("%d members after a guest signed up; want 1", n)
	}

	// A guest who signs in to an existing account: their rows move over,
	// the guest row goes.
	ann := newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	g2 := newBrowser(t, srv)
	d2 := g2.create("/c/routes/guest", url.Values{"name": {"Second draft"}, "distance_km": {"2"}})
	if n, _ := members.Count(t.Context(), runtime.Query{}); n != 3 {
		t.Errorf("%d members with a second guest; want 3", n)
	}
	if code, _, loc := g2.post("/login", url.Values{"login": {"ann@example.com"}, "password": {"long enough pw"}, "next": {"/c/routes"}}); code != 303 || loc != "/c/routes" {
		t.Fatalf("guest login: %d %s", code, loc)
	}
	if code, body, _ := g2.get(d2); code != 200 || !strings.Contains(body, "Ann") {
		t.Errorf("after signing in the draft is not Ann's: %d", code)
	}
	if code, _, _ := ann.get(d2); code != 200 {
		t.Errorf("Ann's other browser can't see the adopted route: %d", code)
	}
	if n, _ := members.Count(t.Context(), runtime.Query{}); n != 2 {
		t.Errorf("%d members after adoption; want 2 (the guest is gone)", n)
	}

	// Where guests are not admitted nothing changes: a visitor is sent to sign in.
	if code, _, h := newBrowser(t, srv).get("/c/walks/new"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Errorf("a visitor creating where guests may not: %d %s", code, h.Get("Location"))
	}
}
