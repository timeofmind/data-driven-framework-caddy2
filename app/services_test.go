package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// servicesFixture is the walkclub app routed to a bucket whose keys the
// routing file does not give: the app must start all the same, and the
// keys come from the Service Configuration page.
func servicesFixture(t *testing.T, signup bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := copyDir(fixtureApp, dir); err != nil {
		t.Fatal(err)
	}
	if !signup {
		b, _ := os.ReadFile(filepath.Join(dir, "app.yaml"))
		s := strings.Replace(string(b), "signup: {enabled: true,", "signup: {enabled: false,", 1)
		os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(s), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "routing"), 0o755)
	os.WriteFile(filepath.Join(dir, "routing", "walkclub.yaml"), []byte(fmt.Sprintf(`
listeners:
  db:     {type: sqlite, dsn: %q}
  copies: {type: s3, bucket: club-copies, give_up_after: 1h}
  coach:  {type: llm, providers: claude, claude_model: claude-haiku-5-5, max_tokens: 50, templates: llm}
  pay:    {type: payment, brand_name: "Walk Club", currency: CAD}
routes:
  default: {source: db, flows_to: [db]}
  Route: {source: db, flows_to: [db, copies]}
`, filepath.Join(dir, "club.db"))), 0o644)
	return dir
}

