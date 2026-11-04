package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

const siteSchema = "../sites/athleticstogether/schema/athleticstogether.yaml"

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	cfg := Config{
		SchemaFiles: []string{siteSchema}, Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "app.db"),
		SiteName: "Athletics Together", AllowSignup: true, Admins: []string{"admin@example.com"},
		UploadDir: t.TempDir(), DefaultTimezone: "America/Vancouver",
		Import: ImportConfig{Activity: "Activity", Lap: "Lap", Match: "PlannedWorkout", Registration: "EventRegistration", Result: "EventResult"},
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

// csrf loads a page and returns its form token.
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
	code, body, loc := b.post("/signup", url.Values{"email": {email}, "password_hash": {"long enough pw"}, "password_confirm": {"long enough pw"}, "display_name": {name}})
	if code != http.StatusSeeOther {
		b.t.Fatalf("signup %s: %d\n%s", email, code, body)
	}
	return loc
}

// create posts a generated form and returns the new record's path.
func (b *browser) create(path string, form url.Values) string {
	b.t.Helper()
	code, body, loc := b.post(path, form)
	if code != http.StatusSeeOther {
		b.t.Fatalf("create %s: %d\n%s", path, code, extractAlert(body))
	}
	return loc
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

func idOf(t *testing.T, loc string) string {
	t.Helper()
	i := strings.LastIndex(loc, "/")
	return loc[i+1:]
}

func TestSignupLoginAndPrivacy(t *testing.T) {
	_, srv := newTestApp(t)
	alice := newBrowser(t, srv)
	me := alice.signup("Alice@Example.com", "Alice")
	if !strings.HasPrefix(me, "/c/athletes/") {
		t.Fatalf("signup redirect %q", me)
	}
	if code, body, _ := alice.get(me); code != 200 || !strings.Contains(body, "Alice") || strings.Contains(body, "alice@example.com") {
		t.Fatalf("profile page: %d, email leaked: %v", code, strings.Contains(body, "alice@example.com"))
	}
	// Duplicate email, case-insensitive.
	other := newBrowser(t, srv)
	code, body, _ := other.post("/signup", url.Values{"email": {"alice@example.com"}, "password_hash": {"long enough pw"}, "password_confirm": {"long enough pw"}, "display_name": {"X"}})
	if code != 200 || !strings.Contains(body, "already has an account") {
		t.Fatalf("duplicate signup: %d %s", code, extractAlert(body))
	}

	plan := alice.create("/c/training-plans/new", url.Values{"name": {"Spring 10K"}, "start_date": {"2026-11-01"}, "visibility": {"private"}})
	pub := alice.create("/c/training-plans/new", url.Values{"name": {"Open Marathon Plan"}, "start_date": {"2026-11-01"}, "visibility": {"public"}})

	anon := newBrowser(t, srv)
	if code, _, _ := anon.get(plan); code != 404 {
		t.Errorf("anonymous sees private plan: %d", code)
	}
	if code, body, _ := anon.get(pub); code != 200 || !strings.Contains(body, "Open Marathon Plan") {
		t.Errorf("anonymous cannot see public plan: %d", code)
	}
	_, list, _ := anon.get("/c/training-plans")
	if strings.Contains(list, "Spring 10K") || !strings.Contains(list, "Open Marathon Plan") {
		t.Error("list leaks private plan or misses public one")
	}

	bob := newBrowser(t, srv)
	bob.signup("bob@example.com", "Bob")
	if code, _, _ := bob.get(plan + "/edit"); code != 404 {
		t.Errorf("bob can open alice's private plan editor: %d", code)
	}
	if code, _, _ := bob.get(pub + "/edit"); code != 403 {
		t.Errorf("bob can edit alice's public plan: %d", code)
	}
	if code, _, _ := bob.post(pub+"/delete", url.Values{}); code != 403 {
		t.Errorf("bob can delete alice's plan: %d", code)
	}
	// Bob cannot attach a workout to Alice's plan by guessing its id.
	code, body, _ = bob.post("/c/planned-workouts/new", url.Values{"plan_id": {idOf(t, plan)}, "date": {"2026-11-05"}, "title": {"sneaky"}})
	if code != 200 || !strings.Contains(body, "does not exist") {
		t.Errorf("bob linked to a private plan: %d %s", code, extractAlert(body))
	}
	code, body, _ = bob.post("/c/planned-workouts/new", url.Values{"plan_id": {idOf(t, pub)}, "date": {"2026-11-05"}, "title": {"sneaky"}})
	if code != 200 || !strings.Contains(body, "must be one of your own") {
		t.Errorf("bob linked to alice's public plan: %d %s", code, extractAlert(body))
	}

	// CSRF: a post without the token is refused.
	resp, _ := bob.c.PostForm(srv.URL+"/c/training-plans/new", url.Values{"name": {"x"}, "start_date": {"2026-11-01"}})
	if resp.StatusCode != 403 {
		t.Errorf("missing CSRF accepted: %d", resp.StatusCode)
	}

	// Sign out, then sign back in with the original-case email.
	alice.post("/logout", url.Values{})
	if code, _, loc := alice.get("/c/training-plans/new"); code != 303 || !strings.HasPrefix(loc.Get("Location"), "/login") {
		t.Errorf("signed-out create should redirect to login, got %d", code)
	}
	code, _, loc := alice.post("/login", url.Values{"login": {"ALICE@example.com"}, "password": {"long enough pw"}, "next": {"/calendar"}})
	if code != 303 || loc != "/calendar" {
		t.Fatalf("login: %d %q", code, loc)
	}
	code, body, _ = alice.post("/login", url.Values{"login": {"alice@example.com"}, "password": {"wrong"}})
	if code != 200 || !strings.Contains(body, "match an account") {
		t.Errorf("bad password: %d", code)
	}
	if code, _, _ := alice.get(plan); code != 200 {
		t.Errorf("alice cannot see her own plan after re-login: %d", code)
	}
}

func fitFile(t *testing.T, start time.Time, km float64, secs float64) []byte {
	t.Helper()
	act := filedef.NewActivity()
	act.FileId = *mesgdef.NewFileId(nil).SetType(typedef.FileActivity).SetManufacturer(typedef.ManufacturerGarmin).
		SetProduct(uint16(typedef.GarminProductFr255)).SetTimeCreated(start)
	act.Activity = mesgdef.NewActivity(nil).SetTimestamp(start.Add(time.Duration(secs) * time.Second)).SetNumSessions(1)
	half := secs / 2
	for i := 0; i < 2; i++ {
		ls := start.Add(time.Duration(float64(i)*half) * time.Second)
		act.Laps = append(act.Laps, mesgdef.NewLap(nil).SetTimestamp(ls.Add(time.Duration(half)*time.Second)).SetStartTime(ls).
			SetTotalElapsedTimeScaled(half).SetTotalTimerTimeScaled(half).SetTotalDistanceScaled(km*500).SetAvgHeartRate(150))
	}
	act.Sessions = append(act.Sessions, mesgdef.NewSession(nil).SetTimestamp(start.Add(time.Duration(secs)*time.Second)).
		SetStartTime(start).SetSport(typedef.SportRunning).SetTotalElapsedTimeScaled(secs).SetTotalTimerTimeScaled(secs).
		SetTotalDistanceScaled(km*1000).SetAvgHeartRate(150).SetMaxHeartRate(171).SetFirstLapIndex(0).SetNumLaps(2))
	fit := act.ToFIT(nil)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (b *browser) upload(files map[string][]byte) string {
	b.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("_csrf", b.csrf("/import"))
	for name, data := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write(data)
	}
	mw.Close()
	resp, err := b.c.Post(b.base+"/import", mw.FormDataContentType(), &buf)
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

func TestPlanUploadMatchAndVirtualEvent(t *testing.T) {
	a, srv := newTestApp(t)
	org := newBrowser(t, srv)
	org.signup("race@example.com", "Race Director")
	event := org.create("/c/events/new", url.Values{"name": {"November Virtual 10K"}, "sport": {"run"}, "distance_km": {"10"},
		"start_date": {"2026-11-01"}, "end_date": {"2026-11-30"}, "registration_open": {"1"}, "visibility": {"public"}})

	runner := newBrowser(t, srv)
	runner.signup("runner@example.com", "Rhea Runner")
	plan := runner.create("/c/training-plans/new", url.Values{"name": {"10K block"}, "start_date": {"2026-11-01"}})
	// The plan page offers "Add workout" prefilled with the plan.
	if _, body, _ := runner.get(plan); !strings.Contains(body, "/c/planned-workouts/new?plan_id="+idOf(t, plan)) {
		t.Fatal("plan page lacks prefilled add-workout link")
	}
	workout := runner.create("/c/planned-workouts/new?plan_id="+idOf(t, plan), url.Values{"plan_id": {idOf(t, plan)},
		"date": {"2026-11-03"}, "title": {"10K time trial"}, "sport": {"run"}, "workout_type": {"race"},
		"target_distance_km": {"10"}, "target_duration": {"45:00"}})
	if _, body, _ := runner.get(workout); !strings.Contains(body, "45:00") || !strings.Contains(body, "Planned") {
		t.Errorf("workout page should show target time 45:00 and status planned")
	}

	// Register for the event from its page.
	_, evPage, _ := runner.get(event)
	if !strings.Contains(evPage, ">Register<") {
		t.Fatal("event page has no Register button")
	}
	runner.create("/c/event-registrations/new?event_id="+idOf(t, event), url.Values{"event_id": {idOf(t, event)}, "team": {"Peninsula Striders"}})
	if _, evPage, _ := runner.get(event); strings.Contains(evPage, ">Register<") {
		t.Error("Register still offered after registering")
	}
	code, body, _ := runner.post("/c/event-registrations/new", url.Values{"event_id": {idOf(t, event)}})
	if code != 200 || !strings.Contains(body, "already exists") {
		t.Errorf("double registration: %d %s", code, extractAlert(body))
	}

	// Upload a watch file: 10.02 km in 44:10 on the evening of Nov 3 (Vancouver time, so Nov 4 UTC).
	start := time.Date(2026, 11, 4, 1, 30, 0, 0, time.UTC)
	res := runner.upload(map[string][]byte{"Garmin_10k.fit": fitFile(t, start, 10.02, 2650)})
	if !strings.Contains(res, "imported") || !strings.Contains(res, "10K time trial") || !strings.Contains(res, "November Virtual 10K") {
		t.Fatalf("upload result missing import/match/event:\n%s", res)
	}
	// Re-uploading the same file is recognised.
	if res := runner.upload(map[string][]byte{"again.fit": fitFile(t, start, 10.02, 2650)}); !strings.Contains(res, "already uploaded") {
		t.Errorf("duplicate upload not detected")
	}

	ctx := context.Background()
	acts, _ := a.Reg.MustClass("Activity").List(ctx, queryAll())
	if len(acts) != 1 {
		t.Fatalf("%d activities", len(acts))
	}
	act := acts[0]
	if act.String("date") != "2026-11-03" || act.String("start_time") != "2026-11-03 18:30:00" || act.String("title") != "Evening Run" {
		t.Errorf("local placement: %s %s %q", act.String("date"), act.String("start_time"), act.String("title"))
	}
	if act.String("device") != "Garmin Forerunner 255" || act.Float("distance_km") != 10.02 || act.Int("duration") != 2650 || act.String("source") != "fit" {
		t.Errorf("activity fields: %+v", act.Values)
	}
	if laps, _ := a.Reg.MustClass("Lap").List(ctx, queryAll()); len(laps) != 2 {
		t.Errorf("%d laps", len(laps))
	}
	pw, _ := a.Reg.MustClass("PlannedWorkout").Load(ctx, act.Int("planned_workout_id"))
	if pw == nil || pw.String("status") != "completed" {
		t.Errorf("planned workout not completed: %+v", pw)
	}
	// The activity page shows pace; the event page shows the leaderboard with a rank.
	_, actPage, _ := runner.get("/c/activities/" + itoa(act.ID()))
	if !strings.Contains(actPage, "4:24 /km") || !strings.Contains(actPage, "Laps") {
		t.Errorf("activity page missing pace or laps")
	}
	anon := newBrowser(t, srv)
	_, board, _ := anon.get(event)
	if !strings.Contains(board, "Leaderboard") || !strings.Contains(board, "Rhea Runner") || !strings.Contains(board, "44:10") || !strings.Contains(board, `class="num rank">1<`) {
		t.Errorf("leaderboard not shown to the public:\n%s", board)
	}
	// The activity itself is private by default: the result links to it but the public sees "private".
	if code, _, _ := anon.get("/c/activities/" + itoa(act.ID())); code != 404 {
		t.Errorf("private activity visible to anonymous: %d", code)
	}
	if code, _, _ := anon.get("/c/laps"); code != 404 && code != 200 {
		t.Errorf("laps list: %d", code)
	}
	_, lapsAnon, _ := anon.get("/api/v1/laps")
	if !strings.Contains(lapsAnon, `"total":0`) {
		t.Errorf("anonymous API sees laps of a private activity: %s", lapsAnon)
	}

	// The combined calendar shows the plan and the log together.
	_, cal, _ := runner.get("/calendar?month=2026-11")
	if !strings.Contains(cal, "10K time trial") || !strings.Contains(cal, "Evening Run") || !strings.Contains(cal, "status-completed") {
		t.Errorf("calendar missing items")
	}
}

func TestAPI(t *testing.T) {
	_, srv := newTestApp(t)
	b := newBrowser(t, srv)
	b.signup("api@example.com", "Api User")
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
	if !me.Authenticated || len(me.CSRF) != 64 {
		t.Fatalf("me: %+v", me)
	}
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
	if code, _ := do("POST", "/api/v1/activities", `{"date":"2026-11-02","title":"Lunch run"}`, ""); code != 403 {
		t.Errorf("API write without CSRF header: %d", code)
	}
	code, out := do("POST", "/api/v1/activities", `{"date":"2026-11-02","title":"Lunch run","distance_km":5.2,"duration":"26:00","athlete_id":999,"source":"fit"}`, me.CSRF)
	if code != 201 || out["duration"] != float64(1560) || out["source"] != "manual" {
		t.Fatalf("API create: %d %v", code, out)
	}
	if _, leaked := out["athlete_id"]; leaked {
		t.Error("hidden owner slot in API output")
	}
	id := itoa(int64(out["id"].(float64)))
	code, out = do("PATCH", "/api/v1/activities/"+id, `{"perceived_effort":11}`, me.CSRF)
	if code != 422 || out["fields"] == nil {
		t.Errorf("API validation: %d %v", code, out)
	}
	if code, _ := do("DELETE", "/api/v1/activities/"+id, ``, me.CSRF); code != 204 {
		t.Errorf("API delete: %d", code)
	}
	resp, _ = http.Get(srv.URL + "/api/v1/openapi.json")
	var spec map[string]any
	json.NewDecoder(resp.Body).Decode(&spec)
	resp.Body.Close()
	if _, ok := spec["paths"].(map[string]any)["/api/v1/planned-workouts/{id}"]; !ok {
		t.Error("openapi missing planned-workouts path")
	}
}

func TestDeleteAccountCascades(t *testing.T) {
	a, srv := newTestApp(t)
	b := newBrowser(t, srv)
	me := b.signup("gone@example.com", "Gone Soon")
	b.create("/c/training-plans/new", url.Values{"name": {"x"}, "start_date": {"2026-11-01"}})
	if _, body, _ := b.get(me + "/delete"); !strings.Contains(body, "Delete your account?") {
		t.Fatal("no account-deletion confirmation")
	}
	if code, _, _ := b.post(me+"/delete", url.Values{}); code != 303 {
		t.Fatalf("delete account: %d", code)
	}
	if n, _ := a.Reg.MustClass("TrainingPlan").Count(context.Background(), queryAll()); n != 0 {
		t.Errorf("plans left after account deletion: %d", n)
	}
	if code, _, _ := b.get("/calendar"); code != 303 {
		t.Errorf("session should be gone: %d", code)
	}
}

func TestPassThroughUnknownPaths(t *testing.T) {
	a, _ := newTestApp(t)
	rec := httptest.NewRecorder()
	if a.Handle(rec, httptest.NewRequest("GET", "/robots.txt", nil)) {
		t.Error("app claimed /robots.txt; it should fall through to the next handler")
	}
	if !a.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", "/static/style.css", nil)) {
		t.Error("stylesheet not served")
	}
}

func queryAll() runtime.Query { return runtime.Query{} }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// resetMySQL drops every table in the scratch database named by dsn.
func resetMySQL(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
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
