package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// A visitor's first write makes them a guest without rotating the CSRF token
// their client holds; every API answer carries the token in a header.
func TestGuestMintKeepsCSRF(t *testing.T) {
	_, srv := newTestApp(t)
	b := newBrowser(t, srv)
	resp, _ := b.c.Get(srv.URL + "/api/v1/me")
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	tok, _ := me["csrf_token"].(string)
	if tok == "" || resp.Header.Get("X-CSRF-Token") != tok {
		t.Fatalf("token %q header %q", tok, resp.Header.Get("X-CSRF-Token"))
	}
	post := func() (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/routes", strings.NewReader(`{"name":"Draft","distance_km":1}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", tok)
		r, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		r.Body.Close()
		msg, _ := body["message"].(string)
		return r, msg
	}
	r1, _ := post() // makes the guest
	if r1.StatusCode != 201 || r1.Header.Get("X-CSRF-Token") != tok {
		t.Fatalf("first write: %d, token after %q (want unchanged %q)", r1.StatusCode, r1.Header.Get("X-CSRF-Token"), tok)
	}
	r2, msg := post() // the same token still works
	if r2.StatusCode != 201 {
		t.Fatalf("second write with the same token: %d %s", r2.StatusCode, msg)
	}
	// Signing in rotates it (a real change of privilege).
	ann := newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	code, _, _ := b.post("/login", url.Values{"login": {"ann@example.com"}, "password": {"long enough pw"}})
	if code != 303 {
		t.Fatalf("login %d", code)
	}
	resp, _ = b.c.Get(srv.URL + "/api/v1/me")
	resp.Body.Close()
	if resp.Header.Get("X-CSRF-Token") == tok {
		t.Error("the token was not rotated at sign-in")
	}
}

// Idle guests are swept after guests.keep_for, with what they own.
func TestIdleGuestSwept(t *testing.T) {
	a, srv := newTestApp(t)
	a.man.Guests.KeepFor = "1ms"
	vis := newBrowser(t, srv)
	draft := vis.create("/c/routes/guest", url.Values{"name": {"Old draft"}, "distance_km": {"1"}})
	members := a.Reg.MustClass("Member")
	if n, _ := members.Count(t.Context(), runtime.Query{}); n != 1 {
		t.Fatalf("members %d", n)
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has second resolution
	// Any new session runs the sweep.
	newBrowser(t, srv).signup("x@example.com", "X")
	if n, _ := members.Count(t.Context(), runtime.Query{}); n != 1 {
		t.Errorf("after the sweep %d members; want 1 (the account; the idle guest gone)", n)
	}
	if code, _, _ := newBrowser(t, srv).get(draft); code != 404 {
		t.Errorf("the swept guest's route is still served: %d", code)
	}
}
