package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// fakeProvider is an OpenID Connect provider: it answers discovery, and its
// token endpoint gives the ID token the test set for the next sign-in, if
// the code, the secret and the PKCE verifier are right.
type fakeProvider struct {
	srv       *httptest.Server
	claims    map[string]any // the next ID token's
	challenge string         // the code challenge the browser was sent with
}

func newFakeProvider(t *testing.T) *fakeProvider {
	f := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("client_secret") != "s3cret" || r.PostForm.Get("code") != "the-code" || r.PostForm.Get("client_id") != "client-1" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || !strings.HasSuffix(r.PostForm.Get("redirect_uri"), "/_dd/signin/oidc/callback") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		body, _ := json.Marshal(f.claims)
		tok := "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
		json.NewEncoder(w).Encode(map[string]string{"id_token": tok, "access_token": "x"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// through takes a browser through a sign-in with the provider as the person
// the claims describe, and returns the callback's answer.
func (f *fakeProvider) through(t *testing.T, b *browser, next string, claims map[string]any) (int, string, http.Header) {
	t.Helper()
	path := "/_dd/signin/oidc"
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	code, body, h := b.get(path)
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), f.srv.URL+"/auth?") {
		t.Fatalf("the start of a sign-in: %d %s\n%s", code, h.Get("Location"), body)
	}
	u, _ := url.Parse(h.Get("Location"))
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "email") {
		t.Fatalf("what the provider was asked: %v", q)
	}
	f.challenge = q.Get("code_challenge")
	f.claims = map[string]any{"iss": f.srv.URL, "aud": "client-1", "exp": time.Now().Add(time.Minute).Unix(), "nonce": q.Get("nonce")}
	for k, v := range claims {
		f.claims[k] = v
	}
	return b.get("/_dd/signin/oidc/callback?code=the-code&state=" + q.Get("state"))
}

