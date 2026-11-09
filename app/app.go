// Package app serves one data-driven application: its pages (drawn by the
// framework's web-ux package from the app's page files), a JSON API with
// OpenAPI, sign-up and sessions, the model's access rules, file uploads
// mapped by the model's import blocks, and a calendar feed.
//
// It is a plain http.Handler-style component; the Caddy module in the parent
// package wraps it, and tests drive it with httptest.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/schema"
	"github.com/timeofmind/data-driven-framework-go/ux"
	webux "github.com/timeofmind/data-driven-framework-go/web-ux"
)

// Config configures an App. Everything about the application itself comes
// from its directory (AppDir: app.yaml, model, pages, theme); the rest is
// deployment.
type Config struct {
	AppDir string
	Driver string // mysql | sqlite
	DSN    string

	BasePath string   // mount point, e.g. "" or "/app"
	Admins   []string // login values (e.g. emails) granted admin rights

	SecureCookies bool
	SessionTTL    time.Duration

	UploadDir      string // where original uploads are kept ("" = not kept)
	MaxUploadBytes int64

	// ClientIP returns the client address for rate limiting; defaults to RemoteAddr.
	ClientIP func(*http.Request) string
	Logger   *slog.Logger
}

// App serves one application.
type App struct {
	cfg     Config
	man     *Manifest
	uploads *runtime.Handle // the upload view's class, if any
	Model   *model.Model
	Reg     *runtime.Registry
	Pages   *ux.Site
	engine  *ux.Engine
	site    *webux.Site
	auth    *runtime.Handle
	bySlug  map[string]*runtime.Handle // API resources
	limiter *limiter
	log     *slog.Logger
	admins  map[string]bool
	loc     *time.Location
}

// New loads the model and pages, connects, brings the database up to date
// (never dropping anything) and prepares the pages' templates.
func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.AppDir == "" {
		return nil, errors.New("datadriven: app directory is required")
	}
	man, err := LoadManifest(cfg.AppDir)
	if err != nil {
		return nil, err
	}
	if man.Name == "" {
		man.Name = "Data-driven app"
	}
	if man.Timezone == "" {
		man.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(man.Timezone)
	if err != nil {
		return nil, fmt.Errorf("app.yaml timezone: %w", err)
	}
	files, err := man.ModelFiles()
	if err != nil {
		return nil, err
	}
	pageFiles, err := man.PageFiles()
	if err != nil {
		return nil, err
	}
	cfg.BasePath = strings.TrimRight(cfg.BasePath, "/")
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.MaxUploadBytes == 0 {
		cfg.MaxUploadBytes = 25 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m, err := schema.LoadFiles(files...)
	if err != nil {
		return nil, err
	}
	pages, err := ux.Load(m, pageFiles...)
	if err != nil {
		return nil, err
	}
	reg, err := runtime.Open(m, cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("datadriven: database: %w", err)
	}
	reg.Location = loc // "today" in the model's rules
	res, err := reg.EnsureSchema(ctx)
	if err != nil {
		reg.Close()
		return nil, fmt.Errorf("datadriven: schema: %w", err)
	}
	for _, t := range res.CreatedTables {
		cfg.Logger.Info("created table", "table", t)
	}
	for _, c := range res.AddedColumns {
		cfg.Logger.Info("added column", "column", c)
	}
	for _, p := range res.Pending {
		cfg.Logger.Warn("schema difference needs a manual migration", "detail", p)
	}
	a := &App{cfg: cfg, man: man, Model: m, Reg: reg, Pages: pages, bySlug: map[string]*runtime.Handle{},
		limiter: newLimiter(), log: cfg.Logger, admins: map[string]bool{}, loc: loc}
	for _, ad := range cfg.Admins {
		if ad = strings.ToLower(strings.TrimSpace(ad)); ad != "" {
			a.admins[ad] = true
		}
	}
	for _, n := range reg.ClassNames() {
		h := reg.MustClass(n)
		a.bySlug[generator.ResourcePath(h.Class)] = h
	}
	if h, err := reg.AuthClass(); err == nil {
		a.auth = h
	}
	fail := func(err error) (*App, error) {
		reg.Close()
		return nil, err
	}
	if c := pages.UploadClass(); c != nil {
		if len(acceptedFormats(c)) == 0 {
			return fail(fmt.Errorf("upload: none of %v has a file reader (available: %v)", c.Import.Formats, importerNames()))
		}
		a.uploads = reg.MustClass(c.Name)
	}
	if err := a.createSessionTable(ctx); err != nil {
		return fail(err)
	}
	if err := a.createFeedTable(ctx); err != nil {
		return fail(err)
	}
	if a.engine, err = ux.NewEngine(pages, reg, cfg.Logger, loc); err != nil {
		return fail(err)
	}
	a.site, err = webux.New(webux.Options{
		Engine: a.engine, Name: man.Name, Tagline: man.Tagline, Nav: man.Nav, ThemeDir: man.ThemeDir(),
		Signup: a.auth != nil && man.signupEnabled(), Log: cfg.Logger,
		Upload: a.uploadView, UploadInfo: a.uploadInfo,
		SignedOut: func(rq *webux.Request) {
			if rq.Viewer != nil {
				a.endAllSessions(rq.R.Context(), rq.Viewer.ID)
			}
			a.clearSession(rq.W)
		},
	})
	if err != nil {
		return fail(err)
	}
	return a, nil
}

// Close releases the database.
func (a *App) Close() error { return a.Reg.Close() }

// ServeHTTP serves the app, answering 404 for paths it does not own.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.Handle(w, r) {
		http.NotFound(w, r)
	}
}

