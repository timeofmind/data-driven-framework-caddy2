package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bot Protection: with bot_defence, every page asks for the edge's solver
// script and the admin area has the page; it lists where the app takes writes,
// all protected, and an admin opts a path out (and back in), kept in the app's
// data where the builder reads it. Without bot_defence, neither.
func TestBotProtection(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	cfg := Config{AppDir: dir, Driver: "sqlite", DSN: filepath.Join(dir, "club.db"), Admins: []string{"admin@example.com"},
		TokenKey: []byte("0123456789abcdef0123456789abcdef"), BotDefence: "/siteprotect/hashcash.js",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	owner, bea := newBrowser(t, srv), newBrowser(t, srv)
	owner.signup("admin@example.com", "Owner")
	bea.signup("bea@example.com", "Bea")

	tag := `<script src="/siteprotect/hashcash.js" async></script>`
	if _, body, _ := bea.get("/"); !strings.Contains(body, tag) {
		t.Fatalf("the home page has no solver:\n%s", body)
	}
	if code, _, _ := bea.get(protectPath); code != http.StatusForbidden {
		t.Fatalf("a member at the page: %d", code)
	}
	code, body, _ := owner.get(protectPath)
	if code != http.StatusOK || !strings.Contains(body, ">Bot Protection</a>") || !strings.Contains(body, "<code>/login</code>") ||
		!strings.Contains(body, "<code>/api/v1/*</code>") || !strings.Contains(body, `value="/login" aria-label="Protect /login" checked`) {
		t.Fatalf("the page: %d\n%s", code, body)
	}
	// opt the API out, and a path of a partner's
	var protect []string
	for _, r := range a.writeEndpoints() {
		if r.Path != "/api/v1/*" {
			protect = append(protect, r.Path)
		}
	}
	listed := []string{}
	for _, r := range a.writeEndpoints() {
		listed = append(listed, r.Path)
	}
	code, _, loc := owner.post(protectPath, url.Values{"listed": listed, "protect": protect, "other": {"/hooks/partner"}})
	if u, _ := url.Parse(loc); code != http.StatusSeeOther || !strings.Contains(u.Query().Get("done"), "Opted out: /api/v1/*, /hooks/partner") {
		t.Fatalf("opting out: %d %s", code, loc)
	}
	ex, _ := a.exemptPaths(context.Background())
	if len(ex) != 2 || ex["/api/v1/*"] == nil || ex["/hooks/partner"].String("added_by") != "admin@example.com" {
		t.Fatalf("kept: %v", ex)
	}
	if _, body, _ = owner.get(protectPath); !strings.Contains(body, `value="/api/v1/*" aria-label="Protect /api/v1/*">`) || !strings.Contains(body, "<code>/hooks/partner</code>") {
		t.Fatalf("after opting out:\n%s", body)
	}
	// a path that isn't one is refused
	if _, body, _ = owner.post(protectPath, url.Values{"other": {"/x {\n}"}}); !strings.Contains(body, "is not a path") {
		t.Fatalf("a bad path:\n%s", body)
	}
	// both protected again
	code, _, loc = owner.post(protectPath, url.Values{"listed": append(listed, "/hooks/partner"), "protect": append(protect, "/api/v1/*", "/hooks/partner")})
	if ex, _ := a.exemptPaths(context.Background()); code != http.StatusSeeOther || len(ex) != 0 || !strings.Contains(loc, "Protected+again") {
		t.Fatalf("protected again: %d %v %s", code, ex, loc)
	}

	// without bot_defence: no solver, no page
	plain, psrv := newServicesApp(t, servicesFixture(t, true), []byte("0123456789abcdef0123456789abcdef"))
	_ = plain
	pb := newBrowser(t, psrv)
	pb.signup("admin@example.com", "Owner")
	if _, body, _ := pb.get("/"); strings.Contains(body, "hashcash") {
		t.Fatal("a solver without bot_defence")
	}
	if code, _, _ := pb.get(protectPath); code == http.StatusOK {
		t.Fatal("the page without bot_defence")
	}
}

func TestPagePattern(t *testing.T) {
	for in, want := range map[string]string{"/recipes/{id}": "/recipes/*", "/": "/", "/c/{club}/walks/{id}/": "/c/*/walks/*", "/about": "/about"} {
		if got := pagePattern(in); got != want {
			t.Errorf("pagePattern(%q) = %q, want %q", in, got, want)
		}
	}
}
