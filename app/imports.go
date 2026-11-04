package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-caddy2/importer"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// ImportConfig names the schema classes that uploads of watch / timing files
// populate. Only Activity is required. Slots are found by name and filled only
// when the class has them:
//
//	Activity:  date, start_time, title, sport, distance_km, duration, moving_time,
//	           elevation_gain_m, avg_hr, max_hr, avg_cadence, calories, source,
//	           device, source_file, source_hash, + a belongs_to to Match
//	Lap:       belongs_to Activity, lap_number, start_time, distance_km, duration,
//	           moving_time, avg_hr, max_hr, elevation_gain_m
//	Match:     the planned-workout class (owner, calendar date, sport, status,
//	           target_distance_km); an upload is linked to that day's plan and the
//	           plan's status becomes completed (or partial when well short)
//	Registration / Result: when an activity falls inside the date window of an
//	           Event the athlete registered for, matches its sport and covers its
//	           distance_km (by no more than 10%), a Result (elapsed_time,
//	           distance_km) is posted
type ImportConfig struct {
	Activity     string
	Lap          string
	Match        string
	Registration string
	Result       string
}

func (ic ImportConfig) enabled() bool { return ic.Activity != "" }

func (ic ImportConfig) validate(a *App) error {
	if !ic.enabled() {
		return nil
	}
	need := func(name, role string) (*model.Class, error) {
		if name == "" {
			return nil, nil
		}
		c := a.Model.Class(name)
		if c == nil || c.PKSlot() == nil {
			return nil, fmt.Errorf("datadriven import: %s class %q is not a concrete class in the schema", role, name)
		}
		return c, nil
	}
	act, err := need(ic.Activity, "activity")
	if err != nil {
		return err
	}
	if ownerOf(act) == "" || !act.HasSlot("date") {
		return fmt.Errorf("datadriven import: activity class %s needs an access owner slot and a date slot", act.Name)
	}
	if lap, err := need(ic.Lap, "lap"); err != nil {
		return err
	} else if lap != nil && fkTo(lap, act.Name) == "" {
		return fmt.Errorf("datadriven import: lap class %s must belong_to %s", lap.Name, act.Name)
	}
	if m, err := need(ic.Match, "match"); err != nil {
		return err
	} else if m != nil && (m.Calendar == "" || ownerOf(m) == "" || fkTo(act, m.Name) == "") {
		return fmt.Errorf("datadriven import: match class %s needs a calendar slot and an owner, and %s must belong_to it", m.Name, act.Name)
	}
	reg, err := need(ic.Registration, "registration")
	if err != nil {
		return err
	}
	res, err := need(ic.Result, "result")
	if err != nil {
		return err
	}
	if (reg == nil) != (res == nil) {
		return errors.New("datadriven import: set both import_registration and import_result, or neither")
	}
	if res != nil {
		if ownerOf(reg) == "" || ownerOf(res) == "" || fkTo(res, act.Name) == "" || !res.HasSlot("elapsed_time") {
			return fmt.Errorf("datadriven import: %s/%s need owners, and %s needs elapsed_time and a belongs_to %s", reg.Name, res.Name, res.Name, act.Name)
		}
	}
	return nil
}

// fkTo returns the FK slot of c that belongs_to target.
func fkTo(c *model.Class, target string) string {
	for _, r := range c.AllRelationships() {
		if r.IsBelongsTo() && r.Target == target {
			return r.Via
		}
	}
	return ""
}

