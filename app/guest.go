package app

import (
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
	if a.rateLimited(rq, "guest", 20, time.Hour) {
		return nil, errStatus(http.StatusTooManyRequests, "Too many new visitors from your network. Please try again later.")
	}
	au := a.auth.Class.Auth
	rec := a.auth.New(nil)
	rec.Values[au.Guest] = randomToken()
	if au.Display != "" {
		rec.Values[au.Display] = "Guest"
	}
	if err := a.auth.Save(rq.ctx(), rec); err != nil {
		return nil, err
	}
	if err := a.startSession(rq, rec.ID()); err != nil {
		return nil, err
	}
	rq.p = a.principal(rec)
	a.log.Info("guest", "id", rec.ID())
	return rq.p, nil
}

// adoptGuest moves what the guest rq.p owns to the account, ends the
// guest's sessions and removes the guest row (nothing of theirs is left to
// cascade). Nothing happens when rq.p is not a guest.
func (a *App) adoptGuest(rq *request, account int64) error {
	if rq.p == nil || !rq.p.Guest || rq.p.ID == account {
		return nil
	}
	moved, err := a.Reg.Adopt(rq.ctx(), rq.p.ID, account)
	if err != nil {
		return err
	}
	a.endAllSessions(rq.ctx(), rq.p.ID)
	if err := a.auth.Delete(rq.ctx(), rq.p.ID); err != nil {
		return err
	}
	a.log.Info("guest adopted", "guest", rq.p.ID, "account", account, "rows", moved)
	return nil
}

// requireAccount is requireLogin for what a guest may not do: their own
// page, their feed.
func (a *App) requireAccount(rq *request) error {
	if !rq.p.SignedIn() {
		return errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	return nil
}
