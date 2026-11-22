package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/mail"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// Password reset: every app whose members sign in with a password has it,
// with nothing in the app to declare. /forgot asks for the login and, for an
// account that exists, mails a link through the app's mail listener (the
// builder sets one up when it publishes the app):
// {{base}}/reset?code=<a token signed with the app's token key, naming the
// account, good for an hour, and bound to the account's current password, so
// it stops working once the password changes — used once, nothing stored>.
// Without a configured mail listener (a preview) /forgot says the app cannot
// send mail yet. The answer is the same whether or not the account exists.
// /reset?code=… names the account by the code, takes the new password, sets it
// on the account and signs the person in.
//
// An app may instead keep reset codes in a class of its own, which flows
// wherever its routing sends it (an older form, still honoured):
//
//	password_reset: {class: PasswordReset, expires: 1h}
//
// The class belongs to the auth class and has slots code, expires_at and
// used_at (and email, filled in if present).
func (a *App) resetEnabled() bool {
	return a.auth != nil && a.auth.Class.Auth.Password != ""
}

// resetClass is the app's own class of reset codes, "" for the built-in link.
func (a *App) resetClass() string {
	if c := a.man.PasswordReset.Class; c != "" && a.Reg.Model.Class(c) != nil {
		return c
	}
	return ""
}

// resetPage marks a reset token (no page has a name starting with _).
const resetPage = "_reset"

// passwordMark is a number that changes when the account's password does: a
// reset token carries it, so a token stops working once it has been used.
func passwordMark(acct *runtime.Record, slot string) int64 {
	sum := sha256.Sum256([]byte(acct.String(slot)))
	return int64(binary.BigEndian.Uint64(sum[:8]) >> 1)
}

// mailer is the app's configured mail listener, nil when it has none (yet).
func (a *App) mailer() *mail.Listener {
	names := make([]string, 0, len(a.Reg.Listeners))
	for name := range a.Reg.Listeners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ml, ok := listener.Unwrap(a.Reg.Listeners[name]).(*mail.Listener); ok && a.Reg.Unconfigured(name) == nil {
			return ml
		}
	}
	return nil
}

func (a *App) resetTTL() time.Duration {
	if d, err := time.ParseDuration(a.man.PasswordReset.Expires); err == nil && d > 0 {
		return d
	}
	return time.Hour
}

// resetFK is the reset class's slot naming the account.
func (a *App) resetFK() string {
	c := a.Reg.Model.Class(a.man.PasswordReset.Class)
	for _, rel := range c.AllRelationships() {
		if rel.IsBelongsTo() && rel.Target == a.auth.Class.Name {
			return rel.Via
		}
	}
	return ""
}

func (a *App) forgot(rq *request) error {
	au := a.auth.Class.Auth
	label := strings.ToLower(a.auth.Class.Slot(au.Login).EffectiveLabel())
	data := map[string]any{"LoginLabel": label, "Login": "", "Expires": humanDuration(a.resetTTL())}
	if a.resetClass() == "" && a.mailer() == nil {
		data["NoMail"] = true
		a.site.Render(a.webRequest(rq), "forgot", "Reset your password", data)
		return nil
	}
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "forgot", 5, time.Hour) {
			return errStatus(http.StatusTooManyRequests, "Too many requests. Please wait a while.")
		}
		login := strings.ToLower(strings.TrimSpace(rq.r.PostFormValue("login")))
		data["Login"] = login
		if login == "" {
			data["Message"] = "Tell us your " + label + "."
		} else {
			if acct := a.accountByLogin(rq.ctx(), login); acct != nil {
				if err := a.startReset(rq, acct, login); err != nil {
					a.log.Error("password reset", "login", login, "error", err)
				}
			}
			data["Done"] = true
		}
	}
	a.site.Render(a.webRequest(rq), "forgot", "Reset your password", data)
	return nil
}

func (a *App) accountByLogin(ctx context.Context, login string) *runtime.Record {
	au := a.auth.Class.Auth
	recs, err := a.auth.List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, login)}, Limit: 1})
	if err != nil || len(recs) == 0 {
		return nil
	}
	return recs[0]
}

