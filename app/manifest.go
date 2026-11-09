package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Manifest is an application's app.yaml: where its data model, pages and
// theme are, and the few settings that belong to neither.
//
//	name: "My App"
//	tagline: "One line under the name"
//	model: [model/*.yaml]            # the data model (globs, relative to the app directory)
//	pages: [pages/*.yaml]            # pages and their interface views (default)
//	theme: theme                     # templates and static/ (default "theme")
//	timezone: America/Vancouver      # "today", and the default for imports
//	signup: {enabled: true, intro: "Shown above the sign-up form"}
//	nav: [calendar, walks, routes]   # page names in the main navigation
type Manifest struct {
	Name     string   `yaml:"name"`
	Tagline  string   `yaml:"tagline"`
	Model    []string `yaml:"model"`
	Pages    []string `yaml:"pages"`
	Theme    string   `yaml:"theme"`
	Timezone string   `yaml:"timezone"`
	Signup   struct {
		Enabled *bool  `yaml:"enabled"`
		Intro   string `yaml:"intro"`
	} `yaml:"signup"`
	Nav []string `yaml:"nav"`

	dir string
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
	return &m, nil
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
