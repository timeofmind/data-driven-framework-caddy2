package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/timeofmind/data-driven-framework-go/listener/signin"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Sign-in providers: people sign in to an app with an account they already
// have elsewhere — Google, or any OpenID Connect provider — instead of a
// password of their own.
//
// An app has it when its routing names a signin listener (listener/signin:
// one that carries no data) — placed there, like any listener's entry, by
// whoever sets the app up; the server decides nothing about it. Such an
// app's admin area has the Sign-in page (signinPath), where an owner sets
// the providers up; an app without the listener has no page and offers no
// provider. What the owner enters — the client ID,
// the client secret (sealed) — is kept with the service settings
// (FrameworkServiceSetting, under "signin:<provider>"). From then on the
// sign-in and sign-up pages offer "Continue with …".
//
// The flow is OpenID Connect's authorization code flow with PKCE:
// /_dd/signin/<provider> sends the browser to the provider with a state, a
// nonce and a code challenge (kept sealed in a short-lived cookie), and
// /_dd/signin/<provider>/callback exchanges the code it comes back with for
// an ID token, straight from the provider's token endpoint over TLS (which
// is what vouches for the token: OpenID Connect Core 3.1.3.7), and checks
// its issuer, audience, expiry and nonce.
//
// Accounts are matched by the email address the provider says it has
// verified; an unverified one is refused. The provider's own id for the
// person is kept (FrameworkIdentity), so a later sign-in finds the account
// whatever its email has become. Someone with no account gets one where
// the app takes sign-ups. Someone who has one keeps its password, and from
// then on chooses at every sign-in between it and the provider — but the
// first time, the provider is joined to the account only once they have
// given that password: the app never checked who made the account, so the
// address alone does not show it is theirs. Admins are refused: they sign
// in through their own links (adminlogin.go).

const (
	signinPath      = "/admin/sign-in"
	signinPathTaken = "/admin/_sign-in" // where it goes in an app with a page of its own at signinPath
	signinRoute     = "/_dd/signin/"    // + <provider>, and + <provider>/callback
	signinCookie    = "dd_signin"
	signinSetting   = "signin:" // + <provider>: the name its settings are kept under
	signinTTL       = 10 * time.Minute
)

// signinKind is a provider the page offers to set up.
type signinKind struct {
	Name, Label, Issuer string
	Custom              bool // the owner names it and gives its issuer
}

// Only providers that say whether they verified the email are here: the
// account is found by it. (Microsoft's email claim is not verified, and
// Apple's flow is its own; neither is offered.)
var signinKinds = []signinKind{
	{Name: "google", Label: "Google", Issuer: "https://accounts.google.com"},
	{Name: "oidc", Custom: true},
}

// signinProvider is a provider as the owner has set it up.
type signinProvider struct {
	signinKind
	ClientID, Secret string
	Enabled          bool
}

func (p signinProvider) ready() bool {
	return p.ClientID != "" && p.Secret != "" && p.Issuer != "" && p.Label != ""
}

// signinRouted reports whether the routing names a signin listener.
func signinRouted(reg *runtime.Registry) bool {
	if reg.Routing == nil {
		return false
	}
	for _, cfg := range reg.Routing.Listeners {
		if cfg.Type == signin.Type {
			return true
		}
	}
	return false
}

func (a *App) identities() *runtime.Handle { return a.Reg.MustClass("FrameworkIdentity") }

// signinBlocked says why sign-in providers cannot be set up here, or "".
func (a *App) signinBlocked() string {
	switch {
	case a.auth == nil:
		return "This app has no accounts."
	case a.signinPath == "":
		return "This app's routing names no signin listener."
	case len(a.cfg.TokenKey) == 0:
		return "This server has no token_key_file, so a provider's secret cannot be kept. Give the server one first."
	case !a.loginIsEmail():
		return "People sign in to this app with something other than an email address, so their accounts cannot be matched to a provider's."
	}
	return ""
}

func (a *App) loginIsEmail() bool {
	s := a.auth.Class.Slot(a.auth.Class.Auth.Login)
	return s != nil && (s.Type == "email" || strings.Contains(strings.ToLower(s.Name), "email"))
}

