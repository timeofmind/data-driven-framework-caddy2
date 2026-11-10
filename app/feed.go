package app

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/ux"
)

// A calendar view's entries (feed: true) as an iCalendar feed, for Google Calendar ("Add
// other calendars → From URL"), Apple Calendar or Outlook: everything on the
// signed-in person's calendar, at a private URL holding a random token.
// Resetting the token cuts off the old URL at once.

// Feeds cover six months back and eighteen ahead.
const feedMonthsBack, feedMonthsAhead = 6, 18

// absURL is path on this site as a full URL (feeds are read by other services).
func absURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// feedPath is the feed page's path: under the calendar that offers it.
func (a *App) feedPath() string {
	p, _ := a.Pages.FeedCalendar()
	return strings.TrimRight(p.Path, "/") + "/feed"
}

// feedPage shows the person's private feed URL and resets it on POST.
func (a *App) feedPage(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	tok, err := a.feedToken(rq.ctx(), rq.p.ID, rq.r.Method == http.MethodPost)
	if err != nil {
		return err
	}
	if rq.r.Method == http.MethodPost {
		http.Redirect(rq.w, rq.r, a.url(a.feedPath())+"?reset=1", http.StatusSeeOther)
		return nil
	}
	cal, _ := a.Pages.FeedCalendar()
	a.site.Render(a.webRequest(rq), "feed", "Calendar feed", map[string]any{
		"URL":      absURL(rq.r, a.url(a.feedPath()+"/"+tok+".ics")),
		"Reset":    rq.r.URL.Query().Get("reset") == "1",
		"Calendar": a.url(cal.Path),
	})
	return nil
}

// feedICS serves the iCalendar feed for the token in the URL. It needs no
// session: calendar services fetch it on their own schedule.
func (a *App) feedICS(w http.ResponseWriter, r *http.Request, file string) {
	tok := strings.TrimSuffix(file, ".ics")
	if len(tok) != 64 || tok == file || a.auth == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	id, ok := a.feedPrincipal(ctx, tok)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rec, err := a.auth.Load(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := a.principal(rec)
	page, view := a.Pages.FeedCalendar()
	if page == nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	from := now.AddDate(0, -feedMonthsBack, 0).Format("2006-01-02")
	to := now.AddDate(0, feedMonthsAhead, 0).Format("2006-01-02")
	entries, err := a.engine.Entries(&ux.Request{Ctx: ctx, Viewer: p, Base: a.cfg.BasePath}, page, view.Name, from, to)
	if err != nil {
		a.log.Error("calendar feed", "error", err)
		http.Error(w, "calendar unavailable", http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	line := func(s string) { b.WriteString(foldICS(s)) }
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//data-driven-framework-caddy2//calendar feed//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:PUBLISH")
	line("X-WR-CALNAME:" + escICS(a.man.Name))
	line("X-PUBLISHED-TTL:PT1H")
	stamp := now.UTC().Format("20060102T150405Z")
	for _, en := range entries {
		start, err1 := time.Parse("2006-01-02", en.From)
		end, err2 := time.Parse("2006-01-02", en.To)
		if err1 != nil || err2 != nil {
			continue
		}
		line("BEGIN:VEVENT")
		line(fmt.Sprintf("UID:%s-%d@%s", strings.ToLower(en.Rec.Class.Name), en.Rec.ID(), r.Host))
		line("DTSTAMP:" + stamp)
		line("DTSTART;VALUE=DATE:" + start.Format("20060102"))
		line("DTEND;VALUE=DATE:" + end.AddDate(0, 0, 1).Format("20060102")) // all-day: the end is exclusive
		line("SUMMARY:" + escICS(en.Text))
		line("CATEGORIES:" + escICS(en.Category))
		if en.URL != "" {
			line("URL:" + absURL(r, en.URL))
		}
		line("TRANSP:TRANSPARENT")
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("Cache-Control", "private, max-age=900")
	h.Set("Content-Disposition", `inline; filename="calendar.ics"`)
	w.Write([]byte(b.String()))
}

// escICS escapes text values (RFC 5545 §3.3.11).
func escICS(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", "").Replace(s)
}

// foldICS ends a content line with CRLF, folding it into 75-octet pieces
// without splitting a UTF-8 character (RFC 5545 §3.1).
func foldICS(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		size := len(string(r))
		if n+size > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	b.WriteString("\r\n")
	return b.String()
}