func newServicesApp(t *testing.T, dir string, key []byte, admins ...string) (*App, *httptest.Server) {
	t.Helper()
	if admins == nil {
		admins = []string{"admin@example.com"}
	}
	cfg := Config{AppDir: dir, Driver: "sqlite", DSN: filepath.Join(dir, "club.db"), Admins: admins,
		TokenKey: key, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	return a, srv
}

// A destination without its keys waits for them; an admin supplies them on
// the page, secrets are kept sealed and never shown again, the destination
// opens with them at once and again at the next start.
func TestServiceConfiguration(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	key := []byte("a-token-key-shared-by-the-nodes")
	a, srv := newServicesApp(t, dir, key)
	if err := a.Reg.Unconfigured("copies"); err == nil || !strings.Contains(err.Error(), "access_key and secret_key are required") {
		t.Fatalf("copies should wait for its keys: %v", err)
	}

	b := newBrowser(t, srv)
	if code, _, h := b.get("/admin/services"); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/login") {
		t.Fatalf("a visitor is sent to sign in: %d %s", code, h.Get("Location"))
	}
	b.signup("admin@example.com", "Admin")
	code, body, _ := b.get("/admin/services")
	if code != http.StatusOK {
		t.Fatalf("admin page: %d\n%s", code, body)
	}
	for _, want := range []string{"Not configured", "access_key and secret_key are required", `name="s_access_key"`, `name="s_secret_key"`, `name="s_region"`, "<dt>bucket</dt><dd>club-copies</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="s_bucket"`) {
		t.Error("a setting the routing file gives is not a field")
	}
	// A per-provider setting is offered for each provider the routing names.
	if !strings.Contains(body, `name="s_claude_api_key"`) || strings.Contains(body, "&lt;p&gt;") || strings.Contains(body, `name="s_claude_model"`) {
		t.Errorf("the llm listener's settings are expanded by provider:\n%s", body)
	}
	if err := a.Reg.Unconfigured("coach"); err == nil || !strings.Contains(err.Error(), "claude_api_key is required") {
		t.Fatalf("coach waits for its key: %v", err)
	}
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"coach"}, "s_claude_api_key": {"sk-test"}}); code != http.StatusSeeOther {
		t.Fatalf("save coach: %d\n%s", code, extractAlert(body))
	}
	if err := a.Reg.Unconfigured("coach"); err != nil {
		t.Fatalf("coach configured: %v", err)
	}

	code, body, loc := b.post("/admin/services", url.Values{"listener": {"copies"}, "s_access_key": {"AKIAEXAMPLE"}, "s_secret_key": {"very-secret-value"}, "s_region": {"us-west-2"}})
	if code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", code, extractAlert(body))
	}
	if a.Reg.Unconfigured("copies") != nil {
		t.Fatalf("configured after the save: %v", a.Reg.Unconfigured("copies"))
	}
	code, body, _ = b.get(loc)
	if code != http.StatusOK || !strings.Contains(body, "Saved copies: configured and running.") || !strings.Contains(body, "Configured and running.") {
		t.Fatalf("after the save: %d\n%s", code, body)
	}
	if strings.Contains(body, "very-secret-value") || strings.Contains(body, "AKIAEXAMPLE") {
		t.Fatal("a secret is never shown again")
	}
	if !strings.Contains(body, `value="us-west-2"`) || !strings.Contains(body, `name="clear_secret_key"`) || !strings.Contains(body, "Stored. Leave blank to keep it.") {
		t.Fatalf("a plain setting is shown, a stored secret offered to forget:\n%s", body)
	}
	// Sealed in the database.
	recs, err := a.Reg.MustClass("FrameworkServiceSetting").List(context.Background(), runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", "copies")}, Limit: 10})
	if err != nil || len(recs) != 3 {
		t.Fatalf("stored: %d %v", len(recs), err)
	}
	for _, r := range recs {
		v := r.String("value")
		switch r.String("setting") {
		case "secret_key", "access_key":
			if !strings.HasPrefix(v, "sealed:v1:") || strings.Contains(v, "very-secret") {
				t.Errorf("%s stored as %q", r.String("setting"), v)
			}
		case "region":
			if v != "us-west-2" {
				t.Errorf("region stored as %q", v)
			}
		}
	}
	// Blank secrets keep what is stored; a changed plain setting takes.
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"copies"}, "s_region": {"eu-west-1"}}); code != http.StatusSeeOther {
		t.Fatalf("second save: %d\n%s", code, extractAlert(body))
	}
	if s := a.Reg.Settings("copies"); s["region"] != "eu-west-1" || s["secret_key"] != "very-secret-value" || s["bucket"] != "club-copies" {
		t.Fatalf("settings after the second save: %v", s)
	}
	// Forgetting the secret leaves the destination waiting again.
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"copies"}, "s_region": {"eu-west-1"}, "clear_secret_key": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("forget: %d\n%s", code, extractAlert(body))
	}
	if a.Reg.Unconfigured("copies") == nil {
		t.Fatal("without its secret key the destination waits again")
	}
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"copies"}, "s_region": {"eu-west-1"}, "s_secret_key": {"another-secret"}}); code != http.StatusSeeOther {
		t.Fatalf("restore: %d\n%s", code, extractAlert(body))
	}
	if a.Reg.Unconfigured("copies") != nil {
		t.Fatal("configured once more")
	}
	// Not for members.
	m := newBrowser(t, srv)
	m.signup("bob@example.com", "Bob")
	if code, _, _ := m.get("/admin/services"); code != http.StatusForbidden {
		t.Fatalf("a member: %d", code)
	}
	if code, _, _ := m.post("/admin/services", url.Values{"_csrf": {m.csrf("/login")}, "listener": {"copies"}, "s_region": {"x"}}); code != http.StatusForbidden {
		t.Fatalf("a member's save: %d", code)
	}
	srv.Close()
	a.Close()

	// The next start opens the destination with what is stored — under the
	// same key; under another key the secrets do not open and it waits.
	a2, srv2 := newServicesApp(t, dir, key)
	if err := a2.Reg.Unconfigured("copies"); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	if s := a2.Reg.Settings("copies"); s["secret_key"] != "another-secret" || s["region"] != "eu-west-1" {
		t.Fatalf("settings after a restart: %v", s)
	}
	srv2.Close()
	a2.Close()
	a3, _ := newServicesApp(t, dir, []byte("a different key"))
	if a3.Reg.Unconfigured("copies") == nil {
		t.Fatal("another token key cannot open the sealed secrets")
	}
}

// Without a token key the server cannot keep a secret, and says so.
func TestServiceConfigurationNeedsKey(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	_, srv := newServicesApp(t, dir, nil)
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	if _, body, _ := b.get("/admin/services"); !strings.Contains(body, "no <code>token_key_file</code>") {
		t.Fatalf("the page should say secrets cannot be kept:\n%s", body)
	}
	code, body, _ := b.post("/admin/services", url.Values{"listener": {"copies"}, "s_access_key": {"AKIAEXAMPLE"}, "s_secret_key": {"shh"}})
	if code != http.StatusOK || !strings.Contains(body, "cannot be kept") {
		t.Fatalf("saving a secret without a key: %d\n%s", code, extractAlert(body))
	}
}

