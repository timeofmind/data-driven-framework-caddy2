package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is an application's app.yaml: where its data model, pages and
// theme are, and the few settings that belong to neither.
//
//	name: "My App"
//	tagline: "One line under the name"
//	model: [model/*.yaml]            # the data model (globs, relative to the app directory)
//	pages: [pages/*.yaml]            # pages and their interface views (default)
//	routing: [routing/*.yaml]        # listeners and routes (default; optional)
//	theme: theme                     # templates and static/ (default "theme")
//	timezone: America/Vancouver      # "today", and the default for imports
//	signup: {enabled: true, intro: "Shown above the sign-up form"}
//	password_reset: {class: PasswordReset, expires: 1h}   # /forgot and /reset (see reset.go)
//	nav: [calendar, walks, routes]   # page names in the main navigation
type Manifest struct {
	Name     string   `yaml:"name"`
	Tagline  string   `yaml:"tagline"`
	Model    []string `yaml:"model"`
	Pages    []string `yaml:"pages"`
	Routing  []string `yaml:"routing"`
	Theme    string   `yaml:"theme"`
	Timezone string   `yaml:"timezone"`
	Signup   struct {
		Enabled *bool  `yaml:"enabled"`
		Intro   string `yaml:"intro"`
	} `yaml:"signup"`
	// PasswordReset enables /forgot and /reset: Class is the app's class
	// holding reset codes (belongs to the auth class; slots code, expires_at,
	// used_at), Expires how long a code is good for (default 1h).
	PasswordReset struct {
		Class   string `yaml:"class"`
		Expires string `yaml:"expires"`
	} `yaml:"password_reset"`
	Nav []string `yaml:"nav"`
	// Guests tunes guests (the auth class's guest slot enables them): the
	// display name a guest row gets, how many new guests and how many
	// guests' writes an address may make per hour, and how long an idle
	// guest (no change to their row) is kept before the sweep removes them
	// with everything they own ("30d", "720h"; 0 or empty: kept for ever).
	Guests struct {
		Display       string `yaml:"display"`
		PerHour       int    `yaml:"per_hour"`
		WritesPerHour int    `yaml:"writes_per_hour"`
		KeepFor       string `yaml:"keep_for"`
	} `yaml:"guests"`

	dir string
}

// guestDisplay, guestPerHour, guestWritesPerHour, guestKeepFor: the guest
// policy with its defaults.
func (m *Manifest) guestDisplay() string {
	if m.Guests.Display != "" {
		return m.Guests.Display
	}
	return "Guest"
}
func (m *Manifest) guestPerHour() int {
	if m.Guests.PerHour > 0 {
		return m.Guests.PerHour
	}
	return 20
}
func (m *Manifest) guestWritesPerHour() int {
	if m.Guests.WritesPerHour > 0 {
		return m.Guests.WritesPerHour
	}
	return 60
}

// guestKeepFor parses keep_for: a Go duration, or days as "30d".
func (m *Manifest) guestKeepFor() (time.Duration, error) {
	v := strings.TrimSpace(m.Guests.KeepFor)
	if v == "" {
		return 0, nil
	}
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("guests.keep_for: %q", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("guests.keep_for: %q", v)
	}
	return d, nil
}

// LoadManifest reads <dir>/app.yaml. Unknown keys are errors.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("app.yaml: %w", err)
	}
	m.dir = dir
	if m.Theme == "" {
		m.Theme = "theme"
	}
	if len(m.Model) == 0 {
		m.Model = []string{"model/*.yaml"}
	}
	if len(m.Pages) == 0 {
		m.Pages = []string{"pages/*.yaml"}
	}
	if len(m.Routing) == 0 {
		m.Routing = []string{"routing/*.yaml"}
	}
	return &m, nil
}

// RoutingFiles expands the routing globs; none is fine (the app then uses
// the one listener the server configures).
func (m *Manifest) RoutingFiles() []string {
	var out []string
	for _, g := range m.Routing {
		matches, _ := filepath.Glob(filepath.Join(m.dir, g))
		sort.Strings(matches)
		out = append(out, matches...)
	}
	return out
}

func (m *Manifest) files(what string, globs []string) ([]string, error) {
	var out []string
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(m.dir, g))
		if err != nil {
			return nil, fmt.Errorf("app.yaml %s %q: %w", what, g, err)
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			return nil, fmt.Errorf("app.yaml %s %q matches no files", what, g)
		}
		out = append(out, matches...)
	}
	return out, nil
}

// ModelFiles expands the model globs (sorted within each glob).
func (m *Manifest) ModelFiles() ([]string, error) { return m.files("model", m.Model) }

// PageFiles expands the pages globs.
func (m *Manifest) PageFiles() ([]string, error) { return m.files("pages", m.Pages) }

// ThemeDir is the absolute theme directory ("" if it does not exist).
func (m *Manifest) ThemeDir() string {
	d := filepath.Join(m.dir, m.Theme)
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

func (m *Manifest) signupEnabled() bool { return m.Signup.Enabled == nil || *m.Signup.Enabled }
