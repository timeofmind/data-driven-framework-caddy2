package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Manifest is an application's app.yaml: everything about the app that is not
// data structure (which lives in its schema files) or styling (its theme).
//
//	name: "My App"
//	tagline: "One line under the name on the home page"
//	schema: [schema/*.yaml]          # globs, relative to the app directory
//	theme: theme                     # layout.html, page templates, static/ (default "theme")
//	timezone: America/Vancouver      # default for imports
//	signup: {enabled: true, intro: "Shown above the sign-up form"}
//	nav: [Meetup, Walk]              # top navigation (default: every top-level class)
//	home:
//	  - {class: Meetup, title: "Upcoming meetups"}
//	  - {class: Walk, mine: true, title: "Your recent walks"}
//	uploads:
//	  class: Walk                    # a class with an import block
//	  title: "Upload GPS files"
//	  intro: "…"
//	  help: ["…", "…"]
type Manifest struct {
	Name     string   `yaml:"name"`
	Tagline  string   `yaml:"tagline"`
	Schema   []string `yaml:"schema"`
	Theme    string   `yaml:"theme"`
	Timezone string   `yaml:"timezone"`
	Signup   struct {
		Enabled *bool  `yaml:"enabled"`
		Intro   string `yaml:"intro"`
	} `yaml:"signup"`
	Nav     []string      `yaml:"nav"`
	Home    []HomeSection `yaml:"home"`
	Uploads *Uploads      `yaml:"uploads"`

	dir string
}

// HomeSection is one list on the home page.
type HomeSection struct {
	Class string `yaml:"class"`
	Title string `yaml:"title"`
	Mine  bool   `yaml:"mine"`  // only the signed-in person's rows (skipped when signed out)
	Limit int    `yaml:"limit"` // default 5
}

// Uploads configures the upload page.
type Uploads struct {
	Class string   `yaml:"class"`
	Title string   `yaml:"title"`
	Intro string   `yaml:"intro"`
	Help  []string `yaml:"help"`
}

// LoadManifest reads <dir>/app.yaml and resolves its schema globs.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("app.yaml: %w", err)
	}
	m.dir = dir
	if m.Theme == "" {
		m.Theme = "theme"
	}
	if len(m.Schema) == 0 {
		m.Schema = []string{"schema/*.yaml"}
	}
	return &m, nil
}

// SchemaFiles expands the schema globs (sorted within each glob).
func (m *Manifest) SchemaFiles() ([]string, error) {
	var out []string
	for _, g := range m.Schema {
		matches, err := filepath.Glob(filepath.Join(m.dir, g))
		if err != nil {
			return nil, fmt.Errorf("app.yaml schema %q: %w", g, err)
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			return nil, fmt.Errorf("app.yaml schema %q matches no files", g)
		}
		out = append(out, matches...)
	}
	return out, nil
}

// ThemeDir is the absolute theme directory ("" if it does not exist).
func (m *Manifest) ThemeDir() string {
	d := filepath.Join(m.dir, m.Theme)
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

func (m *Manifest) signupEnabled() bool { return m.Signup.Enabled == nil || *m.Signup.Enabled }