// An admin link signs the first admin in: with sign-up off, it leads to a
// sign-up with the login fixed; once the account exists it signs in
// straight away. A link under another key, or for a login that is not an
// admin, is refused.
func TestAdminLink(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, false)
	key := []byte("the-token-key")
	_, srv := newServicesApp(t, dir, key)
	b := newBrowser(t, srv)
	if code, _, _ := b.get("/signup"); code != http.StatusNotFound {
		t.Fatalf("sign-up is off: %d", code)
	}
	link := AdminLink(key, "Admin@Example.com", srv.URL, 0)
	if !strings.HasPrefix(link, srv.URL+"/_dd/admin?token=") {
		t.Fatalf("link %s", link)
	}
	path := strings.TrimPrefix(link, srv.URL)
	code, _, h := b.get(path)
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/signup?") {
		t.Fatalf("a first admin goes to sign-up: %d %s", code, h.Get("Location"))
	}
	signupURL := h.Get("Location")
	q, _ := url.ParseQuery(strings.TrimPrefix(signupURL, "/signup?"))
	if q.Get("email") != "admin@example.com" || q.Get("_admin") == "" || q.Get("next") != "/admin/services" {
		t.Fatalf("sign-up query %v", q)
	}
	code, body, _ := b.get(signupURL)
	if code != http.StatusOK || !strings.Contains(body, `name="_admin"`) || !strings.Contains(body, `value="admin@example.com"`) {
		t.Fatalf("the sign-up form: %d\n%s", code, body)
	}
	csrf := csrfRE.FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf")
	}
	// The form's login is ignored: the link's is used.
	code, body, loc := b.post("/signup", url.Values{"_csrf": {csrf[1]}, "_admin": {q.Get("_admin")}, "next": {"/admin/services"},
		"email": {"someone-else@example.com"}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {"First Admin"}})
	if code != http.StatusSeeOther || loc != "/admin/services" {
		t.Fatalf("admin sign-up: %d %s\n%s", code, loc, extractAlert(body))
	}
	if code, body, _ := b.get("/admin/services"); code != http.StatusOK || !strings.Contains(body, "Service Configuration") {
		t.Fatalf("the admin backend: %d\n%s", code, body)
	}
	// A second opening signs the account in.
	b2 := newBrowser(t, srv)
	if code, _, h := b2.get(path); code != http.StatusSeeOther || h.Get("Location") != "/admin/services" {
		t.Fatalf("an existing admin is signed in: %d %s", code, h.Get("Location"))
	}
	if code, _, _ := b2.get("/admin/services"); code != http.StatusOK {
		t.Fatalf("signed in by the link: %d", code)
	}
	// Refused: another key, a login that is not an admin, no token.
	b3 := newBrowser(t, srv)
	if code, _, _ := b3.get(strings.TrimPrefix(AdminLink([]byte("other"), "admin@example.com", srv.URL, 0), srv.URL)); code != http.StatusForbidden {
		t.Fatalf("another key: %d", code)
	}
	if code, _, _ := b3.get(strings.TrimPrefix(AdminLink(key, "bob@example.com", srv.URL, 0), srv.URL)); code != http.StatusForbidden {
		t.Fatalf("not an admin: %d", code)
	}
	if code, _, _ := b3.get("/_dd/admin"); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := b3.get("/signup?_admin=nonsense"); code != http.StatusNotFound {
		t.Fatalf("a bad token does not open sign-up: %d", code)
	}
}

