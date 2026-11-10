// Package datadriven is a Caddy v2 HTTP handler that runs a data-driven
// application. The handler is app-agnostic: an application is a directory —
// its app.yaml manifest, its data model and its page files (both
// data-driven-framework-go YAML) and its theme — and the handler serves its
// pages, accounts, a JSON API, calendar feeds and file uploads from it.
//
//	example.com {
//		datadriven {
//			app           /srv/myapp
//			database_file mysql /etc/myapp/database.dsn
//		}
//	}
package datadriven

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/timeofmind/data-driven-framework-caddy2/app"
)

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("datadriven", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("datadriven", httpcaddyfile.Before, "respond")
}

// Handler is the http.handlers.datadriven module.
type Handler struct {
	// App is the application directory (app.yaml, schema files, theme).
	App string `json:"app"`
	// Driver is "mysql" (MySQL/MariaDB) or "sqlite".
	Driver string `json:"driver,omitempty"`
	// DSN is the database DSN; DSNFile reads it from a file (preferred for secrets).
	DSN     string `json:"dsn,omitempty"`
	DSNFile string `json:"dsn_file,omitempty"`
	// BasePath mounts the app below a path prefix ("" = site root).
	BasePath string `json:"base_path,omitempty"`
	// Admins are login values (e.g. emails) with full access.
	Admins []string `json:"admins,omitempty"`
	// SecureCookies marks cookies Secure (default true; off only for plain-HTTP testing).
	SecureCookies *bool          `json:"secure_cookies,omitempty"`
	SessionTTL    caddy.Duration `json:"session_ttl,omitempty"`
	// UploadDir keeps original uploaded files; MaxUploadBytes limits each file.
	UploadDir      string `json:"upload_dir,omitempty"`
	MaxUploadBytes int64  `json:"max_upload_bytes,omitempty"`
	// TokenKeyFile holds the key that signs fetch tokens (hex or raw; the
	// same file on every node of a cluster).
	TokenKeyFile string `json:"token_key_file,omitempty"`
	// View switches the data view layer off ("off").
	View string `json:"view,omitempty"`

	app *app.App
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.datadriven",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision loads the application and connects to the database.
func (h *Handler) Provision(ctx caddy.Context) error {
	dsn := h.DSN
	if h.DSNFile != "" {
		b, err := os.ReadFile(h.DSNFile)
		if err != nil {
			return fmt.Errorf("datadriven: dsn_file: %w", err)
		}
		dsn = strings.TrimSpace(string(b))
	}
	var key []byte
	if h.TokenKeyFile != "" {
		b, err := os.ReadFile(h.TokenKeyFile)
		if err != nil {
			return fmt.Errorf("datadriven: token_key_file: %w", err)
		}
		key = []byte(strings.TrimSpace(string(b)))
	}
	cfg := app.Config{
		TokenKey:       key,
		NoView:         h.View == "off",
		AppDir:         h.App,
		Driver:         h.Driver,
		DSN:            caddy.NewReplacer().ReplaceKnown(dsn, ""),
		BasePath:       h.BasePath,
		Admins:         h.Admins,
		SecureCookies:  h.SecureCookies == nil || *h.SecureCookies,
		SessionTTL:     time.Duration(h.SessionTTL),
		UploadDir:      h.UploadDir,
		MaxUploadBytes: h.MaxUploadBytes,
		Logger:         ctx.Slogger(),
		ClientIP: func(r *http.Request) string {
			if v, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok {
				return v
			}
			return ""
		},
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a, err := app.New(pctx, cfg)
	if err != nil {
		return err
	}
	h.app = a
	return nil
}

// Validate checks the configuration.
func (h *Handler) Validate() error {
	if h.App == "" {
		return fmt.Errorf("datadriven: app directory is required")
	}
	if h.DSN == "" && h.DSNFile == "" {
		return fmt.Errorf("datadriven: database is required")
	}
	return nil
}

// Cleanup closes the database when the config is unloaded.
func (h *Handler) Cleanup() error {
	if h.app != nil {
		return h.app.Close()
	}
	return nil
}

// ServeHTTP serves the application's paths and passes everything else on.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.app.Handle(w, r) {
		return nil
	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile parses:
//
//	datadriven [<base_path>] {
//		app <directory>
//		database <mysql|sqlite> <dsn>
//		database_file <mysql|sqlite> <path-to-file-holding-dsn>
//		admin <login...>
//		secure_cookies on|off
//		session_ttl <duration>
//		uploads <dir> [<max size per file, e.g. 25MB>]
//		token_key_file <path>
//		view on|off
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		h.BasePath = d.Val()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "app":
			if !d.AllArgs(&h.App) {
				return d.ArgErr()
			}
		case "database", "database_file":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return d.ArgErr()
			}
			h.Driver = args[0]
			if d.Val() == "database" {
				h.DSN = args[1]
			} else {
				h.DSNFile = args[1]
			}
		case "admin", "admins":
			h.Admins = append(h.Admins, d.RemainingArgs()...)
		case "secure_cookies":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val() == "on"
			if !v && d.Val() != "off" {
				return d.Errf("secure_cookies: expected on or off, got %q", d.Val())
			}
			h.SecureCookies = &v
		case "session_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("session_ttl: %v", err)
			}
			h.SessionTTL = caddy.Duration(dur)
		case "token_key_file":
			if !d.AllArgs(&h.TokenKeyFile) {
				return d.ArgErr()
			}
		case "view":
			if !d.NextArg() {
				return d.ArgErr()
			}
			if d.Val() != "on" && d.Val() != "off" {
				return d.Errf("view: expected on or off, got %q", d.Val())
			}
			h.View = d.Val()
		case "uploads":
			args := d.RemainingArgs()
			if len(args) < 1 || len(args) > 2 {
				return d.ArgErr()
			}
			h.UploadDir = args[0]
			if len(args) == 2 {
				n, err := parseSize(args[1])
				if err != nil {
					return d.Errf("uploads: %v", err)
				}
				h.MaxUploadBytes = n
			}
		default:
			return d.Errf("unrecognized datadriven option %q (application settings belong in the app's app.yaml)", d.Val())
		}
	}
	return nil
}

func parseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(u, suf.s) {
			u, mult = strings.TrimSuffix(u, suf.s), suf.m
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(u), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Handler
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
