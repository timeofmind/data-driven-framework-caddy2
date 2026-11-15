package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeMedia is dd-media and the bucket's slot endpoint for the page's calls.
type fakeMedia struct {
	srv, bucket *httptest.Server
	slots       []map[string]any
	uploaded    [][]byte
	fields      []map[string]string
	completed   []string
	deleted     []string
}

func newFakeMedia(t *testing.T) *fakeMedia {
	f := &fakeMedia{}
	f.bucket = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		got := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			got[k] = v[0]
		}
		fh := r.MultipartForm.File["file"]
		if len(fh) != 1 {
			w.WriteHeader(400)
			return
		}
		fr, _ := fh[0].Open()
		b, _ := io.ReadAll(fr)
		f.uploaded = append(f.uploaded, b)
		f.fields = append(f.fields, got)
		w.WriteHeader(204)
	}))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dmt_node" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"unauthorized","message":"unknown media token"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/uploads":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			f.slots = append(f.slots, req)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"media_id": "med_t1", "status": "pending", "url": "https://media.test/apps/a/app_1/t1-logo.png",
				"upload": map[string]any{"method": "POST", "url": f.bucket.URL + "/", "fields": map[string]string{"key": "incoming/med_t1", "Content-Type": "image/png", "policy": "p"}}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/complete"):
			f.completed = append(f.completed, r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"media_id": "med_t1", "status": "ready", "url": "https://media.test/apps/a/app_1/t1-logo.png", "content_type": "image/png", "size": 9, "width": 1, "height": 1})
		case r.Method == "GET" && r.URL.Path == "/v1/media":
			json.NewEncoder(w).Encode(map[string]any{"media": []map[string]any{{"media_id": "med_t1", "url": "https://media.test/apps/a/app_1/t1-logo.png", "filename": "logo.png", "content_type": "image/png", "size": 9}},
				"usage": map[string]any{"files": 1, "bytes": 9, "max_files": 2000, "max_bytes": 5368709120}, "filter": r.URL.Query().Get("type")})
		case r.Method == "DELETE":
			f.deleted = append(f.deleted, r.URL.Path)
			io.WriteString(w, `{"media_id":"med_t1","status":"deleted"}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no such file"}}`)
		}
	}))
	t.Cleanup(func() { f.srv.Close(); f.bucket.Close() })
	return f
}

var dataCSRF = regexp.MustCompile(`data-csrf="([0-9a-f]{64})"`)

