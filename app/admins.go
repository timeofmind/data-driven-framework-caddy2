package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Admins: the admin page every app has, where its admins are kept — who
// they are and the authority each has.
//
// There are two kinds. The owners are the logins the server's configuration
// names (admins): always admins, of level 0, and not changed from inside
// the app, so nobody can be locked out. The others are added on the page
// and kept in the app's own data (FrameworkAdmin), so a change holds on
// every node at once, with no restart. An account whose auth class marks it
// an admin (auth: admin) is one too, of level 1.
//
// A level is a number, and a lower number is more authority. A section of
// the admin area that names a level is for admins whose own level is lower
// than it (runtime.Principal.AdminAllowed); the same rule keeps the page
// itself: an admin adds, changes and removes only admins of less authority
// than their own, and gives no level but one below theirs. So an admin
// cannot remove themselves, their peers or an owner, and the last admin is
// never removed.
//
// Someone added who has an account is an admin at once. Anyone else is
// invited: they are an admin once they have signed up through their login
// link — an account made with that login any other way is not made one,
// since the login alone proves nothing.
//
// Every admin signs in through a login link of their own (adminlogin.go),
// made here: when an admin is added, and again whenever one is asked for.
// It is shown once to the admin who made it and mailed to its admin where
// the app can send mail.

const (
	// maxAdminLevel is the least authority an admin can be given.
	maxAdminLevel = 99
	// The levels the server's own sections ask for unless admin.yaml says
	// otherwise: keys and admins are for owners and level 1.
	servicesLevel = 2
	adminsLevel   = 2
)

// adminsPath is where the Admins page is; adminsPathTaken is where it goes
// in an app that has a page of its own at adminsPath.
const (
	adminsPath      = "/admin/admins"
	adminsPathTaken = "/admin/_admins"
)

func (a *App) adminRecords() *runtime.Handle { return a.Reg.MustClass("FrameworkAdmin") }

// dataAdmins is the admins kept in the app's data who have an account: the
// account's id → their level. Read again whenever the data has moved on.
func (a *App) dataAdmins(ctx context.Context) map[int64]int {
	v := int64(-1)
	if a.Reg.View != nil {
		v = a.Reg.View.AppliedVersion()
	}
	a.adminMu.Lock()
	defer a.adminMu.Unlock()
	if a.adminCache != nil && v >= 0 && v == a.adminCacheAt {
		return a.adminCache
	}
	out := map[int64]int{}
	h, err := a.Reg.Class("FrameworkAdmin")
	if err != nil {
		return out
	}
	recs, err := h.List(ctx, runtime.Query{Limit: 10000})
	if err != nil {
		return out // a table not there yet: nobody
	}
	for _, r := range recs {
		if id := r.Int("principal_id"); id > 0 && !r.Bool("pending") {
			out[id] = int(r.Int("level"))
		}
	}
	a.adminCache, a.adminCacheAt = out, v
	return out
}

func (a *App) forgetAdmins() {
	a.adminMu.Lock()
	a.adminCache = nil
	a.adminMu.Unlock()
}

// invited finds the invitation waiting for a login: an admin added before
// they had an account.
func (a *App) invited(ctx context.Context, login string) *runtime.Record {
	if login == "" {
		return nil
	}
	h, err := a.Reg.Class("FrameworkAdmin")
	if err != nil {
		return nil
	}
	r, err := h.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("login", login)}})
	if err != nil || !r.Bool("pending") {
		return nil
	}
	return r
}

// joinAdmins makes the account just signed up through an invitation's link
// the admin the invitation named.
func (a *App) joinAdmins(ctx context.Context, login string, id int64) {
	inv := a.invited(ctx, login)
	if inv == nil {
		return
	}
	inv.Values["principal_id"], inv.Values["pending"] = id, false
	if err := a.adminRecords().Save(ctx, inv); err != nil {
		a.log.Error("an invited admin could not be made one", "login", login, "error", err)
		return
	}
	a.forgetAdmins()
	a.adminEvent(ctx, login, "joined", login, int(inv.Int("level")))
}

// adminEvent keeps a change to the admins: in the app's data, for the page,
// and in the server's log.
func (a *App) adminEvent(ctx context.Context, actor, action, login string, level int) {
	a.log.Info("admins changed", "actor", actor, "action", action, "login", login, "level", level)
	h, err := a.Reg.Class("FrameworkAdminEvent")
	if err != nil {
		return
	}
	if err := h.Save(ctx, h.New(map[string]any{"actor": actor, "action": action, "login": login, "level": int64(level)})); err != nil {
		a.log.Error("a change to the admins could not be recorded", "error", err)
	}
}