// signinCallback is the redirect address a provider is registered with.
func (a *App) signinCallback(rq *request, name string) string {
	return a.publicBase(rq) + signinRoute + name + "/callback"
}

// signinProviders is every provider as it is set up, in the page's order.
// Read again whenever the data has moved on.
func (a *App) signinProviders(ctx context.Context) []signinProvider {
	v := int64(-1)
	if a.Reg.View != nil {
		v = a.Reg.View.AppliedVersion()
	}
	a.signinMu.Lock()
	defer a.signinMu.Unlock()
	if a.signinCache != nil && v >= 0 && v == a.signinCacheAt {
		return a.signinCache
	}
	values, _ := storedSettings(ctx, a.Reg, a.cfg.TokenKey, a.log)
	out := make([]signinProvider, 0, len(signinKinds))
	for _, k := range signinKinds {
		s := values[signinSetting+k.Name]
		p := signinProvider{signinKind: k, ClientID: s["client_id"], Secret: s["client_secret"], Enabled: s["enabled"] == "1"}
		if k.Custom {
			p.Label, p.Issuer = s["label"], s["issuer"]
		}
		out = append(out, p)
	}
	a.signinCache, a.signinCacheAt = out, v
	return out
}

func (a *App) forgetSignin() {
	a.signinMu.Lock()
	a.signinCache = nil
	a.signinMu.Unlock()
}

// signinButton is a provider as a sign-in or sign-up page offers it.
type signinButton struct{ Name, Label, URL string }

// signinOffered is the providers a page offers: the ones set up and
// switched on.
func (a *App) signinOffered(rq *request, next string) []signinButton {
	if a.signinBlocked() != "" {
		return nil
	}
	var out []signinButton
	for _, p := range a.signinProviders(rq.ctx()) {
		if !p.Enabled || !p.ready() {
			continue
		}
		u := a.url(signinRoute + p.Name)
		if next != "" {
			u += "?next=" + url.QueryEscape(next)
		}
		out = append(out, signinButton{Name: p.Name, Label: p.Label, URL: u})
	}
	return out
}

// ---- the provider's side ----

// oidcMeta is what a provider's discovery document says of it.
type oidcMeta struct {
	Issuer string `json:"issuer"`
	Auth   string `json:"authorization_endpoint"`
	Token  string `json:"token_endpoint"`
	at     time.Time
}

// signinClient is how the server calls a provider: briefly, following no
// redirect, and never an address inside the server's own network — the
// issuer address is an admin's to give, and must not be a way to reach
// what only the server can.
func (a *App) signinClient() *http.Client {
	if a.signinHTTP != nil {
		return a.signinHTTP
	}
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}}
	return &http.Client{Timeout: 10 * time.Second,
		Transport:     &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// providerURL checks an address of a provider's: https (the test client's
// excepted), with a host.
func (a *App) providerURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Fragment != "" || (u.Scheme != "https" && (a.signinHTTP == nil || u.Scheme != "http")) {
		return fmt.Errorf("%q is not an https address", s)
	}
	return nil
}

// signinDiscover reads a provider's discovery document (kept for an hour).
func (a *App) signinDiscover(ctx context.Context, issuer string) (*oidcMeta, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if err := a.providerURL(issuer); err != nil {
		return nil, err
	}
	a.signinMu.Lock()
	m := a.signinMeta[issuer]
	a.signinMu.Unlock()
	if m != nil && time.Since(m.at) < time.Hour {
		return m, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.signinClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("the provider's description could not be read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the provider's description could not be read: %s answered %d", req.URL, resp.StatusCode)
	}
	m = &oidcMeta{at: time.Now()}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(m); err != nil {
		return nil, fmt.Errorf("the provider's description is not readable: %w", err)
	}
	if strings.TrimRight(m.Issuer, "/") != issuer {
		return nil, fmt.Errorf("the provider at %s says its issuer is %q, not that", issuer, m.Issuer)
	}
	for _, e := range []string{m.Auth, m.Token} {
		if err := a.providerURL(e); err != nil {
			return nil, fmt.Errorf("the provider's description: %w", err)
		}
	}
	a.signinMu.Lock()
	if a.signinMeta == nil {
		a.signinMeta = map[string]*oidcMeta{}
	}
	a.signinMeta[issuer] = m
	a.signinMu.Unlock()
	return m, nil
}