// A listener working through providers the routing file leaves open (a
// payment listener's processors) is a card per kind: the admin ticks the
// ones in use, orders them, and gives each its own keys; the providers
// setting is derived from the cards. In test mode the listener runs with
// none; live, a ticked provider without its keys leaves it waiting.
func TestServiceConfigurationProviders(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	a, srv := newServicesApp(t, dir, []byte("a-token-key-shared-by-the-nodes"))
	if err := a.Reg.Unconfigured("pay"); err != nil {
		t.Fatalf("test mode needs nothing: %v", err)
	}
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	// The payment listener is on Payment Checkout; Service Configuration links there.
	if _, sb, _ := b.get("/admin/services"); strings.Contains(sb, `id="svc-pay"`) || !strings.Contains(sb, `href="/admin/checkout"`) {
		t.Errorf("Service Configuration leaves the processors to Payment Checkout:\n%s", sb)
	}
	code, body, _ := b.get("/admin/checkout")
	if code != http.StatusOK {
		t.Fatalf("admin page: %d\n%s", code, body)
	}
	for _, want := range []string{`id="svc-pay-stripe"`, `id="svc-pay-paypal"`, `id="svc-pay-mollie"`, `id="svc-pay-square"`, `id="svc-pay-amazonpay"`,
		`name="use_stripe"`, `name="rank_stripe"`, `name="s_stripe_secret_key"`, `name="s_paypal_client_id"`, `name="s_amazonpay_private_key"`, `<textarea id="f-s_amazonpay_private_key"`,
		`name="s_mode"`, `name="s_route_by"`, "<dt>brand_name</dt><dd>Walk Club</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="s_providers"`) || strings.Contains(body, `name="s_brand_name"`) {
		t.Error("providers is derived from the cards; brand_name is the routing's")
	}
	// A provider not in use is one line, its tick box: its settings show
	// once ticked, and ask the browser for nothing — PayPal alone saves with
	// Square's location left empty.
	if !strings.Contains(body, `<div class="provider-settings" data-dd-show-if="use_square=1" hidden>`) {
		t.Error("an unticked provider's settings are hidden until it is ticked")
	}
	if strings.Contains(body, `name="s_square_location_id" value="" required`) || !strings.Contains(body, "Required to use square.") {
		t.Error("square's location is required only for using square, not by the browser")
	}
	// Stripe and PayPal in use, PayPal first; live.
	code, body, _ = b.post("/admin/checkout", url.Values{"op": {"service"}, "listener": {"pay"}, "s_mode": {"live"},
		"use_stripe": {"1"}, "rank_stripe": {"2"}, "s_stripe_secret_key": {"sk_test_abc"},
		"use_paypal": {"1"}, "rank_paypal": {"1"}, "s_paypal_client_id": {"cid"}, "s_paypal_secret": {"shh"}, "s_paypal_environment": {"sandbox"}})
	if code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", code, extractAlert(body))
	}
	if err := a.Reg.Unconfigured("pay"); err != nil {
		t.Fatalf("configured: %v", err)
	}
	if s := a.Reg.Settings("pay"); s["providers"] != "paypal, stripe" || s["mode"] != "live" || s["stripe_secret_key"] != "sk_test_abc" || s["paypal_secret"] != "shh" {
		t.Fatalf("settings: %v", s)
	}
	code, body, _ = b.get("/admin/checkout")
	if !strings.Contains(body, `<div class="provider-settings" data-dd-show-if="use_paypal=1">`) || !strings.Contains(body, `data-dd-show-if="use_mollie=1" hidden`) {
		t.Error("a provider in use shows its settings, the others not")
	}
	if p, st, m := strings.Index(body, `id="svc-pay-paypal"`), strings.Index(body, `id="svc-pay-stripe"`), strings.Index(body, `id="svc-pay-mollie"`); !(p < st && st < m) {
		t.Error("the providers in use come first, in their order")
	}
	if code != http.StatusOK || !strings.Contains(body, `name="use_paypal" value="1" checked`) || !strings.Contains(body, `name="rank_paypal" min="1" step="1" value="1"`) || !strings.Contains(body, `name="rank_stripe" min="1" step="1" value="2"`) || strings.Contains(body, `name="use_mollie" value="1" checked`) {
		t.Fatalf("the cards after the save:\n%s", body)
	}
	if strings.Contains(body, "sk_test_abc") || strings.Contains(body, "shh") || !strings.Contains(body, `name="clear_stripe_secret_key"`) {
		for _, w := range []string{"sk_test_abc", "shh"} {
			if i := strings.Index(body, w); i >= 0 {
				t.Logf("%s at: %s", w, body[max(0, i-200):min(len(body), i+100)])
			}
		}
		t.Fatal("a provider's secret is never shown again")
	}
	// Square ticked without its token: the listener waits, and says why.
	code, body, _ = b.post("/admin/checkout", url.Values{"op": {"service"}, "listener": {"pay"}, "s_mode": {"live"}, "use_stripe": {"1"}, "use_square": {"1"}, "s_square_location_id": {"L1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("save square: %d\n%s", code, extractAlert(body))
	}
	if err := a.Reg.Unconfigured("pay"); err == nil || !strings.Contains(err.Error(), "square") || !strings.Contains(err.Error(), "access_token is required") {
		t.Fatalf("square without its token: %v", err)
	}
	if s := a.Reg.Settings("pay"); s["providers"] != "stripe, square" {
		t.Fatalf("providers: %q", s["providers"])
	}
	// Amazon Pay ticked with its key: open, and the pages admit its script.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	code, body, _ = b.post("/admin/checkout", url.Values{"op": {"service"}, "listener": {"pay"}, "s_mode": {"live"}, "use_amazonpay": {"1"},
		"s_amazonpay_merchant_id": {"A1M"}, "s_amazonpay_store_id": {"amzn1.application-oa2-client.x"}, "s_amazonpay_public_key_id": {"SANDBOX-K"}, "s_amazonpay_private_key": {pemKey}})
	if code != http.StatusSeeOther {
		t.Fatalf("save amazonpay: %d\n%s", code, extractAlert(body))
	}
	if err := a.Reg.Unconfigured("pay"); err != nil {
		t.Fatalf("amazonpay: %v", err)
	}
	if _, _, h := b.get("/"); !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self' https://static-na.payments-amazon.com") || !strings.Contains(h.Get("Content-Security-Policy"), "frame-src https://www.youtube-nocookie.com https://player.vimeo.com https://static-na.payments-amazon.com") {
		t.Fatalf("the CSP admits Amazon Pay: %s", h.Get("Content-Security-Policy"))
	}
	// Nothing ticked, back to test mode: runs again with no provider.
	if code, body, _ := b.post("/admin/checkout", url.Values{"op": {"service"}, "listener": {"pay"}, "s_mode": {"test"}}); code != http.StatusSeeOther {
		t.Fatalf("save test: %d\n%s", code, extractAlert(body))
	}
	if err := a.Reg.Unconfigured("pay"); err != nil {
		t.Fatalf("test mode again: %v", err)
	}
	if s := a.Reg.Settings("pay"); s["providers"] != "" {
		t.Fatalf("providers forgotten: %q", s["providers"])
	}
	if _, _, h := b.get("/"); strings.Contains(h.Get("Content-Security-Policy"), "amazon") {
		t.Fatalf("the CSP is the app's own again: %s", h.Get("Content-Security-Policy"))
	}
}

