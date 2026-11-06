package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// The tests run the handler against testdata/walkclub, a small fixture app
// (app.yaml + schema + theme). Nothing here knows about any real application.
const fixtureApp = "testdata/walkclub"

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	return newTestAppAt(t, fixtureApp)
}

func newTestAppAt(t *testing.T, dir string) (*App, *httptest.Server) {
	t.Helper()
	cfg := Config{
		AppDir: dir, Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "app.db"),
		Admins: []string{"admin@example.com"}, UploadDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if dsn := os.Getenv("DDF_TEST_MYSQL_DSN"); dsn != "" {
		cfg.Driver, cfg.DSN = "mysql", dsn
		resetMySQL(t, dsn)
	}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	return a, srv
}

// browser is a cookie-keeping client that never follows redirects.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: srv.URL, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(path string) (int, string, http.Header) {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

var csrfRE = regexp.MustCompile(`name="_csrf" value="([0-9a-f]{64})"`)

func (b *browser) csrf(path string) string {
	b.t.Helper()
	_, body, _ := b.get(path)
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		b.t.Fatalf("no CSRF token on %s:\n%s", path, body)
	}
	return m[1]
}

func (b *browser) post(path string, form url.Values) (int, string, string) {
	b.t.Helper()
	if form.Get("_csrf") == "" {
		form.Set("_csrf", b.csrf(path))
	}
	resp, err := b.c.PostForm(b.base+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header.Get("Location")
}

func (b *browser) signup(email, name string) string {
	b.t.Helper()
	code, body, loc := b.post("/signup", url.Values{"email": {email}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {name}})
	if code != http.StatusSeeOther {
		b.t.Fatalf("signup %s: %d\n%s", email, code, extractAlert(body))
	}
	return loc
}

func (b *browser) create(path string, form url.Values) string {
	b.t.Helper()
	code, body, loc := b.post(path, form)
	if code != http.StatusSeeOther {
		b.t.Fatalf("create %s: %d %s", path, code, extractAlert(body))
	}
	return loc
}

func (b *browser) upload(files map[string][]byte) string {
	b.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("_csrf", b.csrf("/upload"))
	for name, data := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write(data)
	}
	mw.Close()
	resp, err := b.c.Post(b.base+"/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		b.t.Fatalf("upload: %d %s", resp.StatusCode, extractAlert(string(body)))
	}
	return string(body)
}

func extractAlert(body string) string {
	var out []string
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`<p class="alert">(.*?)</p>`), regexp.MustCompile(`<p class="err">(.*?)</p>`), regexp.MustCompile(`<h1>(.*?)</h1>`)} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
	}
	return strings.Join(out, " | ")
}

func idOf(loc string) string { return loc[strings.LastIndex(loc, "/")+1:] }

