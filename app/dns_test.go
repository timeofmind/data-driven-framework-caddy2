package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// fakeZone is example.com's DNS behind Cloudflare's API.
type fakeZone struct {
	mu   sync.Mutex
	recs map[string]map[string]any
	next int
}

func (f *fakeZone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, result any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"success": status < 300, "result": result, "errors": []any{}})
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 9109, "message": "Invalid access token"}}})
		return
	}
	switch p := r.URL.Path; {
	case p == "/zones":
		if r.URL.Query().Get("name") == "example.com" {
			reply(200, []any{map[string]any{"id": "z1", "name": "example.com"}})
		} else {
			reply(200, []any{})
		}
	case p == "/zones/z1/dns_records" && r.Method == http.MethodGet:
		var out []any
		if r.URL.Query().Get("page") == "1" {
			for _, rec := range f.recs {
				out = append(out, rec)
			}
		}
		reply(200, out)
	case p == "/zones/z1/dns_records" && r.Method == http.MethodPost:
		var rec map[string]any
		json.NewDecoder(r.Body).Decode(&rec)
		f.next++
		rec["id"] = fmt.Sprintf("r%d", f.next)
		f.recs[rec["id"].(string)] = rec
		reply(200, rec)
	case strings.HasPrefix(p, "/zones/z1/dns_records/"):
		id := strings.TrimPrefix(p, "/zones/z1/dns_records/")
		if f.recs[id] == nil {
			reply(404, nil)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.recs, id)
			reply(200, map[string]any{"id": id})
			return
		}
		var rec map[string]any
		json.NewDecoder(r.Body).Decode(&rec)
		rec["id"] = id
		f.recs[id] = rec
		reply(200, rec)
	default:
		reply(404, nil)
	}
}

func (f *fakeZone) find(typ, name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.recs {
		if r["type"] == typ && r["name"] == name {
			return r
		}
	}
	return nil
}