// A card shows only its mandatory settings; the others fold under Advanced.
// A setting that is one of a few choices is a select, and the settings of
// the other choices are drawn hidden (showif.js switches them): the image
// listener keeps its pictures in the media library unless a bucket is chosen.
func TestServiceConfigurationAdvancedAndChoices(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	t.Setenv("DD_MEDIA_TOKEN", "dmt_test")
	dir := servicesFixture(t, true)
	os.WriteFile(filepath.Join(dir, "routing", "walkclub.yaml"), []byte(fmt.Sprintf(`
listeners:
  db:       {type: sqlite, dsn: %q}
  pictures: {type: image, providers: openai}
routes:
  default: {source: db, flows_to: [db]}
`, filepath.Join(dir, "club.db"))), 0o644)
	_, srv := newServicesApp(t, dir, []byte("a-token-key-shared-by-the-nodes"))
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	code, body, _ := b.get("/admin/services")
	if code != http.StatusOK {
		t.Fatalf("admin page: %d\n%s", code, body)
	}
	card := body[strings.Index(body, `id="svc-pictures"`):]
	adv := strings.Index(card, `<details class="advanced"`)
	if adv < 0 {
		t.Fatalf("no Advanced fold:\n%s", card)
	}
	if k := strings.Index(card, `name="s_openai_api_key"`); k < 0 || k > adv {
		t.Errorf("the provider's key is a mandatory setting, shown before the fold")
	}
	if s := strings.Index(card, `name="s_storage"`); s < adv || !strings.Contains(card, `<select id="f-s_storage" name="s_storage"`) || !strings.Contains(card, `<option value="media" selected>`) {
		t.Errorf("storage is an optional choice, a select under Advanced, media chosen")
	}
	for _, f := range []string{"s_bucket", "s_access_key", "s_endpoint"} {
		i := strings.Index(card, `data-dd-field="`+f+`"`)
		if i < 0 {
			t.Errorf("%s is drawn", f)
			continue
		}
		if tag := card[i : i+strings.Index(card[i:], ">")]; !strings.Contains(tag, "data-dd-show-if=") || !strings.Contains(tag, " hidden") {
			t.Errorf("%s belongs to a bucket choice and is hidden while the media library is chosen: %s", f, tag)
		}
	}
	if !strings.Contains(body, "/_dd/showif") {
		t.Error("the page loads showif.js")
	}
}