// signinPending is what the start of a sign-in leaves in the browser, for
// the callback: sealed, so it is the server's own word.
type signinPending struct {
	Provider, State, Nonce, Verifier, Next string
	// A sign-in waiting for the account's password (State is signinJoin):
	// who the provider said they are.
	Subject, Email string
	Exp            int64
}

func (a *App) setSigninCookie(rq *request, value string, age int) {
	http.SetCookie(rq.w, &http.Cookie{Name: signinCookie, Value: value, Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: age})
}

// signinJoin marks the cookie of a sign-in waiting for the account's password.
const signinJoin = "join"

// signin serves /_dd/signin/<provider>, /_dd/signin/<provider>/callback and
// /_dd/signin/<provider>/join (the one a form posts to).
func (a *App) signin(rq *request, rest string) error {
	name, join := strings.CutSuffix(rest, "/join")
	if (rq.r.Method == http.MethodPost) != join {
		return errStatus(http.StatusMethodNotAllowed, "Use the button on the sign-in page")
	}
	name, callback := strings.CutSuffix(name, "/callback")
	if blocked := a.signinBlocked(); blocked != "" || strings.Contains(name, "/") {
		return errNotFound
	}
	var p *signinProvider
	for _, sp := range a.signinProviders(rq.ctx()) {
		if sp.Name == name && sp.Enabled && sp.ready() {
			p = &sp
			break
		}
	}
	if p == nil {
		return errNotFound
	}
	if a.rateLimited(rq, "signin", 30, 15*time.Minute) {
		return errStatus(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes.")
	}
	if join {
		return a.signinJoined(rq, p)
	}
	meta, err := a.signinDiscover(rq.ctx(), p.Issuer)
	if err != nil {
		a.log.Error("a sign-in provider could not be reached", "provider", p.Name, "error", err)
		return errStatus(http.StatusBadGateway, "Signing in with "+p.Label+" is not working just now. Please try again, or sign in another way.")
	}
	if !callback {
		return a.signinStart(rq, p, meta)
	}
	return a.signinReturn(rq, p, meta)
}

// signinStart sends the browser to the provider.
func (a *App) signinStart(rq *request, p *signinProvider, meta *oidcMeta) error {
	pend := signinPending{Provider: p.Name, State: randomToken(), Nonce: randomToken(), Verifier: randomToken(),
		Next: safeNext(rq.r.URL.Query().Get("next"), a.cfg.BasePath), Exp: time.Now().Add(signinTTL).Unix()}
	b, _ := json.Marshal(pend)
	sealed, err := seal(a.cfg.TokenKey, string(b))
	if err != nil {
		return err
	}
	a.setSigninCookie(rq, sealed, int(signinTTL.Seconds()))
	u, err := url.Parse(meta.Auth)
	if err != nil {
		return err
	}
	challenge := sha256.Sum256([]byte(pend.Verifier))
	q := u.Query()
	for k, v := range map[string]string{"response_type": "code", "client_id": p.ClientID, "redirect_uri": a.signinCallback(rq, p.Name),
		"scope": "openid email profile", "state": pend.State, "nonce": pend.Nonce,
		"code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256"} {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	http.Redirect(rq.w, rq.r, u.String(), http.StatusSeeOther)
	return nil
}

// idClaims is what an ID token says.
type idClaims struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Aud      any    `json:"aud"` // a string, or a list of them
	Exp      float64
	Nonce    string `json:"nonce"`
	Email    string `json:"email"`
	Verified any    `json:"email_verified"` // true, or "true"
	Name     string `json:"name"`
}

func (c *idClaims) forClient(id string) bool {
	switch aud := c.Aud.(type) {
	case string:
		return aud == id
	case []any:
		for _, v := range aud {
			if s, _ := v.(string); s == id {
				return true
			}
		}
	}
	return false
}

func (c *idClaims) emailVerified() bool {
	switch v := c.Verified.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// signinExchange trades the code the provider sent the browser back with
// for the person's ID token, and checks it is this sign-in's.
func (a *App) signinExchange(rq *request, p *signinProvider, meta *oidcMeta, pend *signinPending, code string) (*idClaims, error) {
	ctx := rq.ctx()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.signinCallback(rq, p.Name)},
		"client_id": {p.ClientID}, "client_secret": {p.Secret}, "code_verifier": {pend.Verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.signinClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("the token endpoint answered %d with nothing readable: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return nil, fmt.Errorf("the token endpoint answered %d: %s %s", resp.StatusCode, out.Error, out.Desc)
	}
	parts := strings.Split(out.IDToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("the ID token: %w", err)
	}
	c := &idClaims{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("the ID token: %w", err)
	}
	switch {
	case strings.TrimRight(c.Iss, "/") != strings.TrimRight(meta.Issuer, "/"):
		return nil, fmt.Errorf("the ID token is from %q, not %q", c.Iss, meta.Issuer)
	case !c.forClient(p.ClientID):
		return nil, errors.New("the ID token is for another client")
	case time.Now().Unix() > int64(c.Exp)+60:
		return nil, errors.New("the ID token has expired")
	case subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(pend.Nonce)) != 1:
		return nil, errors.New("the ID token is not this sign-in's (nonce)")
	case c.Sub == "" || len(c.Sub) > 255:
		return nil, errors.New("the ID token names nobody")
	}
	return c, nil
}

// UnmarshalJSON reads exp whatever kind of number it is.
func (c *idClaims) UnmarshalJSON(b []byte) error {
	type plain idClaims
	aux := struct {
		*plain
		Exp json.Number `json:"exp"`
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.Exp, _ = aux.Exp.Float64()
	return nil
}

// signinReturn finishes a sign-in: the browser is back from the provider.
func (a *App) signinReturn(rq *request, p *signinProvider, meta *oidcMeta) error {
	again := errStatus(http.StatusBadRequest, "That sign-in did not finish: it took too long, or was started in another browser. Please go back to the sign-in page and try again.")
	c, err := rq.r.Cookie(signinCookie)
	if err != nil {
		return again
	}
	a.setSigninCookie(rq, "", -1) // one use
	plain, err := unseal(a.cfg.TokenKey, c.Value)
	if err != nil || !strings.HasPrefix(c.Value, sealedPrefix) {
		return again
	}
	var pend signinPending
	q := rq.r.URL.Query()
	if json.Unmarshal([]byte(plain), &pend) != nil || pend.Provider != p.Name || time.Now().Unix() > pend.Exp ||
		subtle.ConstantTimeCompare([]byte(pend.State), []byte(q.Get("state"))) != 1 {
		return again
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		a.log.Info("a provider sign-in was not completed", "provider", p.Name, "error", q.Get("error"))
		return errStatus(http.StatusForbidden, "Signing in with "+p.Label+" was cancelled or refused. You can go back and try again, or sign in another way.")
	}
	claims, err := a.signinExchange(rq, p, meta, &pend, q.Get("code"))
	if err != nil {
		a.log.Error("a provider sign-in failed", "provider", p.Name, "error", err)
		return errStatus(http.StatusBadGateway, "Signing in with "+p.Label+" did not work. Please try again, or sign in another way.")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !strings.Contains(email, "@") || !claims.emailVerified() {
		return errStatus(http.StatusForbidden, p.Label+" did not give this app an email address it has verified, so it cannot tell which account is yours. Verify your address with "+p.Label+", or sign in another way.")
	}
	guest := rq.p // the guest this browser was, if any
	acc, mine, err := a.signinAccount(rq, p, claims, email)
	if err != nil {
		return err
	}
	if !mine {
		// An account with this address that the provider has not been joined
		// to: its password first, once.
		join := signinPending{Provider: p.Name, State: signinJoin, Subject: claims.Sub, Email: email, Next: pend.Next, Exp: time.Now().Add(signinTTL).Unix()}
		b, _ := json.Marshal(join)
		sealed, err := seal(a.cfg.TokenKey, string(b))
		if err != nil {
			return err
		}
		a.setSigninCookie(rq, sealed, int(signinTTL.Seconds()))
		a.renderJoin(rq, p, email, "")
		return nil
	}
	if err := a.startSession(rq, acc.ID(), true); err != nil {
		return err
	}
	if err := a.adoptGuest(rq, guest, acc.ID()); err != nil {
		a.log.Error("adopting a guest's rows failed; signed in without them", "guest", guest.ID, "account", acc.ID(), "error", err)
	}
	a.log.Info("signed in with a provider", "provider", p.Name, "id", acc.ID())
	next := pend.Next
	if next == "" {
		next = a.url("/")
	}
	http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
	return nil
}

func (a *App) renderJoin(rq *request, p *signinProvider, email, msg string) {
	a.site.Render(a.webRequest(rq), "signin-join", "Connect "+p.Label, map[string]any{
		"Login": email, "Label": p.Label, "Message": msg, "Action": a.url(signinRoute + p.Name + "/join")})
}

// signinJoined joins the provider to the account that was there already:
// the person the provider vouched for has given the account's password.
func (a *App) signinJoined(rq *request, p *signinProvider) error {
	ctx := rq.ctx()
	again := errStatus(http.StatusBadRequest, "That sign-in did not finish: it took too long, or was started in another browser. Please go back to the sign-in page and try again.")
	c, err := rq.r.Cookie(signinCookie)
	if err != nil || !strings.HasPrefix(c.Value, sealedPrefix) {
		return again
	}
	plain, err := unseal(a.cfg.TokenKey, c.Value)
	var pend signinPending
	if err != nil || json.Unmarshal([]byte(plain), &pend) != nil || pend.Provider != p.Name || pend.State != signinJoin || time.Now().Unix() > pend.Exp {
		return again
	}
	if a.rateLimited(rq, "login", 10, 15*time.Minute) {
		return errStatus(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes.")
	}
	acc, err := a.Reg.Authenticate(ctx, pend.Email, rq.r.PostFormValue("password"))
	if errors.Is(err, runtime.ErrBadCredentials) {
		a.renderJoin(rq, p, pend.Email, "That password is not this account's.")
		return nil
	} else if err != nil {
		return err
	}
	if a.principal(acc).Admin {
		return errStatus(http.StatusForbidden, "Admins sign in through their own sign-in link, not with "+p.Label+".")
	}
	a.setSigninCookie(rq, "", -1)
	ids := a.identities()
	if err := ids.Save(ctx, ids.New(map[string]any{"provider": p.Name, "subject": pend.Subject, "principal_id": acc.ID(), "email": pend.Email})); err != nil {
		return err
	}
	guest := rq.p
	if err := a.startSession(rq, acc.ID(), true); err != nil {
		return err
	}
	if err := a.adoptGuest(rq, guest, acc.ID()); err != nil {
		a.log.Error("adopting a guest's rows failed; signed in without them", "guest", guest.ID, "account", acc.ID(), "error", err)
	}
	a.log.Info("a provider was joined to an account", "provider", p.Name, "id", acc.ID())
	next := pend.Next
	if next == "" {
		next = a.url("/")
	}
	http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
	return nil
}

// signinAccount finds the account a provider's person signs in to, or makes
// one. mine is false for an account with the person's address that the
// provider has not been joined to yet: it is theirs to sign in to once
// they have given its password (signinJoined).
func (a *App) signinAccount(rq *request, p *signinProvider, claims *idClaims, email string) (acc *runtime.Record, mine bool, err error) {
	ctx := rq.ctx()
	au := a.auth.Class.Auth
	ids := a.identities()
	admins := errStatus(http.StatusForbidden, "Admins sign in through their own sign-in link, not with "+p.Label+". If you do not have yours, or it has expired, ask another admin for a new one.")
	known, err := ids.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("provider", p.Name), runtime.Eq("subject", claims.Sub)}})
	switch {
	case err == nil:
		acc, err = a.auth.Load(ctx, known.Int("principal_id"))
		if errors.Is(err, runtime.ErrNotFound) {
			ids.Delete(ctx, known.ID()) // the account is gone: they start again
			acc, known = nil, nil
		} else if err != nil {
			return nil, false, err
		}
	case errors.Is(err, runtime.ErrNotFound):
		known = nil
	default:
		return nil, false, err
	}
	if acc != nil { // signed in with this provider before
		if a.principal(acc).Admin {
			return nil, false, admins
		}
		return acc, true, nil
	}
	if a.admins[email] {
		return nil, false, admins // an owner, account or not
	}
	acc, err = a.auth.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, email)}})
	switch {
	case err == nil:
		if a.principal(acc).Admin {
			return nil, false, admins
		}
		return acc, false, nil // theirs once they give its password
	case errors.Is(err, runtime.ErrNotFound):
		if !a.man.signupEnabled() {
			return nil, false, errStatus(http.StatusForbidden, "There is no account for "+email+" here, and this app does not take sign-ups.")
		}
		if a.rateLimited(rq, "signup", 5, time.Hour) {
			return nil, false, errStatus(http.StatusTooManyRequests, "Too many sign-ups from your network. Please try again later.")
		}
		acc = a.auth.New(nil)
		// A guest signing up: their row becomes the account, and what they
		// own (a cart) stays theirs.
		if rq.p != nil && rq.p.Guest {
			if g, err := a.auth.Load(ctx, rq.p.ID); err == nil {
				acc = g
				acc.Values[au.Guest] = nil
			}
		}
		in := map[string]any{au.Login: email, au.Password: randomToken()} // a password nobody knows
		if au.Display != "" {
			name := strings.Join(strings.Fields(claims.Name), " ")
			if name == "" {
				name = email[:strings.Index(email, "@")]
			}
			if s := a.auth.Class.Slot(au.Display); s != nil && s.Max != nil && *s.Max >= 1 {
				name = clipText(name, int(*s.Max))
			}
			in[au.Display] = name
		}
		if errs := acc.Assign(in); errs != nil {
			return nil, false, errStatus(http.StatusUnprocessableEntity, "An account could not be made from what "+p.Label+" says of you: "+errs.Error())
		}
		if err := a.auth.Save(ctx, acc); err != nil {
			var fe runtime.FieldErrors
			if errors.As(err, &fe) {
				return nil, false, errStatus(http.StatusUnprocessableEntity, "An account could not be made from what "+p.Label+" says of you: "+fe.Error())
			}
			return nil, false, err
		}
		a.log.Info("signup", "id", acc.ID(), "provider", p.Name)
	default:
		return nil, false, err
	}
	id := ids.New(map[string]any{"provider": p.Name, "subject": claims.Sub, "principal_id": acc.ID(), "email": email})
	if err := ids.Save(ctx, id); err != nil {
		return nil, false, err
	}
	return acc, true, nil
}

