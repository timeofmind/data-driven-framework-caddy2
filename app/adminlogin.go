package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/mail"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// An admin's login link: each admin signs in at an address of their own,
// /_dd/login?k=…, and nowhere else — the public sign-in page refuses an
// admin's account — so there is no page for a bot to find and try admin
// passwords at. The link is not a way in by itself: it leads to a sign-in
// page with the login fixed, which asks for the password; for someone with
// no account yet (an owner's first visit, an admin invited on the Admins
// page) it leads to sign-up with the login fixed, where they set one.
//
// A link is good for a number of days (30 unless told otherwise, at most a
// year) counted from the last sign-in through it: every sign-in renews it,
// and an admin who has not signed in for that long needs a new one, made on
// the Admins page by an admin of more authority (or by themselves while
// they are still signed in). A new link replaces the old. Only the hash of
// a link's key is kept (FrameworkAdminLink), so a link is shown once, when
// it is made, and mailed to the admin where the app has a mail listener.
//
// The operator's admin link (/_dd/admin?token=…, adminlink.go), minted with
// the server's token key, stays as the way in when nobody has a link yet:
// from the Admins page the owner then makes their own.

const (
	loginLinkPath = "/_dd/login"
	// The days a login link is good for after its last use.
	loginLinkDays    = 30
	loginLinkMaxDays = 365
)

func (a *App) loginLinks() *runtime.Handle { return a.Reg.MustClass("FrameworkAdminLink") }

// linkDays reads the days a new link should be good for.
func linkDays(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return loginLinkDays, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > loginLinkMaxDays {
		return 0, fmt.Errorf("A link is good for 1 to %d days after its last use.", loginLinkMaxDays)
	}
	return n, nil
}

// linkExpiry is when a link stops working: its days after its last use.
func linkExpiry(r *runtime.Record) time.Time {
	at := r.String("renewed_at")
	t, err := time.Parse(timeLayout, strings.Replace(at[:min(len(at), 19)], "T", " ", 1))
	if err != nil {
		return time.Time{}
	}
	return t.Add(time.Duration(r.Int("days")) * 24 * time.Hour)
}

// linkFor is the login link a login has, if any.
func (a *App) linkFor(ctx context.Context, login string) *runtime.Record {
	r, err := a.loginLinks().First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("login", login)}})
	if err != nil {
		return nil
	}
	return r
}

// linkState says, for the Admins page, how a login's link stands.
func (a *App) linkState(ctx context.Context, login string) string {
	r := a.linkFor(ctx, login)
	if r == nil {
		return "no link"
	}
	exp := linkExpiry(r)
	if time.Now().UTC().After(exp) {
		return "expired " + exp.Format("2006-01-02")
	}
	return fmt.Sprintf("good until %s (%d days after the last sign-in)", exp.Format("2006-01-02"), r.Int("days"))
}

// newLoginLink makes login a link good for days after each use, in place of
// any it had, and returns its address — the only time it can be told.
func (a *App) newLoginLink(rq *request, login string, days int, actor string) (string, error) {
	ctx := rq.ctx()
	h := a.loginLinks()
	if old := a.linkFor(ctx, login); old != nil {
		if err := h.Delete(ctx, old.ID()); err != nil {
			return "", err
		}
	}
	key := randomToken()
	rec := h.New(map[string]any{"login": login, "token_hash": tokenHash(key), "days": int64(days),
		"renewed_at": time.Now().UTC().Format(timeLayout), "made_by": actor})
	if err := h.Save(ctx, rec); err != nil {
		return "", err
	}
	return a.publicBase(rq) + loginLinkPath + "?k=" + url.QueryEscape(key), nil
}

// dropLoginLink ends a login's link (an admin removed).
func (a *App) dropLoginLink(ctx context.Context, login string) {
	if old := a.linkFor(ctx, login); old != nil {
		a.loginLinks().Delete(ctx, old.ID())
	}
}

// renewLoginLink counts a link's days again from now: its admin signed in.
func (a *App) renewLoginLink(ctx context.Context, r *runtime.Record) {
	r.Values["renewed_at"] = time.Now().UTC().Format(timeLayout)
	if err := a.loginLinks().Save(ctx, r); err != nil {
		a.log.Error("a login link could not be renewed", "login", r.String("login"), "error", err)
	}
}