// startReset saves a reset record for the account: a fresh code, good for
// the configured time. Whatever the routing sends the class to (the mail
// listener) follows.
func (a *App) startReset(rq *request, acct *runtime.Record, login string) error {
	ctx := rq.ctx()
	if a.resetClass() == "" {
		return a.mailReset(rq, acct, login)
	}
	h := a.Reg.MustClass(a.man.PasswordReset.Class)
	fk := a.resetFK()
	if fk == "" {
		return fmt.Errorf("%s has no belongs_to %s", h.Class.Name, a.auth.Class.Name)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	rec := h.New(map[string]any{fk: acct.ID(), "code": hex.EncodeToString(b[:]),
		"expires_at": time.Now().UTC().Add(a.resetTTL()).Format("2006-01-02 15:04:05")})
	if h.Class.Slot("email") != nil {
		rec.Values["email"] = login
	}
	a.log.Info("password reset requested", "account", acct.ID())
	return h.Save(ctx, rec)
}

// mailReset mails the built-in reset link.
func (a *App) mailReset(rq *request, acct *runtime.Record, login string) error {
	ml := a.mailer()
	if ml == nil {
		return errors.New("the app has no mail listener set up")
	}
	tok := a.signer.Sign(ux.Claims{Page: resetPage, Class: a.auth.Class.Name, ID: acct.ID(),
		Version: passwordMark(acct, a.auth.Class.Auth.Password), Exp: time.Now().Add(a.resetTTL()).Unix()})
	link := a.publicBase(rq) + "/reset?code=" + url.QueryEscape(tok)
	name := strings.Join(strings.Fields(a.man.Name), " ")
	body := fmt.Sprintf(`Someone asked to reset the password of your %s account (%s).

To choose a new password, open this link:

%s

It works once, for %s. If you did not ask for this, ignore this message: your password stays as it is.
`, name, login, link, humanDuration(a.resetTTL()))
	a.log.Info("password reset requested", "account", acct.ID())
	return ml.SendMessage(rq.ctx(), login, "Reset your "+name+" password", body)
}

// resetByCode is the unused, unexpired reset record with this code, with
// its account; nil when there is none.
func (a *App) resetByCode(ctx context.Context, code string) (*runtime.Record, *runtime.Record) {
	if a.resetClass() == "" {
		c, ok := a.signer.Verify(code)
		if !ok || c.Page != resetPage || c.Class != a.auth.Class.Name || c.ID == 0 {
			return nil, nil
		}
		acct, err := a.auth.Load(ctx, c.ID)
		if err != nil || acct == nil || passwordMark(acct, a.auth.Class.Auth.Password) != c.Version {
			return nil, nil
		}
		return acct, acct
	}
	if len(code) < 16 || len(code) > 128 {
		return nil, nil
	}
	h := a.Reg.MustClass(a.man.PasswordReset.Class)
	recs, err := h.List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("code", code)}, Limit: 1})
	if err != nil || len(recs) == 0 {
		return nil, nil
	}
	rec := recs[0]
	if rec.String("used_at") != "" {
		return nil, nil
	}
	if exp, err := time.Parse("2006-01-02 15:04:05", rec.String("expires_at")); err != nil || time.Now().UTC().After(exp) {
		return nil, nil
	}
	acct, err := a.auth.Load(ctx, rec.Int(a.resetFK()))
	if err != nil || acct == nil {
		return nil, nil
	}
	return rec, acct
}

func (a *App) reset(rq *request) error {
	au := a.auth.Class.Auth
	code := rq.r.FormValue("code")
	rec, acct := a.resetByCode(rq.ctx(), code)
	if rec == nil {
		a.site.Render(a.webRequest(rq), "reset", "Choose a new password", map[string]any{"Invalid": true})
		return nil
	}
	pwSlot := a.auth.Class.Slot(au.Password)
	data := map[string]any{"Code": code, "Login": acct.String(au.Login), "Help": pwSlot.Description}
	if pwSlot.Min != nil {
		data["Min"] = *pwSlot.Min
	}
	if rq.r.Method == http.MethodPost {
		if a.rateLimited(rq, "reset", 10, 15*time.Minute) {
			return errStatus(http.StatusTooManyRequests, "Too many attempts. Please wait a while.")
		}
		pw := rq.r.PostFormValue("password")
		switch {
		case pw == "":
			data["Message"] = "Choose a password."
		case pw != rq.r.PostFormValue("password_confirm"):
			data["Message"] = "The two passwords don't match."
		default:
			acct.Values[au.Password] = pw // hashed by the save
			if err := a.auth.Save(rq.ctx(), acct); err != nil {
				var fe runtime.FieldErrors
				if errors.As(err, &fe) {
					data["Message"] = pwSlot.EffectiveLabel() + " " + fe[au.Password]
					break
				}
				return err
			}
			if a.resetClass() != "" {
				rec.Values["used_at"] = time.Now().UTC().Format("2006-01-02 15:04:05")
				if err := a.Reg.MustClass(rec.Class.Name).Save(rq.ctx(), rec); err != nil {
					return err
				}
			}
			a.log.Info("password reset", "account", acct.ID())
			if err := a.startSession(rq, acct.ID(), true); err != nil {
				return err
			}
			http.Redirect(rq.w, rq.r, a.url("/"), http.StatusSeeOther)
			return nil
		}
	}
	a.site.Render(a.webRequest(rq), "reset", "Choose a new password", data)
	return nil
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= time.Hour:
		return "an hour"
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return d.String()
}

// resetClassOK checks the configured class at start: it exists, belongs to
// the auth class and has the slots the flow needs.
func (a *App) resetClassOK() error {
	if a.man.PasswordReset.Class == "" {
		return nil
	}
	c := a.Reg.Model.Class(a.man.PasswordReset.Class)
	if c == nil {
		return fmt.Errorf("password_reset: no class %s", a.man.PasswordReset.Class)
	}
	if a.auth == nil {
		return fmt.Errorf("password_reset: the model has no auth class")
	}
	if a.resetFK() == "" {
		return fmt.Errorf("password_reset: %s must belong_to %s", c.Name, a.auth.Class.Name)
	}
	for _, s := range []string{"code", "expires_at", "used_at"} {
		if c.Slot(s) == nil {
			return fmt.Errorf("password_reset: %s needs a slot %s", c.Name, s)
		}
	}
	if a.man.PasswordReset.Expires != "" {
		if _, err := time.ParseDuration(a.man.PasswordReset.Expires); err != nil {
			return fmt.Errorf("password_reset: expires: %w", err)
		}
	}
	if _, err := a.man.guestKeepFor(); err != nil {
		return err
	}
	return nil
}