// clipText shortens s to at most max bytes, at a character's end.
func clipText(s string, max int) string {
	for len(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// dropIdentities forgets the providers an account signed in with (the
// account is being deleted).
func (a *App) dropIdentities(ctx context.Context, principalID int64) {
	h, err := a.Reg.Class("FrameworkIdentity")
	if err != nil {
		return
	}
	recs, err := h.List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("principal_id", principalID)}})
	if err != nil {
		return
	}
	for _, r := range recs {
		h.Delete(ctx, r.ID())
	}
}

// ---- the admin page ----

type signinPage struct {
	Message, Error string
	Blocked        string // why nothing can be set up here
	Path           string
	Providers      []signinRow
}

type signinRow struct {
	Name, Title, Label, Issuer string
	Custom                     bool
	ClientID                   string
	HasSecret                  bool
	Enabled, Ready, On         bool
	Callback, Problem          string
	People                     int
}

// signinAdmin serves the Sign-in page and its saves.
func (a *App) signinAdmin(rq *request) error {
	if ok, err := a.adminSection(rq, a.signinPath); !ok {
		return err
	}
	ctx := rq.ctx()
	pg := signinPage{Blocked: a.signinBlocked(), Path: a.signinPath}
	if pg.Blocked == "" && rq.r.Method == http.MethodPost {
		msg, err := a.saveSignin(rq)
		var he *httpError
		switch {
		case errors.As(err, &he):
			return err
		case err != nil:
			pg.Error = err.Error()
		default:
			http.Redirect(rq.w, rq.r, a.url(a.signinPath)+"?done="+url.QueryEscape(msg), http.StatusSeeOther)
			return nil
		}
	} else if done := rq.r.URL.Query().Get("done"); done != "" {
		pg.Message = done
	}
	if pg.Blocked == "" {
		for _, p := range a.signinProviders(ctx) {
			row := signinRow{Name: p.Name, Title: p.Label, Label: p.Label, Issuer: p.Issuer, Custom: p.Custom, ClientID: p.ClientID,
				HasSecret: p.Secret != "", Enabled: p.Enabled, Ready: p.ready(), Callback: a.signinCallback(rq, p.Name)}
			if p.Custom {
				row.Title = "Another OpenID Connect provider"
				if p.Label != "" {
					row.Title += ": " + p.Label
				}
			}
			row.On = row.Enabled && row.Ready
			if row.Enabled && !row.Ready {
				row.Problem = "Switched on, but not offered yet: it still needs its client ID and secret" + map[bool]string{true: ", its name and its issuer address", false: ""}[p.Custom] + "."
			}
			if n, err := a.identities().Count(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("provider", p.Name)}}); err == nil {
				row.People = int(n)
			}
			pg.Providers = append(pg.Providers, row)
		}
	}
	a.site.Render(a.webRequest(rq), "signin", "Sign-in providers", pg)
	return nil
}