// sectionLevel is the level the admin section a path is in asks for: the
// longest section it is under; 0 when it is in none, or the section is any
// admin's.
func (a *App) sectionLevel(path string) int {
	best, level := "", 0
	for _, s := range a.sections {
		if (path == s.URL || strings.HasPrefix(path, s.URL+"/")) && len(s.URL) > len(best) {
			best, level = s.URL, s.Level
		}
	}
	return level
}

// adminSection lets an admin into one of the server's own sections, or says
// why not. A GET by an admin whose authority does not reach it goes to the
// first section theirs does (a layout's Admin link may lead anywhere).
func (a *App) adminSection(rq *request, path string) (bool, error) {
	if rq.p == nil {
		return false, errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	if rq.p.AdminAllowed(a.sectionLevel(path)) {
		return true, nil
	}
	if rq.p.Admin && rq.r.Method == http.MethodGet {
		for _, s := range a.sections {
			if s.URL != path && rq.p.AdminAllowed(s.Level) {
				http.Redirect(rq.w, rq.r, a.url(s.URL), http.StatusSeeOther)
				return false, nil
			}
		}
	}
	return false, errForbidden
}

type adminsPage struct {
	Message, Error        string
	Link, LinkFor         string
	LinkMailed            bool
	LinkDays              int // the days the link just made is good for after each sign-in
	Days, MaxDays         int // a new link's days: the default and the most
	MyLink                string
	OtherAdmins           []ownerRow // admins by the model's auth class (auth: admin)
	Me                    string
	MyLevel               int
	MinLevel, MaxLevel    int
	CanAdd                bool
	LoginLabel, LoginType string
	Owners                []ownerRow
	Admins                []adminRow
	Sections              []adminSectionRow
	Events                []adminEventRow
}

type adminRow struct {
	ID         int64
	Login      string
	Name       string
	Level      int
	Pending    bool
	AddedBy    string
	Manageable bool
	Link       string // how their login link stands
}

// ownerRow is an admin the page does not keep: an owner, or an account the
// model marks an admin.
type ownerRow struct {
	Login, Link string
	CanLink     bool
}

type adminSectionRow struct{ Label, Reach string }

type adminEventRow struct{ At, Actor, What string }

// adminsAdmin serves the Admins page and its changes.
func (a *App) adminsAdmin(rq *request) error {
	if a.auth == nil {
		return errStatus(http.StatusNotFound, "This app has no accounts.")
	}
	if ok, err := a.adminSection(rq, a.adminsPath); !ok {
		return err
	}
	ctx := rq.ctx()
	au := a.auth.Class.Auth
	pg := adminsPage{Me: rq.p.Name, MyLevel: rq.p.Level, MinLevel: rq.p.Level + 1, MaxLevel: maxAdminLevel, Days: loginLinkDays, MaxDays: loginLinkMaxDays, LoginType: "text"}
	pg.CanAdd = pg.MinLevel <= maxAdminLevel
	if me, err := a.auth.Load(ctx, rq.p.ID); err == nil {
		pg.Me = me.String(au.Login)
	}
	pg.LoginLabel = au.Login
	if s := a.auth.Class.Slot(au.Login); s != nil {
		pg.LoginLabel = s.EffectiveLabel()
		if strings.Contains(strings.ToLower(s.Name), "email") {
			pg.LoginType = "email"
		}
	}
	if rq.r.Method == http.MethodPost {
		msg, link, linkFor, err := a.changeAdmins(rq, strings.ToLower(pg.Me))
		var he *httpError
		switch {
		case errors.As(err, &he):
			return err
		case err != nil:
			pg.Error = err.Error()
		case link != "":
			// The link is shown once, on this answer: it is a way in, and
			// does not belong in an address.
			pg.Message, pg.Link, pg.LinkFor = msg, link, linkFor
			if r := a.linkFor(ctx, linkFor); r != nil {
				pg.LinkDays = int(r.Int("days"))
				pg.LinkMailed = a.mailLoginLink(ctx, linkFor, link, pg.LinkDays)
			}
		default:
			http.Redirect(rq.w, rq.r, a.url(a.adminsPath)+"?done="+url.QueryEscape(msg), http.StatusSeeOther)
			return nil
		}
	} else if done := rq.r.URL.Query().Get("done"); done != "" {
		pg.Message = done
	}
	pg.MyLink = a.linkState(ctx, strings.ToLower(pg.Me))
	for login := range a.admins {
		pg.Owners = append(pg.Owners, ownerRow{Login: login, Link: a.linkState(ctx, login), CanLink: rq.p.Level == 0})
	}
	sort.Slice(pg.Owners, func(i, j int) bool { return pg.Owners[i].Login < pg.Owners[j].Login })
	recs, err := a.adminRecords().List(ctx, runtime.Query{Limit: 10000})
	if err != nil {
		return err
	}
	for _, r := range recs {
		row := adminRow{ID: r.ID(), Login: r.String("login"), Level: int(r.Int("level")), Pending: r.Bool("pending"), AddedBy: r.String("added_by")}
		if id := r.Int("principal_id"); id > 0 {
			if acc, err := a.auth.Load(ctx, id); err == nil {
				row.Login = acc.String(au.Login) // the login they have now
				if au.Display != "" {
					row.Name = acc.String(au.Display)
				}
			}
		}
		row.Manageable = rq.p.Level < row.Level
		row.Link = a.linkState(ctx, strings.ToLower(row.Login))
		pg.Admins = append(pg.Admins, row)
	}
	// The accounts the model's auth class marks admins (level 1): not kept
	// here, but they need a login link like any admin.
	if au.Admin != "" {
		kept := a.dataAdmins(ctx)
		marked, _ := a.auth.List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Admin, true)}, Limit: 200})
		for _, acc := range marked {
			login := strings.ToLower(acc.String(au.Login))
			if _, is := kept[acc.ID()]; is || a.admins[login] {
				continue
			}
			pg.OtherAdmins = append(pg.OtherAdmins, ownerRow{Login: login, Link: a.linkState(ctx, login), CanLink: rq.p.Level < 1})
		}
	}
	sort.SliceStable(pg.Admins, func(i, j int) bool {
		if pg.Admins[i].Level != pg.Admins[j].Level {
			return pg.Admins[i].Level < pg.Admins[j].Level
		}
		return pg.Admins[i].Login < pg.Admins[j].Login
	})
	for _, s := range a.sections {
		reach := "every admin"
		switch {
		case s.Level == 1:
			reach = "owners only (level 0)"
		case s.Level == 2:
			reach = "owners and level 1"
		case s.Level > 2:
			reach = fmt.Sprintf("owners and levels 1 to %d", s.Level-1)
		}
		pg.Sections = append(pg.Sections, adminSectionRow{Label: s.Label, Reach: reach})
	}
	if h, err := a.Reg.Class("FrameworkAdminEvent"); err == nil {
		evs, _ := h.List(ctx, runtime.Query{OrderBy: "id desc", Limit: 20})
		for _, e := range evs {
			at := e.String("created_at")
			pg.Events = append(pg.Events, adminEventRow{At: strings.Replace(at[:min(len(at), 16)], "T", " ", 1), Actor: e.String("actor"), What: adminEventText(e)})
		}
	}
	a.site.Render(a.webRequest(rq), "admins", "Admins", pg)
	return nil
}

