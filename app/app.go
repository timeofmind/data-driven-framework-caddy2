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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timeofmind/data-driven-framework-go/changelog"
	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/appbuilder"
	"github.com/timeofmind/data-driven-framework-go/listener/hooks"
	"github.com/timeofmind/data-driven-framework-go/listener/llm"
	"github.com/timeofmind/data-driven-framework-go/listener/mail"
	"github.com/timeofmind/data-driven-framework-go/listener/payment"
	_ "github.com/timeofmind/data-driven-framework-go/listener/paypal"
	_ "github.com/timeofmind/data-driven-framework-go/listener/s3"
	_ "github.com/timeofmind/data-driven-framework-go/listener/shipping"
	_ "github.com/timeofmind/data-driven-framework-go/listener/sql"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/routing"
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

	// TokenKey signs the fetch tokens forms carry (the same on every node
	// of a cluster); empty: a random key for this process, so tokens
	// expire at a restart (the browser then loads the page again).
	TokenKey []byte
	// NoView runs without the data view layer (reads go to the source).
	NoView bool
	// NoTimers keeps this node from firing the model's timed triggers.
	NoTimers bool
	// LogDir, when set, keeps the change log there: a save is safe once on
	// disk, and the destinations catch up from it.
	LogDir string
	// BaseURL is the site's public URL, for links in mail.
	BaseURL string
	// UMLPath serves the app's UML diagrams there ("" = not served);
	// UMLAdmins limits them to admins.
	UMLPath   string
	UMLAdmins bool
	// Cluster, when set, joins this node to the others (needs LogDir).
	Cluster *ClusterConfig

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
	signer  *ux.Signer
	cancel  context.CancelFunc
	bg      context.Context // the app's lifetime, for what a listener runs in the background
	hooks   hooks.Hooks     // what a destination takes from the app (readyListener)
	// liveMemo: what checking a change for the event streams cost, kept a
	// moment, shared by every stream (live.go).
	liveMemo liveMemo
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
	if cfg.UMLPath != "" {
		cfg.UMLPath = "/" + strings.Trim(cfg.UMLPath, "/")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.MaxUploadBytes == 0 {
		cfg.MaxUploadBytes = 25 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m, err := schema.Load(frameworkSources(files)...)
	if err != nil {
		return nil, err
	}
	pages, err := ux.Load(m, pageFiles...)
	if err != nil {
		return nil, err
	}
	reg, err := openRegistry(m, man, cfg)
	if err != nil {
		return nil, fmt.Errorf("datadriven: database: %w", err)
	}
	reg.Location = loc // "today" in the model's rules
	for name, l := range reg.Listeners {
		if !l.Caps().Schema {
			continue
		}
		res, err := l.Prepare(ctx, m, listener.PrepareOptions{Classes: reg.Holds(name)})
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("datadriven: schema (%s): %w", name, err)
		}
		for _, t := range res.Created {
			cfg.Logger.Info("created table", "listener", name, "table", t)
		}
		for _, c := range res.Added {
			cfg.Logger.Info("added column", "listener", name, "column", c)
		}
		for _, p := range res.Pending {
			cfg.Logger.Warn("schema difference needs a manual migration", "listener", name, "detail", p)
		}
	}
	if added, err := reg.EnsureVersions(ctx); err != nil {
		reg.Close()
		return nil, fmt.Errorf("datadriven: versions: %w", err)
	} else if len(added) > 0 {
		cfg.Logger.Info("added record versions", "tables", added)
	}
	// The settings admins stored on the Service Configuration page: the
	// destinations that have some are opened again with them.
	reopenStored(ctx, reg, cfg.TokenKey, cfg.Logger)
	if !cfg.NoView {
		reg.EnableView()
	}
	bg, cancel := context.WithCancel(context.Background())
	if cfg.LogDir != "" {
		lg, err := changelog.Open(cfg.LogDir, changelog.Options{ModelHash: m.Hash(), AlertBytes: 1 << 30, HardBytes: 8 << 30,
			Alert: func(size int64) { cfg.Logger.Error("the change log is large: a destination is behind", "bytes", size) }})
		if err != nil {
			cancel()
			reg.Close()
			return nil, fmt.Errorf("datadriven: change log: %w", err)
		}
		reg.UseLog(bg, lg, runtime.LogOptions{Logger: cfg.Logger})
	}
	if cfg.Cluster != nil {
		if cfg.LogDir == "" {
			cancel()
			reg.Close()
			return nil, errors.New("datadriven: a cluster needs change_log")
		}
		if err := joinCluster(bg, reg, cfg); err != nil {
			cancel()
			reg.Close()
			return nil, err
		}
	}
	// The model's timed triggers (a clock running out): fired by the
	// cluster's leader, or by the only node, after the log has been replayed.
	if !cfg.NoTimers {
		reg.StartTimers(bg, cfg.Logger)
	}
	// The destinations that answer records (mail, llm, paypal, shipping)
	// take what they need from the app: records and their children for
	// their templates, and the outcome written back onto the record.
	appHooks := hooks.Hooks{
		Logger: cfg.Logger,
		Resolve: func(ctx context.Context, class string, id int64) (map[string]any, error) {
			rec, err := reg.MustClass(class).Load(ctx, id)
			if errors.Is(err, runtime.ErrNotFound) {
				return nil, hooks.ErrGone
			}
			if err != nil {
				return nil, err
			}
			reg.LoadLinks(ctx, rec)
			vals := map[string]any{}
			for k, v := range rec.Values {
				vals[k] = v
			}
			for _, cv := range runtime.Computed(rec) {
				vals[cv.Name] = cv.Value
			}
			return vals, nil
		},
		Children: func(ctx context.Context, class string, id int64, rel string) ([]map[string]any, error) {
			h := reg.MustClass(class)
			rec, err := h.Load(ctx, id)
			if errors.Is(err, runtime.ErrNotFound) {
				return nil, hooks.ErrGone
			}
			if err != nil {
				return nil, err
			}
			kids, err := h.Children(ctx, rec, rel, runtime.Query{Limit: 1000})
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(kids))
			for i, k := range kids {
				reg.LoadLinks(ctx, k)
				vals := map[string]any{}
				for f, v := range k.Values {
					vals[f] = v
				}
				for _, cv := range runtime.Computed(k) {
					vals[cv.Name] = cv.Value
				}
				out[i] = vals
			}
			return out, nil
		},
		Record: recordOutcome(reg),
		// For a destination that follows work it started (appbuilder: a
		// build under way): the records to take up again after a restart,
		// and whether this node leads — its loop runs on the leader alone.
		Find: func(ctx context.Context, class, slot string, value any) ([]int64, error) {
			recs, err := reg.MustClass(class).List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(slot, value)}, Limit: 1000})
			if err != nil {
				return nil, err
			}
			ids := make([]int64, len(recs))
			for i, r := range recs {
				ids[i] = r.ID()
			}
			return ids, nil
		},
		Leading: reg.Leading,
	}
	key := cfg.TokenKey
	if len(key) == 0 {
		key = []byte(randomToken())
	}
	a := &App{cfg: cfg, man: man, Model: m, Reg: reg, Pages: pages, bySlug: map[string]*runtime.Handle{},
		limiter: newLimiter(), log: cfg.Logger, admins: map[string]bool{}, loc: loc, cancel: cancel, bg: bg, hooks: appHooks}
	for _, ad := range cfg.Admins {
		if ad = strings.ToLower(strings.TrimSpace(ad)); ad != "" {
			a.admins[ad] = true
		}
	}
	a.signer = ux.NewSigner(key)
	for _, l := range reg.Listeners {
		if err := a.readyListener(listener.Unwrap(l)); err != nil {
			cancel()
			reg.Close()
			return nil, err
		}
	}
	if p, _ := pages.Match(servicesPath); p != nil {
		cancel()
		reg.Close()
		return nil, fmt.Errorf("%s: the path %s is the server's own (Service Configuration)", p.File, servicesPath)
	}
	for _, n := range reg.ClassNames() {
		if frameworkClass(n) {
			continue
		}
		h := reg.MustClass(n)
		a.bySlug[generator.ResourcePath(h.Class)] = h
	}
	if h, err := reg.AuthClass(); err == nil {
		a.auth = h
	}
	fail := func(err error) (*App, error) {
		cancel()
		reg.Close()
		return nil, err
	}
	if err := a.resetClassOK(); err != nil {
		return fail(err)
	}
	if c := pages.UploadClass(); c != nil {
		if len(acceptedFormats(c)) == 0 {
			return fail(fmt.Errorf("upload: none of %v has a file reader (available: %v)", c.Import.Formats, importerNames()))
		}
		a.uploads = reg.MustClass(c.Name)
	}
	if a.engine, err = ux.NewEngine(pages, reg, cfg.Logger, loc); err != nil {
		return fail(err)
	}
	a.engine.Signer = a.signer
	a.site, err = webux.New(webux.Options{
		Engine: a.engine, Name: man.Name, Tagline: man.Tagline, Nav: man.Nav, ThemeDir: man.ThemeDir(),
		Signup: a.auth != nil && man.signupEnabled(), Reset: a.resetEnabled(), Log: cfg.Logger,
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

// readyListener prepares a listener as the app needs it, at the start and
// again when an admin configures one on the Service Configuration page:
// the hooks a destination answers records through, its template directory
// (every routed class's template must load now, not at the first
// delivery), the templates kept in a class, and what a builder follows.
func (a *App) readyListener(l listener.Listener) error {
	cfg, reg := a.cfg, a.Reg
	if h, ok := l.(hooks.Hooked); ok {
		h.SetHooks(a.hooks)
	}
	if t, ok := l.(hooks.Templated); ok {
		if d := t.Dir(); d != "" && !filepath.IsAbs(d) {
			t.SetDir(filepath.Join(cfg.AppDir, d)) // the app's mail/ or llm/ directory
		}
		if t, ok := l.(interface{ CheckTemplates() error }); ok {
			if err := t.CheckTemplates(); err != nil {
				return err
			}
		}
	}
	if pl, ok := l.(*payment.Listener); ok && pl.BaseURL() == "" {
		pl.SetBaseURL(cfg.BaseURL) // a return_url that is a path of the app
	}
	if ml, ok := l.(*mail.Listener); ok {
		if ml.BaseURL() == "" {
			ml.SetBaseURL(cfg.BaseURL)
		}
		if tc := ml.TemplateClass; tc != "" {
			if reg.Model.Class(tc) == nil {
				return fmt.Errorf("mail listener %s: template_class %s is not in the model", ml.Name(), tc)
			}
			ml.Lookup = func(ctx context.Context, name string) (*mail.Template, error) {
				recs, err := reg.MustClass(tc).List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("name", name)}, Limit: 1})
				if err != nil || len(recs) == 0 {
					return nil, err
				}
				r := recs[0]
				return mail.NewTemplate(r.String("to"), r.String("subject"), r.String("body"), r.String("vars"))
			}
		}
	}
	if ab, ok := l.(*appbuilder.Listener); ok {
		ab.Start(a.bg) // follows the builds under way; on the leader alone
	}
	if ll, ok := l.(*llm.Listener); ok {
		if tc := ll.TemplateClass; tc != "" {
			if reg.Model.Class(tc) == nil {
				return fmt.Errorf("llm listener %s: template_class %s is not in the model", ll.Name(), tc)
			}
			ll.Lookup = func(ctx context.Context, name string) (*llm.Template, error) {
				recs, err := reg.MustClass(tc).List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("name", name)}, Limit: 1})
				if err != nil || len(recs) == 0 {
					return nil, err
				}
				r := recs[0]
				return llm.NewTemplate(r.String("system"), r.String("prompt"), r.String("model"), int(r.Int("max_tokens")), r.Bool("json"), r.String("vars"))
			}
		}
	}
	return nil
}

