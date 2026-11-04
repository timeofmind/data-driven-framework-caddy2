package importer

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// GPX reads GPS Exchange files (Strava/COROS/Garmin exports, most apps).
// Totals are computed from the track; heart rate comes from the Garmin
// TrackPointExtension when present.
type GPX struct{}

func (GPX) Name() string { return "gpx" }

func (GPX) Detect(filename string, head []byte) bool {
	return strings.EqualFold(filepath.Ext(filename), ".gpx") || bytes.Contains(head, []byte("<gpx"))
}

type gpxDoc struct {
	Creator  string `xml:"creator,attr"`
	Metadata struct {
		Time string `xml:"time"`
	} `xml:"metadata"`
	Tracks []struct {
		Name     string `xml:"name"`
		Type     string `xml:"type"`
		Segments []struct {
			Points []gpxPoint `xml:"trkpt"`
		} `xml:"trkseg"`
	} `xml:"trk"`
}

type gpxPoint struct {
	Lat  float64 `xml:"lat,attr"`
	Lon  float64 `xml:"lon,attr"`
	Time string  `xml:"time"`
	HR   int     `xml:"extensions>TrackPointExtension>hr"`
	Cad  int     `xml:"extensions>TrackPointExtension>cad"`
	EleV string  `xml:"ele"`
}