// A live app's zone (a cloudflare listener no class is routed to): no DNS
// section until its token works; then the records the app needs are put in
// place, the page shows the zone as Cloudflare holds it and changes it
// there, and a needed record deleted since is offered, not added back.
func TestDNSPage(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	f := &fakeZone{recs: map[string]map[string]any{
		"mx": {"id": "mx", "type": "MX", "name": "example.com", "content": "mail.example.com", "ttl": 1, "priority": 10},
	}}
	cf := httptest.NewServer(f)
	defer cf.Close()
	dir := servicesFixture(t, true)
	os.WriteFile(filepath.Join(dir, "routing", "dns.yaml"), []byte(fmt.Sprintf(`listeners:
  dns:
    type: cloudflare
    base_url: %s
    records: |
      TXT example.com. v=spf1 include:spf.mywahosting.com -all # Mail: SPF
      A example.com. 203.0.113.7 # The app's domain
`, cf.URL)), 0o644)
	a, srv := newServicesApp(t, dir, []byte("a-token-key-shared-by-the-nodes"))
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")

	_, body, _ := b.get("/admin/services")
	if !strings.Contains(body, `name="s_zone"`) || !strings.Contains(body, `name="s_api_token"`) {
		t.Fatalf("the zone's settings are asked for on Service Configuration")
	}
	if a.adminSectionShown("/admin/dns") || !a.adminSectionShown("/admin/media") {
		t.Error("no DNS section in the menu before the token works")
	}
	if _, body, _ = b.get("/admin/dns"); !strings.Contains(body, "Not set up yet") {
		t.Errorf("the page says what is missing:\n%s", body)
	}

	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"dns"}, "s_zone": {"example.com"}, "s_api_token": {"tok"}}); code != http.StatusSeeOther {
		t.Fatalf("save the zone: %d\n%s", code, extractAlert(body))
	}
	deadline := time.Now().Add(5 * time.Second)
	for (f.find("TXT", "example.com") == nil || f.find("A", "example.com") == nil) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if f.find("TXT", "example.com") == nil || f.find("A", "example.com") == nil {
		t.Fatalf("the needed records are put in place once the token works: %v", f.recs)
	}
	noted := func() int {
		all, _ := a.dnsAdded().List(context.Background(), runtime.Query{Limit: 100})
		return len(all)
	}
	for noted() < 2 && time.Now().Before(deadline) { // the note follows the record
		time.Sleep(20 * time.Millisecond)
	}

	_, body, _ = b.get("/admin/dns")
	if !a.adminSectionShown("/admin/dns") {
		t.Error("the DNS section is in the menu once the token works")
	}
	for _, want := range []string{"mail.example.com", "priority 10", "203.0.113.7", "in place", "needed: Mail: SPF"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}

	if !strings.Contains(body, "Every record the app needs is in place.") {
		t.Errorf("the page says the zone is whole:\n%s", body)
	}

	// A record the app needs that was never put in place (the leader has not
	// come round yet) is put in place by the page itself, which says so.
	all, _ := a.dnsAdded().List(context.Background(), runtime.Query{Limit: 100})
	var notes []*runtime.Record
	for _, n := range all {
		if n.String("type") == "A" {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 {
		for _, n := range all {
			t.Logf("note %v", n.Values)
		}
		t.Fatalf("the A record noted once: %d of %d", len(notes), len(all))
	}
	a.dnsAdded().Delete(context.Background(), notes[0].ID())
	aID := f.find("A", "example.com")["id"].(string)
	f.mu.Lock()
	delete(f.recs, aID)
	f.mu.Unlock()
	_, body, _ = b.get("/admin/dns")
	if !strings.Contains(body, "Just added A example.com at Cloudflare.") || f.find("A", "example.com") == nil {
		t.Errorf("the page puts a pending record in place and says so:\n%s", body)
	}

	// An admin deletes the SPF: it is not added back, but offered.
	spf := f.find("TXT", "example.com")["id"].(string)
	if code, body, _ := b.post("/admin/dns", url.Values{"op": {"delete"}, "listener": {"dns"}, "id": {spf}}); code != http.StatusSeeOther {
		t.Fatalf("delete: %d\n%s", code, extractAlert(body))
	}
	a.keepDNS(context.Background())
	if f.find("TXT", "example.com") != nil {
		t.Error("a needed record an admin deleted is not added back")
	}
	_, body, _ = b.get("/admin/dns")
	if !strings.Contains(body, ">missing<") || !strings.Contains(body, `name="op" value="fulfil"`) || !strings.Contains(body, "isn&#39;t put back by itself") || strings.Contains(body, "Just added TXT") {
		t.Errorf("the deleted record is offered:\n%s", body)
	}
	if code, body, _ := b.post("/admin/dns", url.Values{"op": {"fulfil"}, "listener": {"dns"}, "need": {"0"}}); code != http.StatusSeeOther {
		t.Fatalf("put back: %d\n%s", code, extractAlert(body))
	}
	if f.find("TXT", "example.com") == nil {
		t.Error("put back on the page")
	}

	// Add and edit a record at Cloudflare.
	if code, body, _ := b.post("/admin/dns", url.Values{"op": {"add"}, "listener": {"dns"}, "type": {"CNAME"}, "name": {"www"}, "content": {"example.com"}, "proxied": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("add: %d\n%s", code, extractAlert(body))
	}
	www := f.find("CNAME", "www.example.com")
	if www == nil || www["proxied"] != true {
		t.Fatalf("added: %v", f.recs)
	}
	if _, body, _ = b.get("/admin/dns?edit=dns:" + www["id"].(string)); !strings.Contains(body, "Edit CNAME www.example.com") {
		t.Errorf("the edit form:\n%s", body)
	}
	if code, body, _ := b.post("/admin/dns", url.Values{"op": {"save"}, "listener": {"dns"}, "id": {www["id"].(string)}, "type": {"CNAME"}, "name": {"www"}, "content": {"other.example.com"}}); code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", code, extractAlert(body))
	}
	if got := f.find("CNAME", "www.example.com"); got["content"] != "other.example.com" || got["proxied"] != false {
		t.Errorf("saved: %v", got)
	}
	if code, _, _ := b.post("/admin/dns", url.Values{"op": {"add"}, "listener": {"nope"}, "type": {"A"}, "name": {"x"}, "content": {"1.2.3.4"}}); code != http.StatusBadRequest {
		t.Errorf("an unknown zone: %d", code)
	}
}

// An app keeping DNS records of its own (a class routed to the listener)
// has no DNS page: the listener is the app's.
func TestDNSPageNotForAnAppsOwnRecords(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	os.WriteFile(filepath.Join(dir, "routing", "dns.yaml"), []byte("listeners:\n  dns: {type: cloudflare}\nroutes:\n  Outing: {source: db, flows_to: [db, dns]}\n"), 0o644)
	a, _ := newServicesApp(t, dir, []byte("a-token-key-shared-by-the-nodes"))
	if a.dnsPath != "" {
		t.Errorf("a DNS page at %s", a.dnsPath)
	}
}