func mustID(loc string) int64 { n, _ := strconv.ParseInt(idOf(loc), 10, 64); return n }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAccountsAccessAndForms(t *testing.T) {
	_, srv := newTestApp(t)
	ann := newBrowser(t, srv)
	me := ann.signup("Ann@Example.com", "Ann")
	if !strings.HasPrefix(me, "/c/members/") {
		t.Fatalf("signup redirect %q", me)
	}
	if code, body, _ := ann.get(me); code != 200 || !strings.Contains(body, "Ann") || strings.Contains(body, "ann@example.com") {
		t.Fatalf("profile: %d, hidden login leaked: %v", code, strings.Contains(body, "ann@example.com"))
	}
	other := newBrowser(t, srv)
	code, body, _ := other.post("/signup", url.Values{"email": {"ann@example.com"}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {"X"}})
	if code != 200 || !strings.Contains(body, "already has an account") {
		t.Fatalf("duplicate signup: %d %s", code, extractAlert(body))
	}

	private := ann.create("/c/routes/new", url.Values{"name": {"Secret loop"}, "distance_km": {"5"}})
	public := ann.create("/c/routes/new", url.Values{"name": {"Harbour walk"}, "distance_km": {"8"}, "visibility": {"public"}})
	anon := newBrowser(t, srv)
	if code, _, _ := anon.get(private); code != 404 {
		t.Errorf("anonymous sees a private row: %d", code)
	}
	if code, _, _ := anon.get(public); code != 200 {
		t.Errorf("anonymous cannot see a public row: %d", code)
	}
	if _, list, _ := anon.get("/c/routes"); strings.Contains(list, "Secret loop") || !strings.Contains(list, "Harbour walk") {
		t.Error("list leaks a private row or misses a public one")
	}

	ben := newBrowser(t, srv)
	ben.signup("ben@example.com", "Ben")
	if code, _, _ := ben.get(private + "/edit"); code != 404 {
		t.Errorf("ben opens ann's private editor: %d", code)
	}
	if code, _, _ := ben.get(public + "/edit"); code != 403 {
		t.Errorf("ben edits ann's public row: %d", code)
	}
	if code, _, _ := ben.post(public+"/delete", url.Values{"_csrf": {ben.csrf("/c/routes/new")}}); code != 403 {
		t.Errorf("ben deletes ann's row: %d", code)
	}
	code, body, _ = ben.post("/c/walks/new", url.Values{"route_id": {idOf(private)}, "day": {"2026-11-05"}, "title": {"x"}})
	if code != 200 || !strings.Contains(body, "does not exist") {
		t.Errorf("linked to a private row: %d %s", code, extractAlert(body))
	}
	code, body, _ = ben.post("/c/walks/new", url.Values{"route_id": {idOf(public)}, "day": {"2026-11-05"}, "title": {"x"}})
	if code != 200 || !strings.Contains(body, "must be one of your own") {
		t.Errorf("linked to someone else's row: %d %s", code, extractAlert(body))
	}
	resp, _ := ben.c.PostForm(srv.URL+"/c/routes/new", url.Values{"name": {"x"}})
	if resp.StatusCode != 403 {
		t.Errorf("missing CSRF accepted: %d", resp.StatusCode)
	}

	code, body, _ = ann.post("/c/walks/new", url.Values{"day": {"2026-11-02"}, "title": {"Evening"}, "distance_km": {"-1"}, "duration": {"1:75"}})
	if code != 200 || !strings.Contains(body, "must be at least 0") || !strings.Contains(body, "must be a duration") {
		t.Errorf("validation: %d %s", code, extractAlert(body))
	}
	walk := ann.create("/c/walks/new", url.Values{"day": {"2026-11-02"}, "title": {"Evening"}, "distance_km": {"8"}, "duration": {"1:36:00"}, "route_id": {idOf(public)}})
	if _, wp, _ := ann.get(walk); !strings.Contains(wp, "1:36:00") || !strings.Contains(wp, "12:00 /km") {
		t.Errorf("duration display or computed field missing")
	}
	if _, rp, _ := ann.get(public); !strings.Contains(rp, "2026-11-02") {
		t.Error("trigger did not update the linked route")
	}

	ann.post("/logout", url.Values{"_csrf": {ann.csrf("/c/routes/new")}})
	if code, _, h := ann.get("/c/routes/new"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Errorf("signed-out create should redirect to login: %d", code)
	}
	code, _, loc := ann.post("/login", url.Values{"login": {"ANN@example.com"}, "password": {"long enough pw"}, "next": {"/calendar"}})
	if code != 303 || loc != "/calendar" {
		t.Fatalf("login: %d %q", code, loc)
	}
	if code, body, _ := ann.post("/login", url.Values{"login": {"ann@example.com"}, "password": {"wrong"}}); code != 200 || !strings.Contains(body, "match an account") {
		t.Errorf("bad password: %d", code)
	}
}

