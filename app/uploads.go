package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Uploads turn files into records of the manifest's upload class. The file
// readers (package importer) produce device-neutral fields; what happens to
// them — which slots they fill, what gets linked or created — is entirely the
// class's import block in the app's schema.

// uploadResult describes one item found in one uploaded file.
type uploadResult struct {
	File      string `json:"file"`
	Status    string `json:"status"` // imported | duplicate | error
	Error     string `json:"error,omitempty"`
	Record    *link  `json:"record,omitempty"`
	Summary   string `json:"summary,omitempty"`
	LinkedTo  []link `json:"linked_to,omitempty"`
	CreatedIn []link `json:"created_for,omitempty"`
}

const maxFilesPerUpload = 20

func importerNames() []string { return importer.Names() }

// acceptedFormats are the class's import formats that have a reader.
func acceptedFormats(c *model.Class) []string {
	have := map[string]bool{}
	for _, n := range importer.Names() {
		have[n] = true
	}
	var out []string
	for _, f := range c.Import.Formats {
		if have[f] {
			out = append(out, f)
		}
	}
	return out
}

func (a *App) uploadPage(rq *request) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	if !runtime.CanCreate(a.uploads.Class, rq.p) {
		return errForbidden
	}
	var results []uploadResult
	if rq.r.Method == http.MethodPost {
		var err error
		if results, err = a.handleUpload(rq); err != nil {
			return err
		}
	}
	u := a.man.Uploads
	formats := acceptedFormats(a.uploads.Class)
	accept := make([]string, len(formats))
	for i, f := range formats {
		accept[i] = "." + f
	}
	a.render(rq, "upload", firstNonEmpty(u.Title, "Upload"), map[string]any{
		"Title": firstNonEmpty(u.Title, "Upload"), "Intro": u.Intro, "Help": u.Help,
		"Results": results, "Formats": formats, "Accept": strings.Join(accept, ","),
		"MaxMB": a.cfg.MaxUploadBytes >> 20, "MaxFiles": maxFilesPerUpload,
	})
	return nil
}

// handleUpload reads multipart "files" and imports each one. CSRF has already
// been checked (form token, or X-CSRF-Token for the API).
func (a *App) handleUpload(rq *request) ([]uploadResult, error) {
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
	var out []uploadResult
	for _, fh := range files {
		name := filepath.Base(fh.Filename)
		if fh.Size > a.cfg.MaxUploadBytes {
			out = append(out, uploadResult{File: name, Status: "error", Error: fmt.Sprintf("larger than %d MB", a.cfg.MaxUploadBytes>>20)})
			continue
		}
		f, err := fh.Open()
		if err != nil {
			out = append(out, uploadResult{File: name, Status: "error", Error: "could not read the file"})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, a.cfg.MaxUploadBytes+1))
		f.Close()
		if err != nil {
			out = append(out, uploadResult{File: name, Status: "error", Error: "could not read the file"})
			continue
		}
		out = append(out, a.importFile(rq, name, data)...)
	}
	return out, nil
}

func (a *App) importFile(rq *request, name string, data []byte) []uploadResult {
	c := a.uploads.Class
	acts, format, err := importer.Parse(name, data)
	if err == nil && !contains(acceptedFormats(c), format) {
		err = importer.ErrUnsupported
	}
	if err != nil {
		msg := err.Error()
		if errors.Is(err, importer.ErrUnsupported) {
			msg = "not a supported file (" + strings.Join(acceptedFormats(c), ", ") + ")"
		}
		return []uploadResult{{File: name, Status: "error", Error: msg}}
	}
	sum := sha256.Sum256(data)
	fileHash := hex.EncodeToString(sum[:])
	a.keepOriginal(rq.p, fileHash, name, data)
	tz := a.Reg.ImportTimezone(rq.ctx(), c, rq.p, a.man.Timezone)
	var out []uploadResult
	for i, act := range acts {
		h := fileHash
		if i > 0 {
			h2 := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", fileHash, i)))
			h = hex.EncodeToString(h2[:])
		}
		res := uploadResult{File: name}
		outcome, err := a.uploads.Import(rq.ctx(), rq.p, importSource(act, name, h, tz))
		switch {
		case err != nil:
			a.log.Warn("import", "file", name, "error", err)
			res.Status, res.Error = "error", "could not be saved"
			var fe runtime.FieldErrors
			if errors.As(err, &fe) {
				res.Error = fe.Error()
			}
		case outcome.Duplicate != nil:
			res.Status = "duplicate"
			res.Record = &link{Label: outcome.Duplicate.Title(), URL: a.recordURL(c, outcome.Duplicate.ID())}
		default:
			res.Status = "imported"
			rec := outcome.Record
			res.Record = &link{Label: rec.Title(), URL: a.recordURL(c, rec.ID())}
			res.Summary = recordSummary(rec)
			for _, l := range outcome.Linked {
				res.LinkedTo = append(res.LinkedTo, link{Label: l.Title(), URL: a.recordURL(l.Class, l.ID())})
			}
			for _, cr := range outcome.Created {
				res.CreatedIn = append(res.CreatedIn, link{Label: cr.Title(), URL: a.recordURL(cr.Class, cr.ID())})
			}
		}
		out = append(out, res)
	}
	return out
}

