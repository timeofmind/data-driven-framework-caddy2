package importer

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CSV reads split/lap exports from timing equipment (lap counters, chip
// timing, track timing systems, rowing ergs). Each row is one lap of a single
// activity. Recognised header names (case-insensitive):
//
//	date | start | start_time | timestamp   when the lap (or activity) started
//	sport | type | activity                 sport of the activity
//	distance_km | km | distance_mi | miles | distance_m | meters | distance
//	time | duration | elapsed | split | lap_time | seconds
//	hr | avg_hr | heart_rate | max_hr
//	device | source                         timing equipment name
//
// A bare "distance" column is read as km when every value is under 200, else metres.
// Times accept h:mm:ss, mm:ss(.s) or seconds.
type CSV struct{}

func (CSV) Name() string { return "csv" }

func (CSV) Detect(filename string, head []byte) bool {
	if !strings.EqualFold(filepath.Ext(filename), ".csv") {
		return false
	}
	first := strings.ToLower(string(head[:min(len(head), bytes.IndexByte(append(head, '\n'), '\n'))]))
	return strings.Contains(first, "time") || strings.Contains(first, "duration") || strings.Contains(first, "split") || strings.Contains(first, "distance")
}

func (CSV) Parse(r io.Reader) ([]*Activity, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("expected a header row and at least one data row")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(strings.ReplaceAll(h, " ", "_")))] = i
	}
	find := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}
		return -1
	}
	cStart := find("start_time", "start", "date", "timestamp", "datetime")
	cSport := find("sport", "type", "activity")
	cTime := find("time", "duration", "elapsed", "split", "lap_time", "seconds")
	cHR := find("avg_hr", "hr", "heart_rate", "average_hr")
	cMax := find("max_hr")
	cDev := find("device", "source")
	distCol, distScale := -1, 0.0
	for _, d := range []struct {
		name  string
		scale float64
	}{{"distance_km", 1000}, {"km", 1000}, {"distance_mi", 1609.344}, {"miles", 1609.344}, {"mi", 1609.344},
		{"distance_m", 1}, {"meters", 1}, {"metres", 1}, {"distance", 0}} {
		if i, ok := col[d.name]; ok {
			distCol, distScale = i, d.scale
			break
		}
	}
	if cTime < 0 && distCol < 0 {
		return nil, fmt.Errorf("no time or distance column in header %v", rows[0])
	}
	data := rows[1:]
	if distScale == 0 && distCol >= 0 {
		distScale = 1000
		for _, row := range data {
			if v, err := strconv.ParseFloat(cell(row, distCol), 64); err == nil && v >= 200 {
				distScale = 1
				break
			}
		}
	}
	a := &Activity{}
	var hrWeighted, hrSecs float64
	for n, row := range data {
		if strings.TrimSpace(strings.Join(row, "")) == "" {
			continue
		}
		var lap Lap
		if cTime >= 0 {
			secs, ok := parseClock(cell(row, cTime))
			if !ok {
				return nil, fmt.Errorf("row %d: cannot read time %q", n+2, cell(row, cTime))
			}
			lap.Elapsed, lap.Moving = secs, secs
		}
		if distCol >= 0 && cell(row, distCol) != "" {
			v, err := strconv.ParseFloat(cell(row, distCol), 64)
			if err != nil {
				return nil, fmt.Errorf("row %d: cannot read distance %q", n+2, cell(row, distCol))
			}
			lap.DistanceM = v * distScale
		}
		if cStart >= 0 {
			if t, ok := parseAnyTime(cell(row, cStart)); ok {
				lap.Start = t
			}
		}
		if cHR >= 0 {
			lap.AvgHR, _ = strconv.Atoi(cell(row, cHR))
			if lap.AvgHR > 0 {
				hrWeighted += float64(lap.AvgHR) * lap.Elapsed
				hrSecs += lap.Elapsed
			}
		}
		if cMax >= 0 {
			lap.MaxHR, _ = strconv.Atoi(cell(row, cMax))
		}
		if lap.MaxHR > a.MaxHR {
			a.MaxHR = lap.MaxHR
		}
		if a.Sport == "" && cSport >= 0 {
			a.Sport = canonicalSport(cell(row, cSport))
		}
		if a.Device == "" && cDev >= 0 {
			a.Device = cell(row, cDev)
		}
		a.Laps = append(a.Laps, lap)
	}
	if len(a.Laps) == 0 {
		return nil, fmt.Errorf("no data rows")
	}
	if hrSecs > 0 {
		a.AvgHR = int(hrWeighted/hrSecs + 0.5)
	}
	if a.Laps[0].Start.IsZero() {
		return nil, fmt.Errorf("a date or start_time column is required to place the activity in the log")
	}
	// Chain lap starts from the previous lap's end when rows carry no clock time.
	dateOnly := !hasClock(data, cStart)
	for i := 1; i < len(a.Laps); i++ {
		if a.Laps[i].Start.IsZero() || dateOnly {
			a.Laps[i].Start = a.Laps[i-1].Start.Add(time.Duration(a.Laps[i-1].Elapsed * float64(time.Second)))
		}
	}
	if len(a.Laps) == 1 {
		a.Elapsed, a.Moving, a.DistanceM = a.Laps[0].Elapsed, a.Laps[0].Moving, a.Laps[0].DistanceM
		a.Start = a.Laps[0].Start
		a.Laps = nil
	}
	return []*Activity{a}, nil
}

func hasClock(rows [][]string, c int) bool {
	return c >= 0 && len(rows) > 0 && strings.ContainsAny(cell(rows[0], c), ":T")
}

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// parseClock reads h:mm:ss(.s), mm:ss(.s) or plain seconds.
func parseClock(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	var total float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || (i > 0 && f >= 60) {
			return 0, false
		}
		total = total*60 + f
	}
	return total, true
}

func parseAnyTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "01/02/2006 15:04:05", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