func TestThemeAndManifest(t *testing.T) {
	_, srv := newTestApp(t)
	b := newBrowser(t, srv)
	_, home, _ := b.get("/")
	if !strings.Contains(home, `data-theme="walkclub"`) || !strings.Contains(home, "/static/club.css") {
		t.Error("the app's theme layout was not used")
	}
	if code, css, h := b.get("/static/club.css"); code != 200 || !strings.Contains(css, "serif") || !strings.HasPrefix(h.Get("Content-Type"), "text/css") {
		t.Errorf("theme static file: %d", code)
	}
	if code, _, _ := b.get("/static/../app.yaml"); code == 200 {
		t.Error("static path traversal")
	}
	if _, signup, _ := b.get("/signup"); !strings.Contains(signup, "Join the club.") {
		t.Error("signup intro from app.yaml missing")
	}

	// An app without a theme falls back to the built-in default.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "schema"), 0o755)
	for _, f := range []string{"club.yaml", "library.yaml", "rules.yaml"} {
		src, _ := os.ReadFile(filepath.Join(fixtureApp, "schema", f))
		os.WriteFile(filepath.Join(dir, "schema", f), src, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("name: Bare\n"), 0o644)
	_, srv2 := newTestAppAt(t, dir)
	b2 := newBrowser(t, srv2)
	if _, h, _ := b2.get("/"); !strings.Contains(h, "/static/style.css") || !strings.Contains(h, "Bare") {
		t.Error("default theme not used for an app without one")
	}
	if code, _, _ := b2.get("/static/style.css"); code != 200 {
		t.Error("default stylesheet missing")
	}
}

func fitFile(t *testing.T, start time.Time, km float64, secs float64) []byte {
	t.Helper()
	act := filedef.NewActivity()
	act.FileId = *mesgdef.NewFileId(nil).SetType(typedef.FileActivity).SetManufacturer(typedef.ManufacturerCoros).
		SetProductName("PACE 3").SetTimeCreated(start)
	act.Activity = mesgdef.NewActivity(nil).SetTimestamp(start.Add(time.Duration(secs) * time.Second)).SetNumSessions(1)
	half := secs / 2
	for i := 0; i < 2; i++ {
		ls := start.Add(time.Duration(float64(i)*half) * time.Second)
		act.Laps = append(act.Laps, mesgdef.NewLap(nil).SetTimestamp(ls.Add(time.Duration(half)*time.Second)).SetStartTime(ls).
			SetTotalElapsedTimeScaled(half).SetTotalTimerTimeScaled(half).SetTotalDistanceScaled(km*500))
	}
	act.Sessions = append(act.Sessions, mesgdef.NewSession(nil).SetTimestamp(start.Add(time.Duration(secs)*time.Second)).
		SetStartTime(start).SetSport(typedef.SportWalking).SetTotalElapsedTimeScaled(secs).SetTotalTimerTimeScaled(secs).
		SetTotalDistanceScaled(km*1000).SetFirstLapIndex(0).SetNumLaps(2))
	fit := act.ToFIT(nil)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gpxFile(start time.Time, km float64, secs float64) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><gpx creator="test" version="1.1" xmlns="http://www.topografix.com/GPX/1/1"><trk><type>walking</type><trkseg>`)
	n := 60
	for i := 0; i <= n; i++ {
		f := float64(i) / float64(n)
		fmt.Fprintf(&b, `<trkpt lat="%.6f" lon="-123.4"><time>%s</time></trkpt>`, 48.65+km/111.195*f, start.Add(time.Duration(secs*f)*time.Second).Format(time.RFC3339))
	}
	b.WriteString(`</trkseg></trk></gpx>`)
	return []byte(b.String())
}

func TestUploadsDrivenBySchema(t *testing.T) {
	a, srv := newTestApp(t)
	org := newBrowser(t, srv)
	org.signup("org@example.com", "Organiser")
	meetup := org.create("/c/meetups/new", url.Values{"title": {"Sunday stroll"}, "day": {"2026-11-03"}, "open": {"1"}})
	closed := org.create("/c/meetups/new", url.Values{"title": {"Closed one"}, "day": {"2026-11-04"}})

	w := newBrowser(t, srv)
	w.signup("walker@example.com", "Wendy")
	route := w.create("/c/routes/new", url.Values{"name": {"Seafront"}, "distance_km": {"6"}})
	if _, mp, _ := w.get(meetup); !strings.Contains(mp, ">Attend<") {
		t.Fatal("add_label from the schema not shown")
	}
	if _, cp, _ := w.get(closed); strings.Contains(cp, ">Attend<") {
		t.Error("add_when false (closed meetup) still offers Attend")
	}
	w.create("/c/attendances/new?meetup_id="+idOf(meetup), url.Values{"meetup_id": {idOf(meetup)}})
	if _, mp, _ := w.get(meetup); strings.Contains(mp, ">Attend<") {
		t.Error("Attend still offered after attending (unique meetup+member)")
	}

	// 6.02 km in 1:12:00 on Nov 3, Vancouver evening (UTC next day).
	start := time.Date(2026, 11, 4, 1, 30, 0, 0, time.UTC)
	res := w.upload(map[string][]byte{"walk.fit": fitFile(t, start, 6.02, 4320)})
	if !strings.Contains(res, "imported") || !strings.Contains(res, "Seafront") || !strings.Contains(res, "Sunday stroll") {
		t.Fatalf("upload did not import / link / create per the schema:\n%s", res)
	}
	if res := w.upload(map[string][]byte{"again.fit": fitFile(t, start, 6.02, 4320)}); !strings.Contains(res, "already uploaded") {
		t.Error("dedupe failed")
	}
	if res := w.upload(map[string][]byte{"route.tcx": []byte(`<?xml version="1.0"?><TrainingCenterDatabase><Activities><Activity Sport="Walking"><Id>2026-11-03T10:00:00Z</Id><Lap StartTime="2026-11-03T10:00:00Z"><TotalTimeSeconds>60</TotalTimeSeconds><DistanceMeters>100</DistanceMeters></Lap></Activity></Activities></TrainingCenterDatabase>`)}); !strings.Contains(res, "not a supported file") {
		t.Error("a format the class does not import was accepted")
	}

	ctx := context.Background()
	walks, _ := a.Reg.MustClass("Walk").List(ctx, runtime.Query{})
	if len(walks) != 1 {
		t.Fatalf("%d walks", len(walks))
	}
	wk := walks[0]
	if wk.String("day") != "2026-11-03" || wk.String("kind") != "other" || wk.Float("distance_km") != 6.02 || wk.Int("duration") != 4320 {
		t.Errorf("mapped fields: %v", wk.Values)
	}
	if legs, _ := a.Reg.MustClass("Leg").List(ctx, runtime.Query{}); len(legs) != 2 {
		t.Errorf("%d legs", len(legs))
	}
	if r, _ := a.Reg.MustClass("Route").Load(ctx, mustID(route)); r.String("last_walked") != "2026-11-03" {
		t.Error("trigger on the imported record did not run")
	}

	// A faster GPX walk by another attendee on the same day ranks first.
	z := newBrowser(t, srv)
	z.signup("zed@example.com", "Zed")
	z.create("/c/attendances/new?meetup_id="+idOf(meetup), url.Values{"meetup_id": {idOf(meetup)}})
	z.upload(map[string][]byte{"z.gpx": gpxFile(time.Date(2026, 11, 3, 17, 0, 0, 0, time.UTC), 6, 3000)})
	anon := newBrowser(t, srv)
	_, board, _ := anon.get(meetup)
	zi, wi := strings.Index(board, "Zed"), strings.Index(board, "Wendy")
	if !strings.Contains(board, "Best times") || zi < 0 || wi < 0 || zi > wi || !strings.Contains(board, `class="num rank">1<`) {
		t.Errorf("ranked board wrong (zed %d, wendy %d)", zi, wi)
	}
	if _, legsAnon, _ := anon.get("/api/v1/legs"); !strings.Contains(legsAnon, `"total":0`) {
		t.Errorf("parent-governed rows leaked: %s", legsAnon)
	}
	_, cal, _ := w.get("/calendar?month=2026-11")
	if !strings.Contains(cal, "6 km") {
		t.Error("calendar_label not applied")
	}
	if !strings.Contains(cal, "kind-other") {
		t.Error("enum CSS class missing on calendar items")
	}
}

func TestAPI(t *testing.T) {
	_, srv := newTestApp(t)
	b := newBrowser(t, srv)
	b.signup("api@example.com", "Api")
	resp, err := b.c.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	var me struct {
		Authenticated bool   `json:"authenticated"`
		CSRF          string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	do := func(method, path, body, token string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := do("POST", "/api/v1/walks", `{"day":"2026-11-02","title":"Lunch"}`, ""); code != 403 {
		t.Errorf("write without CSRF header: %d", code)
	}
	code, out := do("POST", "/api/v1/walks", `{"day":"2026-11-02","title":"Lunch","distance_km":5,"duration":"50:00","member_id":999,"source_hash":"x"}`, me.CSRF)
	if code != 201 || out["duration"] != float64(3000) || out["source_hash"] != nil {
		t.Fatalf("create: %d %v", code, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if code, out := do("PATCH", "/api/v1/walks/"+id, `{"distance_km":-3}`, me.CSRF); code != 422 || out["fields"] == nil {
		t.Errorf("validation: %d %v", code, out)
	}
	if code, _ := do("DELETE", "/api/v1/walks/"+id, ``, me.CSRF); code != 204 {
		t.Errorf("delete: %d", code)
	}
	resp, _ = http.Get(srv.URL + "/api/v1/openapi.json")
	var spec map[string]any
	json.NewDecoder(resp.Body).Decode(&spec)
	resp.Body.Close()
	if spec["info"].(map[string]any)["title"] != "Walk Club" {
		t.Error("openapi title should come from app.yaml")
	}
}

func TestDeleteAccountCascades(t *testing.T) {
	a, srv := newTestApp(t)
	b := newBrowser(t, srv)
	me := b.signup("gone@example.com", "Gone")
	b.create("/c/routes/new", url.Values{"name": {"x"}})
	if _, body, _ := b.get(me + "/delete"); !strings.Contains(body, "Delete your account?") {
		t.Fatal("no account-deletion confirmation")
	}
	if code, _, _ := b.post(me+"/delete", url.Values{}); code != 303 {
		t.Fatalf("delete account: %d", code)
	}
	if n, _ := a.Reg.MustClass("Route").Count(context.Background(), runtime.Query{}); n != 0 {
		t.Errorf("rows left after account deletion: %d", n)
	}
}

func TestPassThroughUnknownPaths(t *testing.T) {
	a, _ := newTestApp(t)
	if a.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", "/robots.txt", nil)) {
		t.Error("app claimed /robots.txt")
	}
}

// resetMySQL drops every table in the scratch database named by dsn.
func resetMySQL(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0")
	rows, err := conn.QueryContext(ctx, "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE()")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := conn.ExecContext(ctx, "DROP TABLE `"+n+"`"); err != nil {
			t.Fatal(err)
		}
	}
}