// publicBase is the site's public address, base path included: the server's
// base_url, or the request's own when it names none.
func (a *App) publicBase(rq *request) string {
	base := strings.TrimRight(a.cfg.BaseURL, "/")
	if base == "" {
		scheme := "http"
		if rq.r.TLS != nil || strings.EqualFold(rq.r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + rq.r.Host
	}
	if a.cfg.BasePath != "" && !strings.HasSuffix(base, a.cfg.BasePath) {
		base += a.cfg.BasePath
	}
	return base
}

// adminHome is the first section of the admin area p's authority reaches.
func (a *App) adminHome(p *runtime.Principal) string {
	for _, s := range a.sections {
		if p.AdminAllowed(s.Level) {
			return a.url(s.URL)
		}
	}
	return a.url("/")
}

// mailLoginLink mails a link to its admin through the app's mail listener,
// and reports whether it went. Without one (or with a login that is not an
// address) the link is only shown to the admin who made it.
func (a *App) mailLoginLink(ctx context.Context, login, link string, days int) bool {
	if !strings.Contains(login, "@") {
		return false
	}
	for name, l := range a.Reg.Listeners {
		ml, ok := listener.Unwrap(l).(*mail.Listener)
		if !ok || a.Reg.Unconfigured(name) != nil {
			continue
		}
		body := fmt.Sprintf(`You are an admin of %s. This is your personal sign-in page for its admin area:

%s

It asks for your password; the first time, it lets you set one. Admins cannot sign in at the public sign-in page, only here.

The link stays good for %d days after each time you sign in through it. If you do not sign in for %d days it expires, and another admin can make you a new one. Keep it to yourself.
`, a.man.Name, link, days, days)
		if err := ml.SendMessage(ctx, login, "Your admin sign-in link for "+strings.Join(strings.Fields(a.man.Name), " "), body); err != nil {
			a.log.Warn("an admin's login link could not be mailed; it was shown to the admin who made it", "login", login, "listener", name, "error", err)
			return false
		}
		return true
	}
	return false
}

// adminLogin serves an admin's own sign-in page, /_dd/login?k=….
func (a *App) adminLogin(rq *request) error {
	if a.auth == nil {
		return errStatus(http.StatusNotFound, "This app has no accounts.")
	}
	ctx := rq.ctx()
	gone := errStatus(http.StatusForbidden, "This sign-in link is not valid, or it has expired: a link is good for a number of days after the last sign-in through it. Ask an admin for a new one.")
	key := rq.r.URL.Query().Get("k")
	if key == "" {
		return gone
	}
	if a.rateLimited(rq, "admin-login", 30, 15*time.Minute) {
		return errStatus(http.StatusTooManyRequests, "Too many attempts. Please wait a few minutes.")
	}
	link, err := a.loginLinks().First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("token_hash", tokenHash(key))}})
	if err != nil || time.Now().UTC().After(linkExpiry(link)) {
		return gone
	}
	login := link.String("login")
	au := a.auth.Class.Auth
	owner, inv := a.admins[login], a.invited(ctx, login)
	acc, err := a.auth.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, login)}})
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		// No account yet: sign-up with the login fixed, where the password is set.
		if !owner && inv == nil {
			return gone
		}
		tok := a.signer.Sign(ux.Claims{Admin: login, Exp: time.Now().Add(AdminLinkTTL).Unix()})
		q := url.Values{au.Login: {login}, "_admin": {tok}, "next": {a.adminsPath}}
		http.Redirect(rq.w, rq.r, a.url("/signup")+"?"+q.Encode(), http.StatusSeeOther)
		return nil
	case err != nil:
		return err
	case inv != nil && !owner:
		// Invited, and an account with the login has been made since by
		// someone: the link proves nothing about who holds that account.
		return errStatus(http.StatusForbidden, "An account with this login was created after the invitation was made. Ask an admin to add you again.")
	}
	p := a.principal(acc)
	if !p.Admin {
		return gone
	}
	msg := ""
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "login", 10, 15*time.Minute) {
			return errStatus(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes.")
		}
		rec, err := a.Reg.Authenticate(ctx, login, rq.r.PostFormValue("password"))
		switch {
		case err == nil:
			if err := a.startSession(rq, rec.ID(), true); err != nil {
				return err
			}
			a.renewLoginLink(ctx, link)
			a.log.Info("admin signed in through their login link", "login", login, "id", rec.ID())
			next := safeNext(rq.r.FormValue("next"), a.cfg.BasePath)
			if next == "" {
				next = a.adminHome(p)
			}
			http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
			return nil
		case !errors.Is(err, runtime.ErrBadCredentials):
			return err
		}
		msg = "That password is not this account's."
	}
	days := link.Int("days")
	a.site.Render(a.webRequest(rq), "login", "Admin sign-in", map[string]any{
		"Title": "Admin sign-in", "Message": msg, "Login": login, "Fixed": true,
		"LoginLabel": a.auth.Class.Slot(au.Login).EffectiveLabel(), "Next": safeNext(rq.r.FormValue("next"), a.cfg.BasePath),
		"Action": a.url(loginLinkPath) + "?k=" + url.QueryEscape(key),
		"Note":   fmt.Sprintf("This is your own sign-in page for the admin area. The link to it stays good for %d days after each time you sign in here; if you do not sign in for %d days it expires and an admin makes you a new one.", days, days)})
	return nil
}