// openRegistry binds the model to the app's routing files, or, without any,
// to the one database the server configures.
func openRegistry(m *model.Model, man *Manifest, cfg Config) (*runtime.Registry, error) {
	files := man.RoutingFiles()
	if len(files) == 0 {
		return runtime.Open(m, cfg.Driver, cfg.DSN)
	}
	rt, err := routing.Load(files...)
	if err != nil {
		return nil, err
	}
	// A destination the routing names without all its settings (a bucket
	// without its keys) is not a failed start: it waits, parking what flows
	// to it, for an admin to configure it on the Service Configuration page
	// — or for the settings stored there already (reopenStored).
	return runtime.OpenRoutedWith(m, rt, runtime.OpenOptions{Unconfigured: true, Logger: cfg.Logger})
}

// Close stops the delivery loops and releases the listeners and the log.
func (a *App) Close() error {
	a.cancel()
	err := a.Reg.Close()
	if a.Reg.Log != nil {
		if lerr := a.Reg.Log.Close(); lerr != nil && err == nil {
			err = lerr
		}
	}
	return err
}

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
	// Read your writes across the nodes: the browser carries the newest
	// version it was answered with (the seen cookie), and a node that has not
	// applied it yet waits before answering — so a page or an API read that a
	// load balancer sends to the other node is never older than the last
	// write this browser made. The cookie is set as each answer goes out,
	// with what this node had applied by then.
	if a.Reg.View != nil && parts[0] != "static" && parts[0] != "_dd" {
		if c, err := r.Cookie(seenCookie); err == nil {
			if seen, _ := strconv.ParseInt(c.Value, 10, 64); seen > 0 {
				a.Reg.View.WaitApplied(r.Context(), seen, 1500*time.Millisecond)
			}
		}
		w = &seenWriter{ResponseWriter: w, app: a}
	}
	rq := &request{app: a, w: w, r: r}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", a.csp())

	feedPage, _ := a.Pages.FeedCalendar()
	switch {
	case parts[0] == "static" && len(parts) >= 2:
		a.site.ServeStatic(a.webRequest(rq), strings.Join(parts[1:], "/"))
	case path == "/_dd/admin" && a.auth != nil:
		a.wrap(rq, a.adminLink)
	case parts[0] == "_dd" && len(parts) == 2:
		if !a.site.ServeScript(a.webRequest(rq), parts[1]) {
			http.NotFound(w, r)
		}
	case path == servicesPath:
		a.wrap(rq, a.services)
	case path == "/signup" && a.auth != nil && (a.man.signupEnabled() || a.adminSignup(rq) != ""):
		a.wrap(rq, a.signup)
	case path == "/login" && a.auth != nil:
		a.wrap(rq, a.login)
	case path == "/forgot" && a.resetEnabled():
		a.wrap(rq, a.forgot)
	case path == "/reset" && a.resetEnabled():
		a.wrap(rq, a.reset)
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
	case a.cfg.UMLPath != "" && (path == a.cfg.UMLPath || strings.HasPrefix(path, a.cfg.UMLPath+"/")):
		a.wrap(rq, func(rq *request) error {
			a.serveUML(rq, strings.TrimPrefix(path, a.cfg.UMLPath))
			return nil
		})
	default:
		if p, _ := a.Pages.Match(path); p == nil {
			return false
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("_live") == "1" {
			a.wrap(rq, func(rq *request) error { return a.live(rq, path) })
			return true
		}
		a.wrap(rq, func(rq *request) error {
			// A redraw after a push names the newest version the browser was
			// pushed (by any node): this node waits to have applied it, so
			// the page is never older than the push that asked for it.
			if seen, _ := strconv.ParseInt(r.URL.Query().Get("_seen"), 10, 64); seen > 0 {
				a.Reg.View.WaitApplied(rq.ctx(), seen, 1500*time.Millisecond)
			}
			a.site.ServePage(a.webRequest(rq), path)
			return nil
		})
	}
	return true
}