// importResult describes one activity found in one uploaded file.
type importResult struct {
	File     string `json:"file"`
	Status   string `json:"status"` // imported | duplicate | error
	Error    string `json:"error,omitempty"`
	Activity *link  `json:"activity,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Matched  *link  `json:"matched_plan,omitempty"`
	Results  []link `json:"event_results,omitempty"`
}

const maxFilesPerUpload = 20

func (a *App) importPage(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	var results []importResult
	if rq.r.Method == http.MethodPost {
		var err error
		results, err = a.handleUpload(rq)
		if err != nil {
			return err
		}
	}
	a.render(rq, "import", "Upload watch data", map[string]any{
		"Results": results, "Formats": importer.Names(), "MaxMB": a.cfg.MaxUploadBytes >> 20,
	})
	return nil
}

// handleUpload reads multipart "files" and imports each one. The CSRF token
// has already been checked by wrap (form) or the API layer.
func (a *App) handleUpload(rq *request) ([]importResult, error) {
	if a.rateLimited(rq, "upload", 120, time.Hour) {
		return nil, errStatus(http.StatusTooManyRequests, "Too many uploads. Please wait a while.")
	}
	rq.r.Body = http.MaxBytesReader(rq.w, rq.r.Body, a.cfg.MaxUploadBytes*maxFilesPerUpload)
	if err := rq.r.ParseMultipartForm(8 << 20); err != nil {
		return nil, errStatus(http.StatusRequestEntityTooLarge, "The upload was too large or malformed.")
	}
	defer rq.r.MultipartForm.RemoveAll()
	files := rq.r.MultipartForm.File["files"]
	if len(files) == 0 {
		return nil, errStatus(http.StatusBadRequest, "Choose at least one file to upload.")
	}
	if len(files) > maxFilesPerUpload {
		return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("Upload at most %d files at a time.", maxFilesPerUpload))
	}
	var out []importResult
	for _, fh := range files {
		name := filepath.Base(fh.Filename)
		if fh.Size > a.cfg.MaxUploadBytes {
			out = append(out, importResult{File: name, Status: "error", Error: fmt.Sprintf("larger than %d MB", a.cfg.MaxUploadBytes>>20)})
			continue
		}
		f, err := fh.Open()
		if err != nil {
			out = append(out, importResult{File: name, Status: "error", Error: "could not read the file"})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, a.cfg.MaxUploadBytes+1))
		f.Close()
		if err != nil {
			out = append(out, importResult{File: name, Status: "error", Error: "could not read the file"})
			continue
		}
		out = append(out, a.ImportFile(rq.ctx(), rq.p, name, data)...)
	}
	return out, nil
}

// ImportFile parses one file and records every activity in it for p.
func (a *App) ImportFile(ctx context.Context, p *runtime.Principal, name string, data []byte) []importResult {
	acts, format, err := importer.Parse(name, data)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, importer.ErrUnsupported) {
			msg = "not a recognised activity file (" + strings.Join(importer.Names(), ", ") + ")"
		}
		return []importResult{{File: name, Status: "error", Error: msg}}
	}
	sum := sha256.Sum256(data)
	fileHash := hex.EncodeToString(sum[:])
	a.keepOriginal(p, fileHash, name, data)
	tz := a.timezoneFor(ctx, p)
	var out []importResult
	for i, act := range acts {
		hash := fileHash
		if i > 0 {
			h2 := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", fileHash, i)))
			hash = hex.EncodeToString(h2[:])
		}
		res := a.importActivity(ctx, p, name, format, hash, act, tz)
		out = append(out, res)
	}
	return out
}

func (a *App) timezoneFor(ctx context.Context, p *runtime.Principal) *time.Location {
	name := a.cfg.DefaultTimezone
	if a.auth != nil && a.auth.Class.HasSlot("timezone") {
		if rec, err := a.auth.Load(ctx, p.ID); err == nil && rec.String("timezone") != "" {
			name = rec.String("timezone")
		}
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (a *App) keepOriginal(p *runtime.Principal, hash, name string, data []byte) {
	if a.cfg.UploadDir == "" {
		return
	}
	dir := filepath.Join(a.cfg.UploadDir, fmt.Sprint(p.ID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		a.log.Warn("upload dir", "error", err)
		return
	}
	ext := strings.ToLower(unsafeName.ReplaceAllString(filepath.Ext(name), ""))
	path := filepath.Join(dir, hash+ext)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		a.log.Warn("keep upload", "error", err)
	}
}

// set assigns v to slot when the record's class has it; zero numbers are skipped.
func set(rec *runtime.Record, slot string, v any) {
	s := rec.Class.Slot(slot)
	if s == nil {
		return
	}
	switch x := v.(type) {
	case float64:
		if x <= 0 || math.IsNaN(x) {
			return
		}
	case int:
		if x <= 0 {
			return
		}
	case string:
		if x == "" {
			return
		}
		if s.Type == "enum" && s.EnumObj != nil && !s.EnumObj.HasValue(x) {
			if s.EnumObj.HasValue("other") {
				x = "other"
			} else {
				return
			}
			v = x
		}
		if s.Max != nil && float64(len([]rune(x))) > *s.Max {
			v = string([]rune(x)[:int(*s.Max)])
		}
	}
	rec.Set(slot, v)
}

func (a *App) importActivity(ctx context.Context, p *runtime.Principal, file, format, hash string, act *importer.Activity, tz *time.Location) importResult {
	ic := a.cfg.Import
	res := importResult{File: file}
	ah := a.Reg.MustClass(ic.Activity)
	owner := ownerOf(ah.Class)
	if ah.Class.HasSlot("source_hash") {
		existing, err := ah.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(owner, p.ID), runtime.Eq("source_hash", hash)}})
		if err == nil {
			res.Status = "duplicate"
			res.Activity = &link{Label: existing.Title(), URL: a.recordURL(ah.Class, existing.ID())}
			return res
		}
	}
	local := act.Start.In(tz)
	rec := ah.New(nil)
	rec.Values[owner] = p.ID
	set(rec, "date", local.Format("2006-01-02"))
	set(rec, "start_time", local.Format("2006-01-02 15:04:05"))
	set(rec, "title", act.Title(tz))
	set(rec, "sport", act.Sport)
	set(rec, "distance_km", math.Round(act.DistanceM)/1000)
	set(rec, "duration", math.Round(act.Elapsed))
	set(rec, "moving_time", math.Round(act.Moving))
	set(rec, "elevation_gain_m", math.Round(act.AscentM))
	set(rec, "avg_hr", act.AvgHR)
	set(rec, "max_hr", act.MaxHR)
	set(rec, "avg_cadence", act.AvgCadence)
	set(rec, "calories", act.Calories)
	set(rec, "source", format)
	set(rec, "device", act.Device)
	set(rec, "source_file", file)
	set(rec, "source_hash", hash)

	var matched *runtime.Record
	if ic.Match != "" {
		matched = a.findPlanned(ctx, p, rec)
		if matched != nil {
			rec.Values[fkTo(ah.Class, ic.Match)] = matched.ID()
		}
	}
	if err := ah.Save(ctx, rec); err != nil {
		res.Status, res.Error = "error", "could not save: "+err.Error()
		return res
	}
	res.Status = "imported"
	res.Activity = &link{Label: rec.Title(), URL: a.recordURL(ah.Class, rec.ID())}
	res.Summary = activitySummary(rec)

	if ic.Lap != "" && len(act.Laps) > 1 {
		lh := a.Reg.MustClass(ic.Lap)
		fk := fkTo(lh.Class, ic.Activity)
		for i, l := range act.Laps {
			lr := lh.New(nil)
			lr.Values[fk] = rec.ID()
			set(lr, "lap_number", i+1)
			if !l.Start.IsZero() {
				set(lr, "start_time", l.Start.In(tz).Format("2006-01-02 15:04:05"))
			}
			set(lr, "distance_km", math.Round(l.DistanceM)/1000)
			set(lr, "duration", math.Round(l.Elapsed))
			set(lr, "moving_time", math.Round(l.Moving))
			set(lr, "avg_hr", l.AvgHR)
			set(lr, "max_hr", l.MaxHR)
			set(lr, "elevation_gain_m", math.Round(l.AscentM))
			if err := lh.Save(ctx, lr); err != nil {
				a.log.Warn("import lap", "activity", rec.ID(), "lap", i+1, "error", err)
			}
		}
	}
	if matched != nil {
		mh := a.Reg.MustClass(ic.Match)
		if matched.Class.HasSlot("status") && (matched.String("status") == "planned" || matched.String("status") == "") {
			status := "completed"
			if t := matched.Float("target_distance_km"); t > 0 && rec.Float("distance_km") < 0.8*t {
				status = "partial"
			}
			matched.Set("status", status)
			mh.Save(ctx, matched)
		}
		res.Matched = &link{Label: matched.Title(), URL: a.recordURL(matched.Class, matched.ID())}
	}
	if ic.Result != "" {
		res.Results = a.postEventResults(ctx, p, rec)
	}
	return res
}

// findPlanned picks p's planned workout for the activity's day: same sport,
// preferring one still marked planned.
func (a *App) findPlanned(ctx context.Context, p *runtime.Principal, act *runtime.Record) *runtime.Record {
	mh := a.Reg.MustClass(a.cfg.Import.Match)
	c := mh.Class
	where := []runtime.Cond{runtime.Eq(ownerOf(c), p.ID), runtime.Eq(c.Calendar, act.String("date"))}
	cands, err := mh.List(ctx, runtime.Query{Where: where})
	if err != nil || len(cands) == 0 {
		return nil
	}
	var best *runtime.Record
	for _, cand := range cands {
		if c.HasSlot("sport") && act.Class.HasSlot("sport") && cand.String("sport") != act.String("sport") {
			continue
		}
		if c.HasSlot("workout_type") && cand.String("workout_type") == "rest" {
			continue
		}
		if !c.HasSlot("status") || cand.String("status") == "planned" {
			return cand
		}
		if best == nil {
			best = cand
		}
	}
	return best
}

// postEventResults records a result for every event the athlete registered
// for whose window, sport and distance the activity satisfies.
func (a *App) postEventResults(ctx context.Context, p *runtime.Principal, act *runtime.Record) []link {
	ic := a.cfg.Import
	rh := a.Reg.MustClass(ic.Registration)
	resH := a.Reg.MustClass(ic.Result)
	regs, err := rh.List(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(ownerOf(rh.Class), p.ID)}})
	if err != nil {
		return nil
	}
	var out []link
	date := act.String("date")
	for _, reg := range regs {
		evRel := rh.Class.AllRelationships()
		var ev *runtime.Record
		for _, r := range evRel {
			if r.IsBelongsTo() && r.Via != ownerOf(rh.Class) {
				if eh, err := a.Reg.Class(r.Target); err == nil {
					ev, _ = eh.Load(ctx, reg.Int(r.Via))
				}
				break
			}
		}
		if ev == nil {
			continue
		}
		if ev.Class.HasSlot("start_date") && ev.String("start_date") > date {
			continue
		}
		if ev.Class.HasSlot("end_date") && ev.String("end_date") != "" && ev.String("end_date") < date {
			continue
		}
		if ev.Class.HasSlot("sport") && act.Class.HasSlot("sport") && ev.String("sport") != "" && ev.String("sport") != act.String("sport") {
			continue
		}
		// The activity must cover the event distance but not by much more: the
		// result time is the whole activity's, so a 16 km long run is not a 10K
		// result (it can still be posted by hand).
		if d := ev.Float("distance_km"); d > 0 && (act.Float("distance_km") < d*0.99 || act.Float("distance_km") > d*1.1) {
			continue
		}
		evFK := fkTo(resH.Class, ev.Class.Name)
		actFK := fkTo(resH.Class, act.Class.Name)
		if evFK == "" || actFK == "" {
			continue
		}
		if _, err := resH.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq(evFK, ev.ID()), runtime.Eq(actFK, act.ID())}}); err == nil {
			continue
		}
		r := resH.New(nil)
		r.Values[ownerOf(resH.Class)] = p.ID
		r.Values[evFK] = ev.ID()
		r.Values[actFK] = act.ID()
		secs := act.Int("moving_time")
		if secs == 0 {
			secs = act.Int("duration")
		}
		if secs == 0 {
			continue
		}
		r.Values["elapsed_time"] = secs
		set(r, "distance_km", act.Float("distance_km"))
		set(r, "notes", "Posted automatically from an uploaded activity")
		if err := resH.Save(ctx, r); err != nil {
			a.log.Warn("auto result", "event", ev.ID(), "error", err)
			continue
		}
		out = append(out, link{Label: ev.Title(), URL: a.recordURL(ev.Class, ev.ID())})
	}
	return out
}

func activitySummary(r *runtime.Record) string {
	var parts []string
	if d := r.String("date"); d != "" {
		parts = append(parts, d)
	}
	if r.Float("distance_km") > 0 {
		parts = append(parts, fmt.Sprintf("%.2f km", r.Float("distance_km")))
	}
	for _, n := range []string{"moving_time", "duration"} {
		if r.Int(n) > 0 {
			parts = append(parts, runtime.FormatDuration(r.Int(n)))
			break
		}
	}
	if pf := paceField(r); pf != nil {
		parts = append(parts, pf.Text)
	}
	return strings.Join(parts, " · ")
}
