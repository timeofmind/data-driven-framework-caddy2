package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	uaPhone   = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	uaAndroid = "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36"
	uaTablet  = "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	uaDesktop = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

// phoneMark is what the test's mobile layout carries, and the theme's does not.
const phoneMark = `id="phone-layout"`

// themeBody opens the body in the fixture's theme/layout.html.
const themeBody = `<body data-theme="walkclub">`

// mobileApp serves mobileAppDir(files).
func mobileApp(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	_, srv := newTestAppAt(t, mobileAppDir(t, files))
	return srv
}

// mobileAppDir is the fixture app copied aside, with mobile/ holding files
// (nil: no mobile directory). "layout.html": "" stands for the theme's layout
// with phoneMark and the switch link in its body.
func mobileAppDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.CopyFS(dir, os.DirFS(fixtureApp)); err != nil {
		t.Fatal(err)
	}
	if files != nil {
		if err := os.MkdirAll(filepath.Join(dir, "mobile"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if name == "layout.html" && body == "" {
			b, err := os.ReadFile(filepath.Join(dir, "theme", "layout.html"))
			if err != nil {
				t.Fatal(err)
			}
			body = strings.Replace(string(b), themeBody, themeBody+`<a `+phoneMark+` href="{{.SwitchURL}}">Desktop site</a>`, 1)
			if body == string(b) {
				t.Fatal("the fixture's layout has no " + themeBody)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "mobile", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// uaGet fetches path as a browser with that User-Agent (redirects not followed).
func uaGet(t *testing.T, srv *httptest.Server, path, ua string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("User-Agent", ua)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// An app with mobile/ templates: a phone's User-Agent gets its pages — the
// app's and the handler's own (sign-in, sign-up) — drawn with them, at the
// same addresses; a desktop and a tablet get the theme's; ?dd_view= chooses,
// and the choice is kept in a cookie.
func TestMobilePages(t *testing.T) {
	srv := mobileApp(t, map[string]string{"layout.html": ""})
	for _, c := range []struct {
		name, path, ua string
		phone          bool
	}{
		{"iPhone, a page", "/", uaPhone, true},
		{"Android phone, a page", "/", uaAndroid, true},
		{"iPhone, sign-in", "/login", uaPhone, true},
		{"iPhone, sign-up", "/signup", uaPhone, true},
		{"desktop, a page", "/", uaDesktop, false},
		{"desktop, sign-in", "/login", uaDesktop, false},
		{"tablet, a page", "/", uaTablet, false},
		{"no User-Agent", "/", "", false},
		{"iPhone asking for the desktop site", "/?dd_view=desktop", uaPhone, false},
		{"desktop asking for the mobile site", "/login?dd_view=mobile", uaDesktop, true},
	} {
		resp, body := uaGet(t, srv, c.path, c.ua)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", c.name, resp.StatusCode)
		}
		if got := strings.Contains(body, phoneMark); got != c.phone {
			t.Errorf("%s: mobile templates = %v, want %v", c.name, got, c.phone)
		}
		if v := strings.Join(resp.Header.Values("Vary"), ","); !strings.Contains(v, "User-Agent") || !strings.Contains(v, "Cookie") {
			t.Errorf("%s: Vary = %q", c.name, v)
		}
	}
	// the switch link is this page the other way
	if _, body := uaGet(t, srv, "/login", uaPhone); !strings.Contains(body, `href="/login?dd_view=desktop"`) {
		t.Errorf("switch link: %s", body)
	}
	// the choice is kept
	resp, _ := uaGet(t, srv, "/?dd_view=desktop", uaPhone)
	var kept *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "dd_view" {
			kept = c
		}
	}
	if kept == nil || kept.Value != "desktop" || kept.Path != "/" {
		t.Fatalf("dd_view cookie: %v", resp.Cookies())
	}
	if _, body := uaGet(t, srv, "/login", uaPhone, kept); strings.Contains(body, phoneMark) {
		t.Error("a phone that chose the desktop site got the mobile templates")
	}
}

// An app with no mobile templates — no mobile/, an empty one, one with no
// .html — draws a phone's pages as anyone's: the theme's, no Vary, and
// ?dd_view= neither obeyed nor kept. A mobile/ with only some templates
// leaves the rest to the theme.
func TestMobileFallback(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no mobile directory":    nil,
		"an empty mobile":        {},
		"a mobile with no .html": {"README.txt": "nothing to draw with"},
	} {
		srv := mobileApp(t, files)
		for _, path := range []string{"/", "/login", "/login?dd_view=mobile"} {
			resp, body := uaGet(t, srv, path, uaPhone)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "</html>") {
				t.Errorf("%s: %s: status %d: %s", name, path, resp.StatusCode, body)
			}
			if strings.Contains(body, "dd_view") || len(resp.Header.Values("Vary")) != 0 {
				t.Errorf("%s: %s: not the default pages (Vary %q)", name, path, resp.Header.Values("Vary"))
			}
			for _, c := range resp.Cookies() {
				if c.Name == "dd_view" {
					t.Errorf("%s: %s: a choice kept with nothing to choose", name, path)
				}
			}
		}
	}
	// only a view's template in mobile/: the layout is the theme's, on a phone too
	srv := mobileApp(t, map[string]string{"iv-text.html": `{{define "iv-text"}}<p id="phone-text"></p>{{end}}`})
	_, desk := uaGet(t, srv, "/login", uaDesktop)
	resp, body := uaGet(t, srv, "/login", uaPhone)
	if resp.StatusCode != http.StatusOK || len(body) != len(desk) || strings.Contains(body, phoneMark) {
		t.Errorf("a mobile with one view's template: status %d, %d bytes (desktop %d)", resp.StatusCode, len(body), len(desk))
	}
}

// Under a base path the switch link and the cookie stay inside it.
func TestMobileBasePath(t *testing.T) {
	dir := mobileAppDir(t, map[string]string{"layout.html": ""})
	a, err := New(t.Context(), Config{AppDir: dir, BasePath: "/club", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "app.db"),
		UploadDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	resp, body := uaGet(t, srv, "/club/login?dd_view=mobile", uaDesktop)
	if !strings.Contains(body, `href="/club/login?dd_view=desktop"`) {
		t.Errorf("switch link: %s", body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dd_view" && c.Path != "/club/" {
			t.Errorf("cookie path %q", c.Path)
		}
	}
}
