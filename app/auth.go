package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// Accounts for the model's auth class: sign-up, sign-in, sign-out, and /me
// (the signed-in person's page: the auth class's default page).

func (a *App) signup(rq *request) error {
	c := a.auth.Class
	au := c.Auth
	rec := a.auth.New(nil)
	next := safeNext(rq.r.FormValue("next"), a.cfg.BasePath)
	var errs runtime.FieldErrors
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "signup", 5, time.Hour) {
			return errStatus(http.StatusTooManyRequests, "Too many sign-ups from your network. Please try again later.")
		}
		// A guest signing up: their row becomes the account, and what they
		// own (a cart) stays theirs.
		if rq.p != nil && rq.p.Guest {
			if g, err := a.auth.Load(rq.ctx(), rq.p.ID); err == nil {
				rec = g
				rec.Values[au.Guest] = nil
			}
		}
		f := rq.r.PostForm
		in := map[string]any{
			au.Login:    strings.ToLower(strings.TrimSpace(f.Get(au.Login))),
			au.Password: f.Get(au.Password),
		}
		if au.Display != "" {
			in[au.Display] = f.Get(au.Display)
		}
		errs = rec.Assign(in)
		if errs == nil && f.Get(au.Password) != f.Get("password_confirm") {
			errs = runtime.FieldErrors{"password_confirm": "doesn't match the password"}
		}
		if errs == nil {
			if err := a.auth.Save(rq.ctx(), rec); err != nil {
				var fe runtime.FieldErrors
				if !errors.As(err, &fe) {
					msg := strings.ToLower(err.Error())
					if !strings.Contains(msg, "unique") && !strings.Contains(msg, "duplicate") {
						return err
					}
					fe = runtime.FieldErrors{au.Login: "already has an account — sign in instead"}
				}
				if fe[au.Login] != "" { // the login's uniqueness, checked by the save
					fe[au.Login] = "already has an account — sign in instead"
				}
				errs = fe
			} else {
				if err := a.startSession(rq, rec.ID()); err != nil {
					return err
				}
				a.log.Info("signup", "id", rec.ID())
				if next == "" {
					next = a.Pages.RecordURL(a.cfg.BasePath, c.Name, rec.ID())
				}
				if next == "" {
					next = a.url("/")
				}
				http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
				return nil
			}
		}
	}
	if rq.r.Method != http.MethodPost {
		// A link may fill in the login (an invitation: /signup?email=…).
		if v := strings.TrimSpace(rq.r.URL.Query().Get(au.Login)); v != "" && len(v) <= 254 {
			rec.Values[au.Login] = strings.ToLower(v)
		}
	}
	var fields []ux.FormField
	for _, n := range []string{au.Login, au.Password, "password_confirm", au.Display} {
		if n == "" {
			continue
		}
		if n == "password_confirm" {
			fields = append(fields, ux.FormField{Name: n, Label: "Confirm password", Type: "password", Required: true, Error: errs[n]})
			continue
		}
		s := c.Slot(n)
		ff := ux.FormField{Name: n, Label: s.EffectiveLabel(), Type: ux.InputType(s.Type), Required: true, Help: s.Description, Error: errs[n]}
		switch n {
		case au.Login:
			ff.Type = "email"
			if !strings.Contains(n, "email") {
				ff.Type = "text"
			}
			ff.Value = rec.String(n)
		case au.Password:
		default:
			ff.Value = rec.String(n)
		}
		fields = append(fields, ff)
	}
	a.site.Render(a.webRequest(rq), "signup", "Create your account", map[string]any{
		"Fields": fields, "Message": ux.GeneralError(errs), "Intro": a.man.Signup.Intro, "Next": next})
	return nil
}

func (a *App) login(rq *request) error {
	au := a.auth.Class.Auth
	next := safeNext(rq.r.FormValue("next"), a.cfg.BasePath)
	msg := ""
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "login", 10, 15*time.Minute) {
			return errStatus(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes.")
		}
		rec, err := a.Reg.Authenticate(rq.ctx(), rq.r.PostFormValue("login"), rq.r.PostFormValue("password"))
		if err == nil {
			if err := a.adoptGuest(rq, rec.ID()); err != nil { // a guest's cart comes along
				return err
			}
			if err := a.startSession(rq, rec.ID()); err != nil {
				return err
			}
			if next == "" {
				next = a.url("/")
			}
			http.Redirect(rq.w, rq.r, next, http.StatusSeeOther)
			return nil
		}
		if !errors.Is(err, runtime.ErrBadCredentials) {
			return err
		}
		msg = "That " + strings.ToLower(a.auth.Class.Slot(au.Login).EffectiveLabel()) + " and password don't match an account."
	}
	label := a.auth.Class.Slot(au.Login).EffectiveLabel()
	a.site.Render(a.webRequest(rq), "login", "Sign in", map[string]any{"Message": msg, "Next": next, "LoginLabel": label, "Login": rq.r.PostFormValue("login")})
	return nil
}

func (a *App) logout(rq *request) error {
	if rq.r.Method != http.MethodPost {
		return errStatus(http.StatusMethodNotAllowed, "Use the sign-out button")
	}
	a.endSession(rq)
	http.Redirect(rq.w, rq.r, a.url("/"), http.StatusSeeOther)
	return nil
}

// me goes to the signed-in person's own page.
func (a *App) me(rq *request) error {
	if err := a.requireAccount(rq); err != nil {
		return err
	}
	u := a.Pages.RecordURL(a.cfg.BasePath, a.auth.Class.Name, rq.p.ID)
	if u == "" {
		return errNotFound
	}
	http.Redirect(rq.w, rq.r, u, http.StatusSeeOther)
	return nil
}

// safeNext is where to go after signing in or up: a path of this site
// (given with or without the base path), or "".
func safeNext(next, base string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return ""
	}
	if base != "" && next != base && !strings.HasPrefix(next, base+"/") && !strings.HasPrefix(next, base+"?") {
		next = base + next
	}
	return next
}
