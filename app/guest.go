package app

import (
	"context"
	"net/http"
	"time"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Guests. A visitor without an account who creates a row where guests may
// (the class says access: guests) is given a row of the auth class — its
// guest slot set (auth: guest), login and password empty, display "Guest"
// — and a session, so the row is theirs: a shopper's cart before they sign
// up. Signing up fills that same row in, and keeps the cart; signing in
// moves the guest's rows to the account (runtime.Registry.Adopt) and drops
// the guest. Pages and rules for the signed-in (access: signed_in,
// read/write: authenticated, /me) leave guests out.

// guestsOffered reports whether the auth class takes guests.
func (a *App) guestsOffered() bool { return a.auth != nil && a.auth.Class.Auth.Guest != "" }

// newGuest makes a guest of the visitor: the row, the session, the
// principal (rq.p). A visitor already signed in, or already a guest, is
// returned as they are.
func (a *App) newGuest(rq *request) (*runtime.Principal, error) {
	if rq.p != nil {
		return rq.p, nil
	}
	if !a.guestsOffered() {
		return nil, errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	if a.rateLimited(rq, "guest", a.man.guestPerHour(), time.Hour) {
		return nil, errStatus(http.StatusTooManyRequests, "Too many new visitors from your network. Please try again later.")
	}
	au := a.auth.Class.Auth
	rec := a.auth.New(nil)
	rec.Values[au.Guest] = randomToken()
	if au.Display != "" {
		rec.Values[au.Display] = a.man.guestDisplay()
	}
	if err := a.auth.Save(rq.ctx(), rec); err != nil {
		return nil, err
	}
	if err := a.startSession(rq, rec.ID(), false); err != nil { // the client's CSRF token stays good
		return nil, err
	}
	rq.p = a.principal(rec)
	a.log.Info("guest", "id", rec.ID())
	return rq.p, nil
}

// adoptGuest moves what the guest owns to the account and removes the guest
// row, as one transaction (whole or not at all; nothing of theirs is left
// to cascade), then ends the guest's sessions. Nothing happens when guest is
// not a guest.
func (a *App) adoptGuest(rq *request, guest *runtime.Principal, account int64) error {
	if guest == nil || !guest.Guest || guest.ID == account {
		return nil
	}
	moved := 0
	err := a.Reg.Transact(rq.ctx(), func(ctx context.Context) error {
		n, err := a.Reg.Adopt(ctx, guest.ID, account)
		if err != nil {
			return err
		}
		moved = n
		return a.auth.Delete(ctx, guest.ID)
	})
	if err != nil {
		return err
	}
	a.endAllSessions(rq.ctx(), guest.ID)
	a.log.Info("guest adopted", "guest", guest.ID, "account", account, "rows", moved)
	return nil
}

// sweepGuests removes a few guests idle for longer than guests.keep_for —
// their row unchanged that long (a cart change changes it) — with
// everything they own. Called opportunistically, as expired sessions are.
func (a *App) sweepGuests(ctx context.Context) {
	keep, err := a.man.guestKeepFor()
	if err != nil || keep == 0 || !a.guestsOffered() {
		return
	}
	au := a.auth.Class.Auth
	cut := time.Now().UTC().Add(-keep).Format(timeLayout)
	old, _ := a.auth.List(ctx, runtime.Query{Where: []runtime.Cond{{Field: au.Guest, Op: "notnull"}, {Field: "updated_at", Op: "<", Value: cut}}, Limit: 20})
	for _, g := range old {
		a.endAllSessions(ctx, g.ID())
		if err := a.auth.Delete(ctx, g.ID()); err != nil {
			a.log.Warn("sweeping an idle guest", "id", g.ID(), "error", err)
		} else {
			a.log.Info("an idle guest was swept", "id", g.ID(), "idle_since", g.String("updated_at"))
		}
	}
}

// requireAccount is requireLogin for what a guest may not do: their own
// page, their feed.
func (a *App) requireAccount(rq *request) error {
	if !rq.p.SignedIn() {
		return errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	return nil
}
