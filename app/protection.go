package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Bot Protection: the admin page of an app behind an edge with the bot
// defence (bot_defence: the hosting's hashcash). The edge challenges every
// write — POST, PUT, PATCH, DELETE — from a visitor it has not verified, and
// asks for a proof on each sign-up, sign-in and lost-password form; a visitor
// verifies in the background on any page (the solver script every page asks
// for). A bot that does not solve reads the app but writes nothing.
//
// Everything is protected unless an admin opts a path out here: a program that
// posts to the app and can't solve (an integration, a webhook). The opt-outs
// are kept in the app's own data (FrameworkBotExempt, dd_bot_exempt), where
// the builder reads them when it writes the app's edge site.

const (
	protectPath      = "/admin/bot-protection"
	protectPathTaken = "/admin/_bot-protection"
)

func (a *App) botExempt() *runtime.Handle { return a.Reg.MustClass("FrameworkBotExempt") }

// exemptPathRe is what an opt-out may be: a path, with * for any part, nothing
// the edge's config could read as anything else.
var exemptPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/*-]{0,199}$`)

type protectRow struct {
	Path, Label, What string
	Protected         bool
}

type protectPage struct {
	Message, Error string
	Rows           []protectRow
	Others         []protectRow // opted-out paths not in the list
	Action         string
}

// writeEndpoints are where the app takes writes, as the edge matches them: its
// own forms, each page with something to send (a form, an action, a drag to
// reorder), the JSON API and the media library. Admin pages are left out (only
// signed-in admins write there); like every other path, they stay protected.
func (a *App) writeEndpoints() []protectRow {
	var out []protectRow
	if a.auth != nil {
		if a.man.signupEnabled() {
			out = append(out, protectRow{Path: "/signup", Label: "Sign-up", What: "a proof on every sign-up"})
		}
		out = append(out, protectRow{Path: "/login", Label: "Sign-in", What: "a proof on every sign-in"})
		if a.resetEnabled() {
			out = append(out,
				protectRow{Path: "/forgot", Label: "Lost password", What: "a proof on every reset mail asked for"},
				protectRow{Path: "/reset", Label: "Choose a new password"})
		}
		out = append(out, protectRow{Path: "/logout", Label: "Sign-out"})
	}
	seen := map[string]bool{}
	var pages []protectRow
	for _, p := range a.Pages.Pages {
		if p.Shared || strings.HasPrefix(p.Path, "/admin/") || p.Path == "/admin" {
			continue
		}
		writes := false
		for _, v := range p.Views {
			switch {
			case v.Type == "form" || v.Type == "chips" || v.Type == "picker" || v.Type == "upload",
				len(v.Actions) > 0, v.Reorder != nil:
				writes = true
			}
		}
		if !writes {
			continue
		}
		pth := pagePattern(p.Path)
		if seen[pth] {
			continue
		}
		seen[pth] = true
		pages = append(pages, protectRow{Path: pth, Label: p.LabelText(), What: "its forms and actions"})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Path < pages[j].Path })
	out = append(out, pages...)
	out = append(out,
		protectRow{Path: "/api/v1/*", Label: "The JSON API", What: "writes by the app's own scripts, and by programs"},
		protectRow{Path: "/_dd/media/*", Label: "Media library uploads"})
	return out
}

var pageParamRe = regexp.MustCompile(`\{[^}]*\}`)

// pagePattern is a page's path as the edge matches it: each {param} any one part.
func pagePattern(p string) string {
	p = strings.TrimRight(pageParamRe.ReplaceAllString(p, "*"), "/")
	if p == "" {
		return "/"
	}
	return p
}

// exemptPaths are the opted-out paths, with who opted each out.
func (a *App) exemptPaths(ctx context.Context) (map[string]*runtime.Record, error) {
	recs, err := a.botExempt().List(ctx, runtime.Query{Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := map[string]*runtime.Record{}
	for _, r := range recs {
		out[r.String("path")] = r
	}
	return out, nil
}

func (a *App) protectionAdmin(rq *request) error {
	if ok, err := a.adminSection(rq, a.protectPath); !ok {
		return err
	}
	ctx := rq.ctx()
	pg := protectPage{Action: a.url(a.protectPath)}
	if rq.r.Method == http.MethodPost {
		msg, err := a.saveProtection(rq)
		var he *httpError
		switch {
		case errors.As(err, &he):
			return err
		case err != nil:
			pg.Error = err.Error()
		default:
			http.Redirect(rq.w, rq.r, a.url(a.protectPath)+"?done="+url.QueryEscape(msg), http.StatusSeeOther)
			return nil
		}
	} else if done := rq.r.URL.Query().Get("done"); done != "" {
		pg.Message = done
	}
	exempt, err := a.exemptPaths(ctx)
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, r := range a.writeEndpoints() {
		r.Protected = exempt[r.Path] == nil
		listed[r.Path] = true
		pg.Rows = append(pg.Rows, r)
	}
	for p, r := range exempt {
		if !listed[p] {
			pg.Others = append(pg.Others, protectRow{Path: p, What: "opted out by " + r.String("added_by")})
		}
	}
	sort.Slice(pg.Others, func(i, j int) bool { return pg.Others[i].Path < pg.Others[j].Path })
	a.site.Render(a.webRequest(rq), "protection", "Bot Protection", pg)
	return nil
}

// saveProtection applies the form: each listed path stays protected while it is
// ticked (protect=<path>) and is opted out when not; "other" opts out one more.
func (a *App) saveProtection(rq *request) (string, error) {
	ctx := rq.ctx()
	if err := rq.r.ParseForm(); err != nil {
		return "", err
	}
	exempt, err := a.exemptPaths(ctx)
	if err != nil {
		return "", err
	}
	keep := map[string]bool{}
	for _, p := range rq.r.PostForm["protect"] {
		keep[p] = true
	}
	actor := rq.p.Name
	if acc, err := a.auth.Load(ctx, rq.p.ID); err == nil {
		actor = acc.String(a.auth.Class.Auth.Login)
	}
	var out, in []string
	for _, p := range rq.r.PostForm["listed"] {
		if !exemptPathRe.MatchString(p) {
			continue
		}
		switch r := exempt[p]; {
		case keep[p] && r != nil:
			if err := a.botExempt().Delete(ctx, r.ID()); err != nil {
				return "", err
			}
			in = append(in, p)
		case !keep[p] && r == nil:
			if err := a.botExempt().Save(ctx, a.botExempt().New(map[string]any{"path": p, "added_by": actor})); err != nil {
				return "", err
			}
			out = append(out, p)
		}
	}
	if o := strings.TrimSpace(rq.r.PostFormValue("other")); o != "" {
		if !exemptPathRe.MatchString(o) {
			return "", fmt.Errorf("%q is not a path: start it with /, and use letters, digits, - . _ ~ / and * (any part)", o)
		}
		if exempt[o] == nil && !contains(out, o) {
			if err := a.botExempt().Save(ctx, a.botExempt().New(map[string]any{"path": o, "added_by": actor})); err != nil {
				return "", err
			}
			out = append(out, o)
		}
	}
	a.log.Info("bot protection changed", "actor", actor, "opted_out", out, "protected", in)
	switch {
	case len(out) == 0 && len(in) == 0:
		return "Nothing changed.", nil
	case len(in) == 0:
		return "Opted out: " + strings.Join(out, ", ") + ". The edge applies it within five minutes.", nil
	case len(out) == 0:
		return "Protected again: " + strings.Join(in, ", ") + ". The edge applies it within five minutes.", nil
	}
	return "Opted out: " + strings.Join(out, ", ") + "; protected again: " + strings.Join(in, ", ") + ". The edge applies it within five minutes.", nil
}
