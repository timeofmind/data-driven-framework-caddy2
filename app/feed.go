package app

import (
	"context"
	"database/sql"
	"errors"
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

const feedsTable = "_dd_calendar_feeds"

// Feeds cover six months back and eighteen ahead.
const feedMonthsBack, feedMonthsAhead = 6, 18

func (a *App) createFeedTable(ctx context.Context) error {
	q := a.Reg.Dialect.Quote
	ddl := "CREATE TABLE IF NOT EXISTS " + q(feedsTable) + " (principal_id INTEGER NOT NULL PRIMARY KEY, token TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL)"
	if a.Reg.Dialect.Name() != "sqlite" {
		ddl = "CREATE TABLE IF NOT EXISTS " + q(feedsTable) + " (principal_id INT NOT NULL PRIMARY KEY, token CHAR(64) NOT NULL, created_at DATETIME NOT NULL, UNIQUE KEY uq_dd_calendar_feeds_token (token)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	if _, err := a.Reg.DB.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("datadriven: calendar feeds table: %w", err)
	}
	return nil
}

// feedToken returns p's feed token, creating one (or a new one when reset).
func (a *App) feedToken(ctx context.Context, principalID int64, reset bool) (string, error) {
	q := a.Reg.Dialect.Quote
	if !reset {
		var tok string
		err := a.Reg.DB.QueryRowContext(ctx, "SELECT token FROM "+q(feedsTable)+" WHERE principal_id = ?", principalID).Scan(&tok)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	tok := randomToken()
	if _, err := a.Reg.DB.ExecContext(ctx, "DELETE FROM "+q(feedsTable)+" WHERE principal_id = ?", principalID); err != nil {
		return "", err
	}
	_, err := a.Reg.DB.ExecContext(ctx, "INSERT INTO "+q(feedsTable)+" (principal_id, token, created_at) VALUES (?, ?, ?)",
		principalID, tok, time.Now().UTC().Format("2006-01-02 15:04:05"))
	return tok, err
}

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
	var id int64
	err := a.Reg.DB.QueryRowContext(ctx, "SELECT principal_id FROM "+a.Reg.Dialect.Quote(feedsTable)+" WHERE token = ?", tok).Scan(&id)
	if err != nil {
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