func adminEventText(e *runtime.Record) string {
	login, level := e.String("login"), e.Int("level")
	switch e.String("action") {
	case "added":
		return fmt.Sprintf("made %s an admin, level %d", login, level)
	case "invited":
		return fmt.Sprintf("invited %s to be an admin, level %d", login, level)
	case "joined":
		return fmt.Sprintf("%s took up the invitation, level %d", login, level)
	case "level":
		return fmt.Sprintf("set %s to level %d", login, level)
	case "removed":
		return fmt.Sprintf("removed %s (level %d)", login, level)
	case "link":
		return fmt.Sprintf("made a new login link for %s", login)
	}
	return e.String("action") + " " + login
}

// changeAdmins does what a post of the page asks: add, level, remove or
// link. It answers what to say, and — for someone invited — the link to
// pass on and whose it is. A refusal the admin can mend is a plain error.
func (a *App) changeAdmins(rq *request, actor string) (msg, link, linkFor string, err error) {
	ctx := rq.ctx()
	f := rq.r.PostForm
	au := a.auth.Class.Auth
	h := a.adminRecords()
	level := func() (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(f.Get("level")))
		if err != nil || n <= rq.p.Level || n > maxAdminLevel {
			return 0, fmt.Errorf("The level must be a number from %d to %d: you can give less authority than your own (level %d), not as much or more.", rq.p.Level+1, maxAdminLevel, rq.p.Level)
		}
		return n, nil
	}
	// row is the admin a change names, if this admin may change them.
	row := func() (*runtime.Record, error) {
		id, _ := strconv.ParseInt(f.Get("id"), 10, 64)
		r, err := h.Load(ctx, id)
		if errors.Is(err, runtime.ErrNotFound) {
			return nil, errors.New("That admin is no longer here.")
		}
		if err != nil {
			return nil, err
		}
		if rq.p.Level >= int(r.Int("level")) {
			return nil, errStatus(http.StatusForbidden, "You can change only admins of less authority than your own.")
		}
		return r, nil
	}
	switch f.Get("action") {
	case "add":
		login := strings.ToLower(strings.TrimSpace(f.Get("login")))
		if login == "" || len(login) > 200 || strings.ContainsAny(login, " \t\r\n") {
			return "", "", "", errors.New("Give the login the admin signs in with.")
		}
		n, err := level()
		if err != nil {
			return "", "", "", err
		}
		days, err := linkDays(f.Get("days"))
		if err != nil {
			return "", "", "", err
		}
		if a.admins[login] {
			return "", "", "", fmt.Errorf("%s is an owner already.", login)
		}
		if _, err := h.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("login", login)}}); err == nil {
			return "", "", "", fmt.Errorf("%s is an admin, or invited, already. Change their level or remove them in the list.", login)
		} else if !errors.Is(err, runtime.ErrNotFound) {
			return "", "", "", err
		}
		vals := map[string]any{"login": login, "level": int64(n), "pending": true, "added_by": actor}
		acc, err := a.auth.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, login)}})
		switch {
		case err == nil && (au.Guest == "" || acc.String(au.Guest) == ""):
			if _, is := a.dataAdmins(ctx)[acc.ID()]; is {
				return "", "", "", fmt.Errorf("%s is an admin already.", login)
			}
			vals["principal_id"], vals["pending"] = acc.ID(), false
		case err != nil && !errors.Is(err, runtime.ErrNotFound):
			return "", "", "", err
		}
		if err := h.Save(ctx, h.New(vals)); err != nil {
			return "", "", "", err
		}
		a.forgetAdmins()
		link, err := a.newLoginLink(rq, login, days, actor)
		if err != nil {
			return "", "", "", err
		}
		if vals["pending"] == false {
			a.adminEvent(ctx, actor, "added", login, n)
			return fmt.Sprintf("%s is now an admin, level %d.", login, n), link, login, nil
		}
		a.adminEvent(ctx, actor, "invited", login, n)
		return fmt.Sprintf("%s is invited, level %d. They have no account yet.", login, n), link, login, nil
	case "level":
		r, err := row()
		if err != nil {
			return "", "", "", err
		}
		n, err := level()
		if err != nil {
			return "", "", "", err
		}
		r.Values["level"] = int64(n)
		if err := h.Save(ctx, r); err != nil {
			return "", "", "", err
		}
		a.forgetAdmins()
		a.adminEvent(ctx, actor, "level", r.String("login"), n)
		return fmt.Sprintf("%s is now level %d.", r.String("login"), n), "", "", nil
	case "remove":
		r, err := row()
		if err != nil {
			return "", "", "", err
		}
		if err := h.Delete(ctx, r.ID()); err != nil {
			return "", "", "", err
		}
		a.forgetAdmins()
		a.dropLoginLink(ctx, r.String("login"))
		a.adminEvent(ctx, actor, "removed", r.String("login"), int(r.Int("level")))
		return fmt.Sprintf("%s is no longer an admin.", r.String("login")), "", "", nil
	case "link":
		// A new login link for an admin: for oneself, for an admin of less
		// authority, or — an owner — for another owner.
		login := strings.ToLower(strings.TrimSpace(f.Get("login")))
		days, err := linkDays(f.Get("days"))
		if err != nil {
			return "", "", "", err
		}
		target := -1
		switch inv := a.invited(ctx, login); {
		case a.admins[login]:
			target = 0
		case inv != nil:
			target = int(inv.Int("level"))
		default:
			if acc, err := a.auth.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(au.Login, login)}}); err == nil {
				if p := a.principal(acc); p.Admin {
					target = p.Level
				}
			}
		}
		if target < 0 {
			return "", "", "", fmt.Errorf("%s is not an admin of this app.", login)
		}
		if login != actor && rq.p.Level >= target && !(target == 0 && rq.p.Level == 0) {
			return "", "", "", errStatus(http.StatusForbidden, "You can make a login link for yourself and for admins of less authority than your own.")
		}
		link, err := a.newLoginLink(rq, login, days, actor)
		if err != nil {
			return "", "", "", err
		}
		a.adminEvent(ctx, actor, "link", login, target)
		return fmt.Sprintf("A new login link for %s. Any link they had before no longer works.", login), link, login, nil
	}
	return "", "", "", errStatus(http.StatusBadRequest, "Unknown action")
}
