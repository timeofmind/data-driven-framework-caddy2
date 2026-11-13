package app

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// The admin link: a built app has no accounts, so its builder (or an
// operator) mints a short-lived link, signed with the app's token key,
// naming one of the app's admin logins. Opening it signs that account in
// when it exists, and otherwise leads to sign-up with the login fixed —
// allowed even when sign-up is off — so the first admin can set a password
// and reach the admin backend. The link is checked on any node (the token
// key is shared) and is good until it expires; it is not single-use.

// AdminLinkTTL is how long an admin link is good for unless told otherwise.
const AdminLinkTTL = 10 * time.Minute

// AdminLink mints an admin link for an app at baseURL (its public address,
// base path included), signing in login, good for ttl (AdminLinkTTL when 0).
func AdminLink(tokenKey []byte, login, baseURL string, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = AdminLinkTTL
	}
	s := ux.NewSigner(tokenKey)
	tok := s.Sign(ux.Claims{Admin: strings.ToLower(strings.TrimSpace(login)), Exp: time.Now().Add(ttl).Unix()})
	return strings.TrimRight(baseURL, "/") + "/_dd/admin?token=" + url.QueryEscape(tok)
}

// adminClaims reads a valid admin link token, or reports none.
func (a *App) adminClaims(tok string) (string, bool) {
	c, ok := a.signer.Verify(tok)
	if !ok || c.Admin == "" || c.Page != "" || c.Session != "" {
		return "", false
	}
	return c.Admin, true
}

// adminLink serves /_dd/admin?token=…
func (a *App) adminLink(rq *request) error {
	login, ok := a.adminClaims(rq.r.URL.Query().Get("token"))
	if !ok {
		return errStatus(http.StatusForbidden, "This admin link is not valid or has expired. Ask for a new one.")
	}
	if !a.admins[login] {
		return errStatus(http.StatusForbidden, "This link names a login that is not an admin of this app.")
	}
	if a.auth == nil {
		return errStatus(http.StatusNotFound, "This app has no accounts.")
	}
	au := a.auth.Class.Auth
	rec, err := a.auth.First(rq.ctx(), runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, login)}})
	switch {
	case err == nil:
		if err := a.startSession(rq, rec.ID(), true); err != nil {
			return err
		}
		a.log.Info("admin link used", "login", login, "id", rec.ID())
		http.Redirect(rq.w, rq.r, a.url(servicesPath), http.StatusSeeOther)
		return nil
	case errors.Is(err, runtime.ErrNotFound):
		q := url.Values{au.Login: {login}, "_admin": {rq.r.URL.Query().Get("token")}, "next": {servicesPath}}
		http.Redirect(rq.w, rq.r, a.url("/signup")+"?"+q.Encode(), http.StatusSeeOther)
		return nil
	default:
		return err
	}
}

// adminSignup is the admin login a sign-up request carries a valid admin
// link token for (in _admin, on the query or the form), or "".
func (a *App) adminSignup(rq *request) string {
	tok := rq.r.URL.Query().Get("_admin")
	if rq.r.Method == http.MethodPost {
		tok = rq.r.PostFormValue("_admin")
	}
	if tok == "" {
		return ""
	}
	login, ok := a.adminClaims(tok)
	if !ok || !a.admins[login] {
		return ""
	}
	return login
}