// Handle serves r if the path belongs to the app and reports whether it did.
// Unclaimed paths are left for the next handler (e.g. Caddy's file server).
func (a *App) Handle(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if a.cfg.BasePath != "" {
		if path != a.cfg.BasePath && !strings.HasPrefix(path, a.cfg.BasePath+"/") {
			return false
		}
		path = strings.TrimPrefix(path, a.cfg.BasePath)
	}
	if path == "" {
		path = "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	rq := &request{app: a, w: w, r: r}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")

	feedPage, _ := a.Pages.FeedCalendar()
	switch {
	case parts[0] == "static" && len(parts) >= 2:
		a.site.ServeStatic(a.webRequest(rq), strings.Join(parts[1:], "/"))
	case parts[0] == "_dd" && len(parts) == 2:
		if !a.site.ServeScript(a.webRequest(rq), parts[1]) {
			http.NotFound(w, r)
		}
	case path == "/signup" && a.auth != nil && a.man.signupEnabled():
		a.wrap(rq, a.signup)
	case path == "/login" && a.auth != nil:
		a.wrap(rq, a.login)
	case path == "/logout" && a.auth != nil:
		a.wrap(rq, a.logout)
	case path == "/me" && a.auth != nil:
		a.wrap(rq, a.me)
	case feedPage != nil && a.auth != nil && path == strings.TrimRight(feedPage.Path, "/")+"/feed":
		a.wrap(rq, a.feedPage)
	case feedPage != nil && a.auth != nil && strings.HasPrefix(path, strings.TrimRight(feedPage.Path, "/")+"/feed/"):
		a.feedICS(w, r, strings.TrimPrefix(path, strings.TrimRight(feedPage.Path, "/")+"/feed/"))
	case parts[0] == "api" && len(parts) >= 2 && parts[1] == "v1":
		a.api(rq, parts[2:])
	default:
		if p, _ := a.Pages.Match(path); p == nil {
			return false
		}
		a.wrap(rq, func(rq *request) error {
			a.site.ServePage(a.webRequest(rq), path)
			return nil
		})
	}
	return true
}

// request carries per-request state.
type request struct {
	app    *App
	w      http.ResponseWriter
	r      *http.Request
	p      *runtime.Principal
	csrf   string
	handle *runtime.Handle
	id     int64
}

func (rq *request) ctx() context.Context { return rq.r.Context() }

func (a *App) webRequest(rq *request) *webux.Request {
	return &webux.Request{W: rq.w, R: rq.r, Viewer: rq.p, CSRF: rq.csrf, Base: a.cfg.BasePath}
}

// httpError is returned by handlers to produce an error page or JSON error.
type httpError struct {
	status int
	msg    string
	fields runtime.FieldErrors
}

func (e *httpError) Error() string { return e.msg }

func errStatus(status int, msg string) error { return &httpError{status: status, msg: msg} }

var (
	errNotFound  = errStatus(http.StatusNotFound, "Not found")
	errForbidden = errStatus(http.StatusForbidden, "You don't have permission to do that")
)

// wrap resolves the session and CSRF cookie, checks the CSRF token of a
// POST, runs the handler, and renders an error page if it fails.
func (a *App) wrap(rq *request, fn func(*request) error) {
	a.resolveSession(rq)
	a.ensureCSRF(rq)
	if rq.r.Method == http.MethodPost && !a.checkCSRF(rq, rq.r.PostFormValue("_csrf")) {
		if rq.r.PostFormValue("_json") == "1" {
			writeJSON(rq, http.StatusForbidden, map[string]string{"error": "Your page expired. Reload it and try again."})
			return
		}
		a.renderError(rq, errStatus(http.StatusForbidden, "Your form expired. Go back, reload the page and try again."))
		return
	}
	if err := fn(rq); err != nil {
		a.renderError(rq, err)
	}
}

func (a *App) renderError(rq *request, err error) {
	var he *httpError
	if errors.As(err, &he) {
		a.site.RenderError(a.webRequest(rq), he.status, he.msg)
		return
	}
	a.site.Error(a.webRequest(rq), err)
}

func (a *App) url(path string) string { return a.cfg.BasePath + path }