// saveSignin keeps what the owner entered for one provider.
func (a *App) saveSignin(rq *request) (string, error) {
	ctx := rq.ctx()
	f := rq.r.PostForm
	var kind *signinKind
	for i := range signinKinds {
		if signinKinds[i].Name == f.Get("provider") {
			kind = &signinKinds[i]
		}
	}
	if kind == nil {
		return "", errStatus(http.StatusBadRequest, "No such provider")
	}
	name := signinSetting + kind.Name
	title := kind.Label
	if kind.Custom {
		title = "the provider"
	}
	defer a.forgetSignin()
	if f.Get("action") == "forget" {
		for _, s := range []string{"client_id", "client_secret", "enabled", "label", "issuer"} {
			if err := a.forgetSetting(ctx, name, s); err != nil {
				return "", err
			}
		}
		a.log.Info("a sign-in provider was removed", "provider", kind.Name, "by", rq.p.ID)
		return "Removed " + title + ". People who signed in with it keep their accounts; they can set a password with “Forgot your password” where the app offers it.", nil
	}
	set := map[string]string{"client_id": strings.TrimSpace(f.Get("client_id")), "enabled": map[bool]string{true: "1", false: "0"}[f.Get("enabled") == "1"]}
	if len(set["client_id"]) > 300 || len(f.Get("client_secret")) > 500 {
		return "", errors.New("That client ID or secret is too long to be one.")
	}
	if kind.Custom {
		set["label"] = strings.Join(strings.Fields(f.Get("label")), " ")
		set["issuer"] = strings.TrimRight(strings.TrimSpace(f.Get("issuer")), "/")
		if len(set["label"]) > 40 {
			return "", errors.New("The name on the button is at most 40 characters.")
		}
		if set["issuer"] != "" {
			if _, err := a.signinDiscover(ctx, set["issuer"]); err != nil {
				return "", fmt.Errorf("The issuer address does not answer as an OpenID Connect provider: %v", err)
			}
		}
	}
	for k, v := range set {
		if err := a.storeSetting(ctx, rq.p.ID, name, k, v, false); err != nil {
			return "", err
		}
	}
	if secret := strings.TrimSpace(f.Get("client_secret")); secret != "" { // blank keeps what is stored
		if err := a.storeSetting(ctx, rq.p.ID, name, "client_secret", secret, true); err != nil {
			return "", err
		}
	}
	a.forgetSignin()
	a.log.Info("a sign-in provider was configured", "provider", kind.Name, "by", rq.p.ID)
	for _, p := range a.signinProviders(ctx) {
		if p.Name != kind.Name {
			continue
		}
		switch {
		case p.Enabled && p.ready():
			return "Saved. The sign-in and sign-up pages now offer “Continue with " + p.Label + "”. Try it in a private window before telling anyone.", nil
		case p.ready():
			return "Saved. It is switched off: tick the box to offer it.", nil
		}
	}
	return "Saved. It is not offered until everything it needs is filled in.", nil
}