// Sign-in providers: an app whose routing names a signin listener has the
// admin page they are set up on; offered on the sign-in and sign-up pages from then on; accounts
// matched by the verified email.
func TestSigninProviders(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	// An app whose routing names no signin listener has no Sign-in page.
	{
		dir := servicesFixture(t, true)
		os.RemoveAll(filepath.Join(dir, "theme"))
		_, srv := newServicesApp(t, dir, []byte("0123456789abcdef0123456789abcdef"))
		owner := newBrowser(t, srv)
		owner.signup("admin@example.com", "Owner")
		if code, _, _ := owner.get("/admin/sign-in"); code != http.StatusNotFound {
			t.Fatalf("the page without the listener: %d", code)
		}
		if _, body, _ := owner.get("/admin/admins"); strings.Contains(body, ">Sign-in</a>") {
			t.Fatalf("the admin area names the page without the listener:\n%s", body)
		}
		if code, _, _ := owner.get("/_dd/signin/oidc"); code != http.StatusNotFound {
			t.Fatalf("a sign-in without the listener: %d", code)
		}
	}
	// The routing names one: the page is there, and not on the services page.
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	rf := filepath.Join(dir, "routing", "walkclub.yaml")
	rb, _ := os.ReadFile(rf)
	os.WriteFile(rf, []byte(strings.Replace(string(rb), "routes:", "  people: {type: signin}\nroutes:", 1)), 0o644)
	a, srv := newServicesApp(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	idp := newFakeProvider(t)
	a.signinHTTP = http.DefaultClient
	ctx := context.Background()
	owner, bea := newBrowser(t, srv), newBrowser(t, srv)
	owner.signup("admin@example.com", "Owner")
	bea.signup("bea@example.com", "Bea")

	if code, _, _ := newBrowser(t, srv).get("/_dd/signin/oidc"); code != http.StatusNotFound {
		t.Fatalf("a sign-in before any is set up: %d", code)
	}
	if _, body, _ := owner.get("/admin/services"); strings.Contains(body, `id="svc-people"`) {
		t.Fatalf("the signin listener is on the services page:\n%s", body)
	}
	if code, _, _ := bea.get("/admin/sign-in"); code != http.StatusForbidden {
		t.Fatalf("a member at the page: %d", code)
	}
	code, body, _ := owner.get("/admin/sign-in")
	if code != http.StatusOK || !strings.Contains(body, ">Sign-in</a>") || !strings.Contains(body, srv.URL+"/_dd/signin/google/callback") || !strings.Contains(body, srv.URL+"/_dd/signin/oidc/callback") {
		t.Fatalf("the page: %d\n%s", code, body)
	}
	if _, body, _ := newBrowser(t, srv).get("/login"); strings.Contains(body, "Continue with") {
		t.Fatalf("offered before it is set up:\n%s", body)
	}

	// An issuer that is no provider is refused; a real one is kept, its secret sealed.
	save := url.Values{"provider": {"oidc"}, "action": {"save"}, "label": {"Acme ID"}, "issuer": {srv.URL}, "client_id": {"client-1"}, "client_secret": {"s3cret"}, "enabled": {"1"}}
	if code, body, _ := owner.post("/admin/sign-in", save); code != http.StatusOK || !strings.Contains(body, "does not answer as an OpenID Connect provider") {
		t.Fatalf("an issuer that is none: %d\n%s", code, body)
	}
	save.Set("issuer", idp.srv.URL+"/")
	save.Del("_csrf")
	code, _, loc := owner.post("/admin/sign-in", save)
	if code != http.StatusSeeOther {
		t.Fatalf("saving: %d", code)
	}
	if _, body, _ := owner.get(strings.TrimPrefix(loc, srv.URL)); !strings.Contains(body, "now offer “Continue with Acme ID”") || strings.Contains(body, "s3cret") {
		t.Fatalf("after saving:\n%s", body)
	}
	st, err := a.settingsClass().First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", "signin:oidc"), runtime.Eq("setting", "client_secret")}})
	if err != nil || !strings.HasPrefix(st.String("value"), sealedPrefix) {
		t.Fatalf("the secret as kept: %v %v", st, err)
	}
	for _, p := range []string{"/login", "/signup"} {
		if _, body, _ := newBrowser(t, srv).get(p + "?next=/c/walks"); !strings.Contains(body, `href="/_dd/signin/oidc?next=%2Fc%2Fwalks"`) || !strings.Contains(body, "Continue with Acme ID") {
			t.Fatalf("%s does not offer it:\n%s", p, body)
		}
	}

	// Someone new: an account is made for the verified address, and found again by the provider's id.
	cat := newBrowser(t, srv)
	code, body, h := idp.through(t, cat, "/c/walks", map[string]any{"sub": "cat-1", "email": "Cat@Example.com", "email_verified": true, "name": "Cat  Flap"})
	if code != http.StatusSeeOther || h.Get("Location") != "/c/walks" {
		t.Fatalf("a new person: %d %s\n%s", code, h.Get("Location"), body)
	}
	acc, err := a.auth.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("email", "cat@example.com")}})
	if err != nil || acc.String("name") != "Cat Flap" {
		t.Fatalf("the new account: %v %v", acc, err)
	}
	if code, _, h := cat.get("/me"); code != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "/"+itoa(acc.ID())) {
		t.Fatalf("signed in as: %d %s", code, h.Get("Location"))
	}
	again := newBrowser(t, srv)
	if code, _, _ := idp.through(t, again, "", map[string]any{"sub": "cat-1", "email": "cat.new@example.com", "email_verified": "true"}); code != http.StatusSeeOther {
		t.Fatalf("the same person again: %d", code)
	}
	if n, _ := a.auth.Count(ctx, runtime.Query{}); n != 3 {
		t.Fatalf("accounts: %d, want 3", n)
	}
	if code, _, h := again.get("/me"); code != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "/"+itoa(acc.ID())) {
		t.Fatalf("the same person is another account: %s", h.Get("Location"))
	}

	// An address the provider has not verified is nobody's.
	if code, body, _ := idp.through(t, newBrowser(t, srv), "", map[string]any{"sub": "dan-1", "email": "dan@example.com"}); code != http.StatusForbidden || !strings.Contains(body, "has verified") {
		t.Fatalf("an unverified address: %d\n%s", code, body)
	}
	// An admin does not sign in this way, account or not.
	if code, body, _ := idp.through(t, newBrowser(t, srv), "", map[string]any{"sub": "own-1", "email": "admin@example.com", "email_verified": true}); code != http.StatusForbidden || !strings.Contains(body, "their own sign-in link") {
		t.Fatalf("an owner: %d\n%s", code, body)
	}
	// A token for another client, or another sign-in, is refused.
	if code, _, _ := idp.through(t, newBrowser(t, srv), "", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true, "aud": "someone-else"}); code != http.StatusBadGateway {
		t.Fatalf("another client's token: %d", code)
	}
	if code, _, _ := idp.through(t, newBrowser(t, srv), "", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true, "nonce": "stale"}); code != http.StatusBadGateway {
		t.Fatalf("another sign-in's token: %d", code)
	}
	stray := newBrowser(t, srv)
	stray.get("/_dd/signin/oidc")
	if code, _, _ := stray.get("/_dd/signin/oidc/callback?code=the-code&state=" + strings.Repeat("0", 64)); code != http.StatusBadRequest {
		t.Fatalf("a callback with another state: %d", code)
	}

	// An account that existed keeps its password: the provider is joined to
	// it once that password is given, and both ways in work from then on.
	bea2 := newBrowser(t, srv)
	code, body, _ = idp.through(t, bea2, "/c/walks", map[string]any{"sub": "bea-1", "email": "bea@example.com", "email_verified": true})
	if code != http.StatusOK || !strings.Contains(body, "Enter its password once") {
		t.Fatalf("an existing account: %d\n%s", code, body)
	}
	if code, _, h := bea2.get("/me"); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/login") {
		t.Fatalf("signed in before giving the password: %d %s", code, h.Get("Location"))
	}
	tok := csrfRE.FindStringSubmatch(body)[1]
	if code, body, _ := bea2.post("/_dd/signin/oidc/join", url.Values{"_csrf": {tok}, "password": {"not it"}}); code != http.StatusOK || !strings.Contains(body, "not this account") {
		t.Fatalf("a wrong password: %d\n%s", code, body)
	}
	if code, _, loc := bea2.post("/_dd/signin/oidc/join", url.Values{"_csrf": {tok}, "password": {"long enough pw"}}); code != http.StatusSeeOther || loc != "/c/walks" {
		t.Fatalf("joining: %d %s", code, loc)
	}
	if code, _, h := bea2.get("/me"); code != http.StatusSeeOther || strings.Contains(h.Get("Location"), "/login") {
		t.Fatalf("not signed in after joining: %d %s", code, h.Get("Location"))
	}
	if code, _, h := bea.get("/me"); code != http.StatusSeeOther || strings.Contains(h.Get("Location"), "/login") {
		t.Fatalf("the session it had was ended: %d %s", code, h.Get("Location"))
	}
	if code, _, _ := newBrowser(t, srv).post("/login", url.Values{"login": {"bea@example.com"}, "password": {"long enough pw"}}); code != http.StatusSeeOther {
		t.Fatalf("the password no longer works: %d", code)
	}
	if code, _, _ := idp.through(t, newBrowser(t, srv), "", map[string]any{"sub": "bea-1", "email": "bea@example.com", "email_verified": true}); code != http.StatusSeeOther {
		t.Fatalf("the joined account, by the provider: %d", code)
	}
	lone := newBrowser(t, srv)
	if code, _, _ := lone.post("/_dd/signin/oidc/join", url.Values{"_csrf": {lone.csrf("/login")}, "password": {"long enough pw"}}); code != http.StatusBadRequest {
		t.Fatalf("joining with no sign-in behind it: %d", code)
	}

	// Switched off, then removed: no longer offered; the accounts stay.
	save.Del("_csrf")
	save.Del("enabled")
	save.Set("client_secret", "")
	owner.post("/admin/sign-in", save)
	if _, body, _ := newBrowser(t, srv).get("/login"); strings.Contains(body, "Continue with") {
		t.Fatalf("offered while switched off:\n%s", body)
	}
	if _, body, _ := owner.get("/admin/sign-in"); !strings.Contains(body, "Set up, but switched off") || !strings.Contains(body, "2 accounts have signed in with it") {
		t.Fatalf("switched off:\n%s", body)
	}
	owner.post("/admin/sign-in", url.Values{"provider": {"oidc"}, "action": {"forget"}})
	if _, err := a.settingsClass().First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", "signin:oidc")}}); err == nil {
		t.Fatal("settings are left after removing the provider")
	}
}