func (GPX) Parse(r io.Reader) ([]*Activity, error) {
	var doc gpxDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	var out []*Activity
	for _, trk := range doc.Tracks {
		var pts []trackPoint
		for _, seg := range trk.Segments {
			for _, p := range seg.Points {
				tp := trackPoint{lat: p.Lat, lon: p.Lon, hr: p.HR, cad: p.Cad}
				if p.EleV != "" {
					fmt.Sscan(p.EleV, &tp.ele)
					tp.hasEle = true
				}
				tp.t, _ = parseXMLTime(p.Time)
				pts = append(pts, tp)
			}
		}
		if len(pts) == 0 {
			continue
		}
		a := summarise(pts)
		a.Sport = canonicalSport(firstNonEmpty(trk.Type, trk.Name))
		a.Device = gpxDevice(doc.Creator)
		if a.Start.IsZero() {
			a.Start, _ = parseXMLTime(doc.Metadata.Time)
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no track points found")
	}
	return out, nil
}

func gpxDevice(creator string) string {
	c := strings.TrimSpace(creator)
	lc := strings.ToLower(c)
	switch {
	case strings.Contains(lc, "coros"):
		return "COROS " + strings.TrimSpace(strings.TrimPrefix(c, "COROS"))
	case strings.HasPrefix(lc, "strava"), strings.HasPrefix(lc, "gpsbabel"), c == "":
		return ""
	}
	return c
}

// TCX reads Garmin Training Center XML files.
type TCX struct{}

func (TCX) Name() string { return "tcx" }

func (TCX) Detect(filename string, head []byte) bool {
	return strings.EqualFold(filepath.Ext(filename), ".tcx") || bytes.Contains(head, []byte("TrainingCenterDatabase"))
}

type tcxDoc struct {
	Activities []struct {
		Sport   string `xml:"Sport,attr"`
		ID      string `xml:"Id"`
		Creator struct {
			Name string `xml:"Name"`
		} `xml:"Creator"`
		Laps []struct {
			StartTime string  `xml:"StartTime,attr"`
			Seconds   float64 `xml:"TotalTimeSeconds"`
			Meters    float64 `xml:"DistanceMeters"`
			Calories  int     `xml:"Calories"`
			AvgHR     int     `xml:"AverageHeartRateBpm>Value"`
			MaxHR     int     `xml:"MaximumHeartRateBpm>Value"`
			Points    []struct {
				Time   string  `xml:"Time"`
				Lat    float64 `xml:"Position>LatitudeDegrees"`
				Lon    float64 `xml:"Position>LongitudeDegrees"`
				Ele    string  `xml:"AltitudeMeters"`
				Meters float64 `xml:"DistanceMeters"`
				HR     int     `xml:"HeartRateBpm>Value"`
			} `xml:"Track>Trackpoint"`
		} `xml:"Lap"`
	} `xml:"Activities>Activity"`
}

func (TCX) Parse(r io.Reader) ([]*Activity, error) {
	var doc tcxDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	var out []*Activity
	for _, act := range doc.Activities {
		a := &Activity{Sport: canonicalSport(act.Sport), Device: strings.TrimSpace(act.Creator.Name)}
		a.Start, _ = parseXMLTime(act.ID)
		var all []trackPoint
		var hrSum, hrN float64
		for _, l := range act.Laps {
			var pts []trackPoint
			for _, p := range l.Points {
				tp := trackPoint{lat: p.Lat, lon: p.Lon, hr: p.HR, dist: p.Meters}
				if p.Ele != "" {
					fmt.Sscan(p.Ele, &tp.ele)
					tp.hasEle = true
				}
				tp.t, _ = parseXMLTime(p.Time)
				pts = append(pts, tp)
			}
			sum := summarise(pts)
			lap := Lap{Elapsed: l.Seconds, Moving: l.Seconds, DistanceM: l.Meters, AvgHR: l.AvgHR, MaxHR: l.MaxHR, AscentM: sum.AscentM}
			lap.Start, _ = parseXMLTime(l.StartTime)
			if lap.DistanceM == 0 {
				lap.DistanceM = sum.DistanceM
			}
			if l.AvgHR > 0 {
				hrSum += float64(l.AvgHR) * l.Seconds
				hrN += l.Seconds
			}
			if l.MaxHR > a.MaxHR {
				a.MaxHR = l.MaxHR
			}
			a.Calories += l.Calories
			a.Laps = append(a.Laps, lap)
			all = append(all, pts...)
		}
		if hrN > 0 {
			a.AvgHR = int(math.Round(hrSum / hrN))
		}
		if len(all) > 1 && !all[0].t.IsZero() && !all[len(all)-1].t.IsZero() {
			a.Elapsed = all[len(all)-1].t.Sub(all[0].t).Seconds()
		}
		a.TrackPoints = len(all)
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no activities found")
	}
	return out, nil
}

type trackPoint struct {
	t        time.Time
	lat, lon float64
	ele      float64
	hasEle   bool
	hr, cad  int
	dist     float64 // cumulative distance from the device, if any
}

// summarise computes distance, elapsed/moving time, ascent and HR from points.
func summarise(pts []trackPoint) *Activity {
	a := &Activity{TrackPoints: len(pts)}
	if len(pts) == 0 {
		return a
	}
	a.Start = pts[0].t
	var hrSum, hrN int
	var lastEle float64
	haveEle := false
	for i, p := range pts {
		if p.hr > 0 {
			hrSum += p.hr
			hrN++
			if p.hr > a.MaxHR {
				a.MaxHR = p.hr
			}
		}
		if p.hasEle {
			// Ignore sub-metre noise so GPS jitter doesn't inflate ascent.
			if !haveEle {
				lastEle, haveEle = p.ele, true
			} else if d := p.ele - lastEle; d >= 1 {
				a.AscentM += d
				lastEle = p.ele
			} else if d <= -1 {
				lastEle = p.ele
			}
		}
		if i == 0 {
			continue
		}
		prev := pts[i-1]
		var step float64
		if p.dist > 0 && prev.dist > 0 {
			step = p.dist - prev.dist
		} else if p.lat != 0 || p.lon != 0 {
			step = haversine(prev.lat, prev.lon, p.lat, p.lon)
		}
		a.DistanceM += step
		if !p.t.IsZero() && !prev.t.IsZero() {
			dt := p.t.Sub(prev.t).Seconds()
			if dt > 0 && dt < 60 && step/dt > 0.3 {
				a.Moving += dt
			}
		}
	}
	if last := pts[len(pts)-1]; last.dist > 0 && pts[0].dist >= 0 && a.DistanceM == 0 {
		a.DistanceM = last.dist
	}
	if !pts[0].t.IsZero() && !pts[len(pts)-1].t.IsZero() {
		a.Elapsed = pts[len(pts)-1].t.Sub(pts[0].t).Seconds()
	}
	if hrN > 0 {
		a.AvgHR = int(math.Round(float64(hrSum) / float64(hrN)))
	}
	return a
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371008.8
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

func parseXMLTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05.000"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