// importSource flattens a parsed activity into the field names an import
// block maps from. Measurements that were not recorded are left out (null).
//
// Fields: sport, sub_sport, title, device, format, file_name, file_hash,
// start_utc, start_local, date_local, elapsed_s, moving_s, distance_m,
// distance_km, ascent_m, avg_hr, max_hr, calories, avg_cadence, avg_power_w,
// track_points. Children "laps": index, start_local, elapsed_s, moving_s,
// distance_m, distance_km, ascent_m, avg_hr, max_hr.
func importSource(act *importer.Activity, file, hash string, tz *time.Location) runtime.ImportSource {
	f := map[string]any{
		"sport": act.Sport, "title": act.Title(tz), "format": act.Format, "file_name": file, "file_hash": hash,
	}
	put := func(k string, v any) {
		switch x := v.(type) {
		case string:
			if x == "" {
				return
			}
		case float64:
			if x <= 0 {
				return
			}
		case int:
			if x <= 0 {
				return
			}
		}
		f[k] = v
	}
	put("sub_sport", act.SubSport)
	put("device", act.Device)
	if !act.Start.IsZero() {
		local := act.Start.In(tz)
		f["start_utc"] = act.Start.UTC().Format("2006-01-02 15:04:05")
		f["start_local"] = local.Format("2006-01-02 15:04:05")
		f["date_local"] = local.Format("2006-01-02")
	}
	put("elapsed_s", act.Elapsed)
	put("moving_s", act.Moving)
	put("distance_m", act.DistanceM)
	if act.DistanceM > 0 {
		f["distance_km"] = float64(int64(act.DistanceM+0.5)) / 1000
	}
	put("ascent_m", act.AscentM)
	put("avg_hr", act.AvgHR)
	put("max_hr", act.MaxHR)
	put("calories", act.Calories)
	put("avg_cadence", act.AvgCadence)
	put("avg_power_w", act.AvgPowerW)
	put("track_points", act.TrackPoints)
	var laps []map[string]any
	for _, l := range act.Laps {
		lm := map[string]any{}
		if !l.Start.IsZero() {
			lm["start_local"] = l.Start.In(tz).Format("2006-01-02 15:04:05")
		}
		for k, v := range map[string]float64{"elapsed_s": l.Elapsed, "moving_s": l.Moving, "distance_m": l.DistanceM, "ascent_m": l.AscentM} {
			if v > 0 {
				lm[k] = v
			}
		}
		if l.DistanceM > 0 {
			lm["distance_km"] = float64(int64(l.DistanceM+0.5)) / 1000
		}
		if l.AvgHR > 0 {
			lm["avg_hr"] = l.AvgHR
		}
		if l.MaxHR > 0 {
			lm["max_hr"] = l.MaxHR
		}
		laps = append(laps, lm)
	}
	return runtime.ImportSource{Fields: f, Children: map[string][]map[string]any{"laps": laps}}
}

// recordSummary is the record's calendar text plus its computed values.
func recordSummary(r *runtime.Record) string {
	parts := []string{}
	if c := r.Class.Calendar; c != "" && r.String(c) != "" {
		parts = append(parts, r.String(c)[:min(10, len(r.String(c)))])
	}
	for _, cv := range runtime.Computed(r) {
		parts = append(parts, cv.Text)
	}
	return strings.Join(parts, " · ")
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

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