// call sends a JSON call with the browser's cookies and the CSRF header.
func (b *browser) call(method, path, body, csrf string, hdr map[string]string) (int, string) {
	b.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// The Media page is the admin area's; its calls reach dd-media with the
// node's token, a file is streamed to the slot and completed, and the
// admin navigation lists the server's sections and the app's (admin.yaml).
func TestMediaPage(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	f := newFakeMedia(t)
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme")) // the built-in layout: the fixture's theme has its own frame
	os.WriteFile(filepath.Join(dir, "admin.yaml"), []byte("sections:\n  - {label: Walks, path: /c/walks}\n"), 0o644)
	cfg := Config{AppDir: dir, Driver: "sqlite", DSN: filepath.Join(dir, "club.db"), Admins: []string{"admin@example.com"},
		TokenKey: []byte("0123456789abcdef0123456789abcdef"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MediaURL: f.srv.URL, MediaToken: "dmt_node"}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	defer a.Close()
	if got := sortedAnchors(a.site.AdminSections()); strings.Join(got, "|") != "Admins /admin/admins|Media /admin/media|Services /admin/services|Walks /c/walks" {
		t.Fatalf("sections %v", got)
	}

	b := newBrowser(t, srv)
	if code, _, h := b.get("/admin/media"); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/login") {
		t.Fatalf("a visitor is sent to sign in: %d %s", code, h.Get("Location"))
	}
	b.signup("admin@example.com", "Admin")
	code, body, h := b.get("/admin/media")
	if code != 200 || !strings.Contains(body, `id="dd-media"`) || !strings.Contains(body, `class="wrap admin-nav"`) || !strings.Contains(body, ">Walks</a>") || !strings.Contains(body, "/_dd/media.") {
		t.Fatalf("page %d: %s", code, body[:min(len(body), 800)])
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data: https:") {
		t.Fatalf("csp %s", csp)
	}
	m := dataCSRF.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token on the page")
	}
	csrf := m[1]
	// the services page carries the same nav
	if _, body, _ = b.get("/admin/services"); !strings.Contains(body, ">Media</a>") || !strings.Contains(body, ">Walks</a>") {
		t.Fatal("the services page has no admin nav")
	}

	// the library, as dd-media lists it
	code, body = b.call("GET", "/_dd/media?type=image", "", "", nil)
	if code != 200 || !strings.Contains(body, `"med_t1"`) || !strings.Contains(body, `"filter":"image"`) {
		t.Fatalf("list %d %s", code, body)
	}
	// a slot: dd-media's, with an upload token in place of the presigned POST
	code, body = b.call("POST", "/_dd/media/slot", `{"filename":"logo.png","content_type":"image/png","size":9}`, csrf, nil)
	if code != 201 {
		t.Fatalf("slot %d %s", code, body)
	}
	var slot struct {
		MediaID string `json:"media_id"`
		Token   string `json:"upload_token"`
	}
	json.Unmarshal([]byte(body), &slot)
	if slot.MediaID != "med_t1" || slot.Token == "" || strings.Contains(body, "policy") || f.slots[0]["login"] != "Admin" {
		t.Fatalf("slot %s (login %v)", body, f.slots[0]["login"])
	}
	// the file, streamed on to the bucket with the slot's fields
	req, _ := http.NewRequest("POST", srv.URL+"/_dd/media/med_t1/upload?name=logo.png", bytes.NewReader([]byte("PNG-bytes")))
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-Media-Slot", slot.Token)
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(f.uploaded) != 1 || string(f.uploaded[0]) != "PNG-bytes" || f.fields[0]["policy"] != "p" || f.fields[0]["key"] != "incoming/med_t1" {
		t.Fatalf("upload %d %s: %v %v", resp.StatusCode, rb, f.uploaded, f.fields)
	}
	// another file's token is refused
	if code, _ = b.call("POST", "/_dd/media/med_other/upload", "x", csrf, map[string]string{"X-Media-Slot": slot.Token, "Content-Type": "image/png"}); code != 400 {
		t.Fatalf("another file's token: %d", code)
	}
	// the completion and a delete, relayed
	code, body = b.call("POST", "/_dd/media/med_t1/complete", "", csrf, nil)
	if code != 200 || !strings.Contains(body, `"ready"`) || len(f.completed) != 1 {
		t.Fatalf("complete %d %s", code, body)
	}
	if code, _ = b.call("DELETE", "/_dd/media/med_t1", "", csrf, nil); code != 200 || len(f.deleted) != 1 {
		t.Fatalf("delete %d", code)
	}
	// without the CSRF token, or as a member, nothing
	if code, _ = b.call("DELETE", "/_dd/media/med_t1", "", "", nil); code != 403 {
		t.Fatalf("no csrf: %d", code)
	}
	b2 := newBrowser(t, srv)
	b2.signup("member@example.com", "Member")
	if code, _ = b2.call("GET", "/_dd/media", "", "", nil); code != 403 {
		t.Fatalf("member: %d", code)
	}
	if code, _, _ = b2.get("/admin/media"); code != 403 {
		t.Fatalf("member page: %d", code)
	}
}

// Without a token the page says the library is not available, and the
// sections fall back to the app's pages under /admin/.
func TestMediaUnavailable(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	os.WriteFile(filepath.Join(dir, "pages", "admin-attendances.yaml"), []byte("path: /admin/attendances\ntitle: Attendances admin\naccess: admin\ninterface_views:\n  - {type: list, name: list, data: {class: Attendance}, fields: [note]}\n"), 0o644)
	a, srv := newServicesApp(t, dir, nil)
	if got := sortedAnchors(a.site.AdminSections()); strings.Join(got, "|") != "Admins /admin/admins|Attendances admin /admin/attendances|Media /admin/media|Services /admin/services" {
		t.Fatalf("sections %v", got)
	}
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	code, body, _ := b.get("/admin/media")
	if code != 200 || !strings.Contains(body, "no media token") || strings.Contains(body, `id="dd-media"`) {
		t.Fatalf("%d %s", code, body[:min(len(body), 400)])
	}
	if code, _ = b.call("GET", "/_dd/media", "", "", nil); code != 503 {
		t.Fatalf("api without a token: %d", code)
	}
	// the app's own admin page carries the nav too
	if code, body, _ = b.get("/admin/attendances"); code != 200 || !strings.Contains(body, `class="wrap admin-nav"`) {
		t.Fatalf("app admin page %d: nav missing", code)
	}
}
