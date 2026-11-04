// Package importer turns files from GPS watches and timing equipment into a
// device-neutral Activity. Importers are pluggable: Register adds one, and
// Parse picks the first importer whose Detect accepts the file.
package importer

import (
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Activity is one recorded workout, independent of the source format.
type Activity struct {
	Sport       string    // canonical: run, ride, swim, walk, hike, row, ski, strength, other
	SubSport    string    // source-specific detail, e.g. "trail", "treadmill", "track"
	Start       time.Time // UTC
	Elapsed     float64   // seconds, including pauses
	Moving      float64   // seconds, timer time (excludes pauses); 0 if unknown
	DistanceM   float64
	AscentM     float64
	AvgHR       int
	MaxHR       int
	Calories    int
	AvgCadence  int
	AvgPowerW   int
	Device      string // e.g. "Garmin Forerunner 255", "COROS PACE 3"
	Format      string // importer name: fit, gpx, tcx, csv
	Laps        []Lap
	TrackPoints int
}

// Lap is one lap or split of an activity.
type Lap struct {
	Start     time.Time
	Elapsed   float64
	Moving    float64
	DistanceM float64
	AscentM   float64
	AvgHR     int
	MaxHR     int
}

// Importer converts one file format.
type Importer interface {
	// Name is a short identifier ("fit", "gpx", …).
	Name() string
	// Detect reports whether the file looks like this format. head holds the
	// first bytes of the file (at least 512 when the file is that long).
	Detect(filename string, head []byte) bool
	// Parse reads the whole file and returns one or more activities
	// (multisport files may contain several).
	Parse(r io.Reader) ([]*Activity, error)
}

var (
	mu        sync.RWMutex
	importers []Importer
)

// Register adds an importer. Later registrations are tried first, so a
// specialised importer can take precedence over a built-in one.
func Register(i Importer) {
	mu.Lock()
	defer mu.Unlock()
	importers = append([]Importer{i}, importers...)
}

// Names lists the registered importers.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	var out []string
	for _, i := range importers {
		out = append(out, i.Name())
	}
	sort.Strings(out)
	return out
}

// ErrUnsupported is returned when no importer recognises a file.
var ErrUnsupported = errors.New("unsupported file format")

// Parse detects the format and parses the file.
func Parse(filename string, data []byte) ([]*Activity, string, error) {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	mu.RLock()
	list := append([]Importer(nil), importers...)
	mu.RUnlock()
	for _, imp := range list {
		if !imp.Detect(filename, head) {
			continue
		}
		acts, err := imp.Parse(strings.NewReader(string(data)))
		if err != nil {
			return nil, imp.Name(), fmt.Errorf("%s: %w", imp.Name(), err)
		}
		if len(acts) == 0 {
			return nil, imp.Name(), fmt.Errorf("%s: file contains no activities", imp.Name())
		}
		for _, a := range acts {
			a.Format = imp.Name()
			a.finish()
		}
		return acts, imp.Name(), nil
	}
	return nil, "", fmt.Errorf("%w: %s (supported: %s)", ErrUnsupported, filepath.Base(filename), strings.Join(Names(), ", "))
}

// finish fills derivable totals from laps when the source lacked them.
func (a *Activity) finish() {
	if a.Sport == "" {
		a.Sport = "other"
	}
	a.Start = a.Start.UTC()
	var dist, el, mv, asc float64
	for _, l := range a.Laps {
		dist += l.DistanceM
		el += l.Elapsed
		mv += l.Moving
		asc += l.AscentM
	}
	if a.DistanceM == 0 {
		a.DistanceM = dist
	}
	if a.Elapsed == 0 {
		a.Elapsed = el
	}
	if a.Moving == 0 {
		a.Moving = mv
	}
	if a.AscentM == 0 {
		a.AscentM = asc
	}
	if a.Start.IsZero() && len(a.Laps) > 0 {
		a.Start = a.Laps[0].Start.UTC()
	}
}

// Title suggests a name like "Morning Run" from the local start hour.
func (a *Activity) Title(loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	h := a.Start.In(loc).Hour()
	part := "Night"
	switch {
	case h >= 5 && h < 12:
		part = "Morning"
	case h >= 12 && h < 17:
		part = "Afternoon"
	case h >= 17 && h < 21:
		part = "Evening"
	}
	label := map[string]string{"run": "Run", "ride": "Ride", "swim": "Swim", "walk": "Walk", "hike": "Hike",
		"row": "Row", "ski": "Ski", "strength": "Strength Session"}[a.Sport]
	if label == "" {
		label = "Workout"
	}
	return part + " " + label
}

// canonicalSport maps common sport names from any source to the canonical set.
func canonicalSport(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return "other"
	case strings.Contains(s, "run") || s == "track" || s == "trail":
		return "run"
	case strings.Contains(s, "cycl") || strings.Contains(s, "bik") || strings.Contains(s, "ride") || s == "biking":
		return "ride"
	case strings.Contains(s, "swim"):
		return "swim"
	case strings.Contains(s, "walk"):
		return "walk"
	case strings.Contains(s, "hik"):
		return "hike"
	case strings.Contains(s, "row"):
		return "row"
	case strings.Contains(s, "ski"):
		return "ski"
	case strings.Contains(s, "strength") || strings.Contains(s, "training"):
		return "strength"
	}
	return "other"
}

func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

func init() {
	Register(CSV{})
	Register(TCX{})
	Register(GPX{})
	Register(FIT{})
}
