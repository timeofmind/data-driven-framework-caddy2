// Package datadriven is a Caddy v2 HTTP handler that serves a complete web
// application — pages, forms, calendars, a JSON API, accounts and file
// uploads — generated at runtime from a data-driven-framework-go YAML schema.
//
//	example.com {
//		datadriven {
//			schema   /etc/myapp/schema.yaml
//			database mysql {env.MYAPP_DSN}
//			site_name "My App"
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
	// Schema files, merged in order.
	Schema []string `json:"schema"`
	// Driver is "mysql" (MySQL/MariaDB) or "sqlite".
	Driver string `json:"driver,omitempty"`
	// DSN is the database DSN; DSNFile reads it from a file (preferred for secrets).
	DSN     string `json:"dsn,omitempty"`
	DSNFile string `json:"dsn_file,omitempty"`

	SiteName string   `json:"site_name,omitempty"`
	Tagline  string   `json:"tagline,omitempty"`
	Home     []string `json:"home,omitempty"`
	// BasePath mounts the app below a path prefix ("" = site root).
	BasePath string `json:"base_path,omitempty"`
	// Signup enables public account creation (default true).
	Signup *bool `json:"signup,omitempty"`
	// Admins are login values (e.g. emails) with full access.
	Admins []string `json:"admins,omitempty"`
	// SecureCookies marks cookies Secure (default true; turn off only for plain-HTTP testing).
	SecureCookies   *bool          `json:"secure_cookies,omitempty"`
	SessionTTL      caddy.Duration `json:"session_ttl,omitempty"`
	DefaultTimezone string         `json:"timezone,omitempty"`

	UploadDir      string `json:"upload_dir,omitempty"`
	MaxUploadBytes int64  `json:"max_upload_bytes,omitempty"`

	Import app.ImportConfig `json:"import,omitempty"`

	// Templates is a directory of template/CSS overrides.
	Templates string `json:"templates,omitempty"`

	app *app.App
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.datadriven",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision loads the schema and connects to the database.
func (h *Handler) Provision(ctx caddy.Context) error {
	dsn := h.DSN
	if h.DSNFile != "" {
		b, err := os.ReadFile(h.DSNFile)
		if err != nil {
			return fmt.Errorf("datadriven: dsn_file: %w", err)
		}
		dsn = strings.TrimSpace(string(b))
	}
	repl := caddy.NewReplacer()
	dsn = repl.ReplaceKnown(dsn, "")
	cfg := app.Config{
		SchemaFiles:     h.Schema,
		Driver:          h.Driver,
		DSN:             dsn,
		SiteName:        h.SiteName,
		Tagline:         h.Tagline,
		HomeClasses:     h.Home,
		BasePath:        h.BasePath,
		AllowSignup:     h.Signup == nil || *h.Signup,
		Admins:          h.Admins,
		SecureCookies:   h.SecureCookies == nil || *h.SecureCookies,
		SessionTTL:      time.Duration(h.SessionTTL),
		DefaultTimezone: h.DefaultTimezone,
		UploadDir:       h.UploadDir,
		MaxUploadBytes:  h.MaxUploadBytes,
		Import:          h.Import,
		TemplateDir:     h.Templates,
		Logger:          ctx.Slogger(),
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
	if len(h.Schema) == 0 {
		return fmt.Errorf("datadriven: schema is required")
	}
	if h.DSN == "" && h.DSNFile == "" {
		return fmt.Errorf("datadriven: database DSN is required")
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

// ServeHTTP serves app paths and passes everything else to the next handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.app.Handle(w, r) {
		return nil
	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile parses:
//
//	datadriven [<base_path>] {
//		schema <file...>
//		database <mysql|sqlite> <dsn>
//		database_file <mysql|sqlite> <path-to-file-holding-dsn>
//		site_name <name>
//		tagline <text>
//		home <Class...>
//		signup on|off
//		admin <login...>
//		secure_cookies on|off
//		session_ttl <duration>
//		timezone <IANA zone>
//		uploads <dir> [<max size, e.g. 25MB>]
//		templates <dir>
//		activity_import {
//			activity <Class>
//			lap <Class>
//			match <Class>
//			registration <Class>
//			result <Class>
//		}
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		h.BasePath = d.Val()
	}
	onOff := func() (*bool, error) {
		if !d.NextArg() {
			return nil, d.ArgErr()
		}
		v := d.Val() == "on" || d.Val() == "true" || d.Val() == "yes"
		if !v && d.Val() != "off" && d.Val() != "false" && d.Val() != "no" {
			return nil, d.Errf("expected on or off, got %q", d.Val())
		}
		return &v, nil
	}
	for d.NextBlock(0) {
		var err error
		switch d.Val() {
		case "schema":
			h.Schema = append(h.Schema, d.RemainingArgs()...)
			if len(h.Schema) == 0 {
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
		case "site_name":
			if !d.AllArgs(&h.SiteName) {
				return d.ArgErr()
			}
		case "tagline":
			if !d.AllArgs(&h.Tagline) {
				return d.ArgErr()
			}
		case "home":
			h.Home = append(h.Home, d.RemainingArgs()...)
		case "signup":
			if h.Signup, err = onOff(); err != nil {
				return err
			}
		case "secure_cookies":
			if h.SecureCookies, err = onOff(); err != nil {
				return err
			}
		case "admin", "admins":
			h.Admins = append(h.Admins, d.RemainingArgs()...)
		case "session_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("session_ttl: %v", err)
			}
			h.SessionTTL = caddy.Duration(dur)
		case "timezone":
			if !d.AllArgs(&h.DefaultTimezone) {
				return d.ArgErr()
			}
			if _, err := time.LoadLocation(h.DefaultTimezone); err != nil {
				return d.Errf("timezone: %v", err)
			}
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
		case "templates":
			if !d.AllArgs(&h.Templates) {
				return d.ArgErr()
			}
		case "activity_import": // ("import" is reserved by the Caddyfile lexer)
			for nest := d.Nesting(); d.NextBlock(nest); {
				role := d.Val()
				var class string
				if !d.AllArgs(&class) {
					return d.ArgErr()
				}
				switch role {
				case "activity":
					h.Import.Activity = class
				case "lap":
					h.Import.Lap = class
				case "match":
					h.Import.Match = class
				case "registration":
					h.Import.Registration = class
				case "result":
					h.Import.Result = class
				default:
					return d.Errf("activity_import: unknown role %q", role)
				}
			}
		default:
			return d.Errf("unrecognized datadriven option %q", d.Val())
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