// Settings saved on one node reach the app's other nodes (sharing its
// database): each reads them again on the Service Configuration page and
// every settingsEvery, and opens the destination with them.
func TestServiceSettingsReachOtherNodes(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	key := []byte("a-token-key-shared-by-the-nodes")
	a, srv := newServicesApp(t, dir, key)
	other, srv2 := newServicesApp(t, dir, key)
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"coach"}, "s_claude_api_key": {"sk-test"}}); code != http.StatusSeeOther {
		t.Fatalf("save coach: %d\n%s", code, extractAlert(body))
	}
	if a.Reg.Unconfigured("coach") != nil || other.Reg.Unconfigured("coach") == nil {
		t.Fatalf("the save opens coach on its node alone, at first: %v / %v", a.Reg.Unconfigured("coach"), other.Reg.Unconfigured("coach"))
	}
	// The other node's page reads the settings again before showing them.
	b2 := *b // the same signed-in admin (cookies go by host, not port)
	b2.base = srv2.URL
	code, body, _ := b2.get("/admin/services")
	if code != http.StatusOK || other.Reg.Unconfigured("coach") != nil {
		t.Fatalf("the other node's page: %d %v", code, other.Reg.Unconfigured("coach"))
	}
	if strings.Contains(body, "claude_api_key is required") {
		t.Fatal("the page shows coach as configured")
	}
	// A setting forgotten on the first node: the other node's pass closes it.
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"coach"}, "clear_claude_api_key": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("forget: %d\n%s", code, extractAlert(body))
	}
	other.syncSettings(context.Background())
	if other.Reg.Unconfigured("coach") == nil {
		t.Fatal("the other node waits again once the key is forgotten")
	}
}

// With a change log (as every published app runs), a stored setting reaches
// the database a moment after the save: the save opens the destination with
// what it stored, not with a read of the database that may not show it yet.
func TestServiceSaveOpensWithLog(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	cfg := Config{AppDir: dir, Driver: "sqlite", DSN: filepath.Join(dir, "club.db"), Admins: []string{"admin@example.com"},
		TokenKey: []byte("a-token-key-shared-by-the-nodes"), LogDir: filepath.Join(dir, "log"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	b := newBrowser(t, srv)
	b.signup("admin@example.com", "Admin")
	for i, k := range []string{"sk-first", "sk-second"} {
		if code, body, _ := b.post("/admin/services", url.Values{"listener": {"coach"}, "s_claude_api_key": {k}}); code != http.StatusSeeOther {
			t.Fatalf("save %d: %d\n%s", i, code, extractAlert(body))
		}
		if err := a.Reg.Unconfigured("coach"); err != nil {
			t.Fatalf("save %d: the destination opens with the key just saved: %v", i, err)
		}
		if got := a.Reg.Settings("coach")["claude_api_key"]; got != k {
			t.Fatalf("save %d: opened with %q", i, got)
		}
	}
	// Forgetting it closes it again at once.
	if code, body, _ := b.post("/admin/services", url.Values{"listener": {"coach"}, "clear_claude_api_key": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("forget: %d\n%s", code, extractAlert(body))
	}
	if a.Reg.Unconfigured("coach") == nil {
		t.Fatal("forgotten: waits for a key again")
	}
}