func (a *App) requireLogin(rq *request) error {
	if rq.p == nil {
		return errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	return nil
}

// ---- sessions ----

const (
	sessionCookie = "dd_session"
	csrfCookie    = "dd_csrf"
	sessionsTable = "_dd_sessions"
	timeLayout    = "2006-01-02 15:04:05"
)

func (a *App) createSessionTable(ctx context.Context) error {
	q := a.Reg.Dialect.Quote
	var ddl []string
	if a.Reg.Dialect.Name() == "sqlite" {
		ddl = []string{
			"CREATE TABLE IF NOT EXISTS " + q(sessionsTable) + " (token_hash TEXT PRIMARY KEY, principal_id INTEGER NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL)",
			"CREATE INDEX IF NOT EXISTS " + q("idx_dd_sessions_principal") + " ON " + q(sessionsTable) + " (principal_id)",
		}
	} else {
		ddl = []string{"CREATE TABLE IF NOT EXISTS " + q(sessionsTable) + " (token_hash CHAR(64) NOT NULL PRIMARY KEY, principal_id INT NOT NULL, created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, INDEX idx_dd_sessions_principal (principal_id), INDEX idx_dd_sessions_expires (expires_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"}
	}
	for _, s := range ddl {
		if _, err := a.Reg.DB.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("datadriven: sessions table: %w", err)
		}
	}
	return nil
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *App) startSession(rq *request, principalID int64) error {
	tok := randomToken()
	now := time.Now().UTC()
	q := a.Reg.Dialect.Quote
	_, err := a.Reg.DB.ExecContext(rq.ctx(),
		"INSERT INTO "+q(sessionsTable)+" (token_hash, principal_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		tokenHash(tok), principalID, now.Format(timeLayout), now.Add(a.cfg.SessionTTL).Format(timeLayout))
	if err != nil {
		return err
	}
	// Opportunistic cleanup of expired sessions.
	a.Reg.DB.ExecContext(rq.ctx(), "DELETE FROM "+q(sessionsTable)+" WHERE expires_at < ?", now.Format(timeLayout))
	http.SetCookie(rq.w, &http.Cookie{Name: sessionCookie, Value: tok, Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(a.cfg.SessionTTL.Seconds())})
	// Rotate the CSRF token at login.
	rq.csrf = randomToken()
	a.setCSRFCookie(rq)
	return nil
}

func (a *App) endSession(rq *request) {
	if c, err := rq.r.Cookie(sessionCookie); err == nil {
		a.Reg.DB.ExecContext(rq.ctx(), "DELETE FROM "+a.Reg.Dialect.Quote(sessionsTable)+" WHERE token_hash = ?", tokenHash(c.Value))
	}
	a.clearSession(rq.w)
	rq.p = nil
}

func (a *App) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (a *App) endAllSessions(ctx context.Context, principalID int64) {
	a.Reg.DB.ExecContext(ctx, "DELETE FROM "+a.Reg.Dialect.Quote(sessionsTable)+" WHERE principal_id = ?", principalID)
}

func (a *App) resolveSession(rq *request) {
	if a.auth == nil || rq.p != nil {
		return
	}
	c, err := rq.r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 64 {
		return
	}
	var pid int64
	var expires string
	err = a.Reg.DB.QueryRowContext(rq.ctx(),
		"SELECT principal_id, expires_at FROM "+a.Reg.Dialect.Quote(sessionsTable)+" WHERE token_hash = ?", tokenHash(c.Value)).Scan(&pid, &expires)
	if err != nil {
		return
	}
	if t, err := time.Parse(timeLayout, strings.Replace(expires[:min(len(expires), 19)], "T", " ", 1)); err != nil || time.Now().UTC().After(t) {
		return
	}
	rec, err := a.auth.Load(rq.ctx(), pid)
	if err != nil {
		return
	}
	rq.p = a.principal(rec)
}

func (a *App) principal(rec *runtime.Record) *runtime.Principal {
	p := runtime.PrincipalFor(rec)
	if a.auth != nil && a.admins[strings.ToLower(rec.String(a.auth.Class.Auth.Login))] {
		p.Admin = true
	}
	return p
}

func (a *App) cookiePath() string {
	if a.cfg.BasePath == "" {
		return "/"
	}
	return a.cfg.BasePath + "/"
}

// ---- CSRF (double-submit cookie) ----

func (a *App) ensureCSRF(rq *request) {
	if c, err := rq.r.Cookie(csrfCookie); err == nil && len(c.Value) == 64 {
		rq.csrf = c.Value
		return
	}
	rq.csrf = randomToken()
	a.setCSRFCookie(rq)
}

func (a *App) setCSRFCookie(rq *request) {
	http.SetCookie(rq.w, &http.Cookie{Name: csrfCookie, Value: rq.csrf, Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
}

func (a *App) checkCSRF(rq *request, submitted string) bool {
	c, err := rq.r.Cookie(csrfCookie)
	if err != nil || len(c.Value) != 64 || len(submitted) != 64 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(submitted)) == 1
}

// ---- rate limiting for login / sign-up / uploads ----

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

// allow records a hit for key and reports whether it is within n per window.
func (l *limiter) allow(key string, n int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= n {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > 10000 { // bound memory under abuse
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.ClientIP != nil {
		if ip := a.cfg.ClientIP(r); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) rateLimited(rq *request, action string, n int, window time.Duration) bool {
	return !a.limiter.allow(action+"|"+a.clientIP(rq.r), n, window)
}