// seenCookie carries the newest applied version a browser was answered with.
const seenCookie = "dd_seen"

// seenWriter sets the seen cookie as the answer's headers go out, to the
// version this node has applied by then — after the request's own writes.
type seenWriter struct {
	http.ResponseWriter
	app  *App
	done bool
}

func (s *seenWriter) stamp() {
	if s.done {
		return
	}
	s.done = true
	if v := s.app.Reg.View.AppliedVersion(); v > 0 {
		http.SetCookie(s.ResponseWriter, &http.Cookie{Name: seenCookie, Value: strconv.FormatInt(v, 10), Path: s.app.cookiePath(),
			HttpOnly: true, Secure: s.app.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	}
}

func (s *seenWriter) WriteHeader(code int) { s.stamp(); s.ResponseWriter.WriteHeader(code) }

func (s *seenWriter) Write(b []byte) (int, error) {
	s.stamp()
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the flusher beneath (the live
// event streams).
func (s *seenWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// request carries per-request state.
type request struct {
	app     *App
	w       http.ResponseWriter
	r       *http.Request
	p       *runtime.Principal
	csrf    string
	session string // the session's hash, for fetch tokens
	handle  *runtime.Handle
	id      int64
}

func (rq *request) ctx() context.Context { return rq.r.Context() }

func (a *App) webRequest(rq *request) *webux.Request {
	wr := &webux.Request{W: rq.w, R: rq.r, Viewer: rq.p, CSRF: rq.csrf, Base: a.cfg.BasePath, Session: rq.session}
	if a.guestsOffered() {
		wr.Guest = func() (*runtime.Principal, error) { return a.newGuest(rq) }
	}
	return wr
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
	if rq.r.Method == http.MethodPost && !a.fetchTokenOK(rq) && !a.checkCSRF(rq, rq.r.PostFormValue("_csrf")) {
		if rq.r.PostFormValue("_json") == "1" {
			writeJSON(rq, http.StatusForbidden, map[string]string{"error": "Your page expired. Reload it and try again."})
			return
		}
		a.renderError(rq, errStatus(http.StatusForbidden, "Your form expired. Go back, reload the page and try again."))
		return
	}
	if rq.r.Method == http.MethodPost && rq.p != nil && rq.p.Guest && a.rateLimited(rq, "guest-write", a.man.guestWritesPerHour(), time.Hour) {
		a.renderError(rq, errStatus(http.StatusTooManyRequests, "Too many changes from your network. Please try again later."))
		return
	}
	if err := fn(rq); err != nil {
		a.renderError(rq, err)
	}
}

// fetchTokenOK reports whether a POST carries a fetch token of this session:
// it then proves the request came from a page the server issued (the
// engine checks the page and view too).
func (a *App) fetchTokenOK(rq *request) bool {
	tok := rq.r.PostFormValue(ux.ParamFetch)
	if tok == "" || rq.session == "" {
		return false
	}
	c, ok := a.signer.Verify(tok)
	return ok && c.Session == rq.session
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
)

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// recordOutcome writes what a destination (mail, a language model) reports
// about a record onto it — the slots the class has, through the normal save
// path, so the outcome is logged, replicated and pushed like any change.
func recordOutcome(reg *runtime.Registry) func(ctx context.Context, class string, id int64, values map[string]any) error {
	return func(ctx context.Context, class string, id int64, values map[string]any) error {
		h := reg.MustClass(class)
		rec, err := h.Load(ctx, id)
		if errors.Is(err, runtime.ErrNotFound) {
			return hooks.ErrGone // deleted before its answer landed: the listener skips it
		}
		if err != nil {
			return err
		}
		for k, v := range values {
			if h.Class.Slot(k) != nil {
				rec.Values[k] = v
			}
		}
		err = h.Save(ctx, rec)
		// The model refusing the outcome (a rule, a kept slot) will refuse it
		// again however often it is delivered: say so, and the entry is parked
		// instead of holding the destination's queue.
		var fe runtime.FieldErrors
		var ke *runtime.KeepError
		if errors.As(err, &fe) || errors.As(err, &ke) {
			return listener.Permanently(err)
		}
		return err
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// startSession begins a session for a principal. rotate renews the CSRF
// token too — at a sign-in or sign-up, where the privilege changes; not when a
// visitor becomes a guest, so a client's token stays good across that write.
func (a *App) startSession(rq *request, principalID int64, rotate bool) error {
	tok, err := a.newSession(rq.ctx(), principalID)
	if err != nil {
		return err
	}
	http.SetCookie(rq.w, &http.Cookie{Name: sessionCookie, Value: tok, Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(a.cfg.SessionTTL.Seconds())})
	if rotate {
		rq.csrf = randomToken()
		a.setCSRFCookie(rq)
	}
	rq.session = tokenHash(tok)
	return nil
}

func (a *App) endSession(rq *request) {
	if c, err := rq.r.Cookie(sessionCookie); err == nil {
		a.dropSessions(rq.ctx(), runtime.Eq("token_hash", tokenHash(c.Value)))
	}
	a.clearSession(rq.w)
	rq.p = nil
	rq.session = ""
}

func (a *App) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: a.cookiePath(), HttpOnly: true,
		Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (a *App) endAllSessions(ctx context.Context, principalID int64) {
	a.dropSessions(ctx, runtime.Eq("principal_id", principalID))
}

func (a *App) resolveSession(rq *request) {
	if a.auth == nil || rq.p != nil {
		return
	}
	c, err := rq.r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 64 {
		return
	}
	hash := tokenHash(c.Value)
	s, ok := a.sessionFor(rq.ctx(), hash)
	if !ok {
		return
	}
	rec, err := a.auth.Load(rq.ctx(), s.Int("principal_id"))
	if err != nil {
		return
	}
	rq.p = a.principal(rec)
	rq.session = hash
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

// amazonPayHosts are what Amazon Pay's checkout needs on the page: its
// script (one host per region) in a frame of its own, and Amazon's pages.
const amazonPayHosts = "https://static-na.payments-amazon.com https://static-eu.payments-amazon.com https://static-fe.payments-amazon.com https://*.payments-amazon.com https://*.amazon.com https://*.amazon.co.uk https://*.amazon.de https://*.amazon.fr https://*.amazon.it https://*.amazon.es https://*.amazon.co.jp"

// csp is the page's Content Security Policy: the app's own origin alone —
// and, while a payment listener has Amazon Pay open (a card ticked on the
// Service Configuration page), Amazon Pay's hosts for its button.
func (a *App) csp() string {
	for _, l := range a.Reg.Listeners {
		pl, ok := listener.Unwrap(l).(*payment.Listener)
		if !ok {
			continue
		}
		for _, k := range pl.Kinds() {
			if k == "amazonpay" {
				return "default-src 'self'; img-src 'self' data: " + amazonPayHosts + "; style-src 'self'; script-src 'self' " + amazonPayHosts +
					"; frame-src " + amazonPayHosts + "; connect-src 'self' " + amazonPayHosts + "; form-action 'self' " + amazonPayHosts + "; frame-ancestors 'none'; base-uri 'none'"
			}
		}
	}
	return "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
}
