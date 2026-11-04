package importer

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
)

// FIT reads Garmin FIT activity files — the native format of Garmin, COROS,
// Suunto, Wahoo, Polar (export) and most other GPS watches and bike computers.
type FIT struct{}

func (FIT) Name() string { return "fit" }

func (FIT) Detect(filename string, head []byte) bool {
	// FIT header: byte 0 = header size (12 or 14), bytes 8..11 = ".FIT".
	return len(head) >= 12 && (head[0] == 12 || head[0] == 14) && bytes.Equal(head[8:12], []byte(".FIT"))
}

func (FIT) Parse(r io.Reader) ([]*Activity, error) {
	dec := decoder.New(r)
	var out []*Activity
	// A file may chain several FIT sequences.
	for dec.Next() {
		f, err := dec.Decode()
		if err != nil {
			return nil, err
		}
		fa := filedef.NewActivity(f.Messages...)
		if fa.FileId.Type != typedef.FileActivity && len(fa.Sessions) == 0 {
			continue
		}
		device := deviceName(fa)
		for si, s := range fa.Sessions {
			a := &Activity{
				Sport:      fitSport(s.Sport),
				SubSport:   fitSubSport(s.SubSport),
				Start:      s.StartTime,
				Elapsed:    finite(s.TotalElapsedTimeScaled()),
				Moving:     finite(s.TotalTimerTimeScaled()),
				DistanceM:  finite(s.TotalDistanceScaled()),
				AscentM:    validU16(s.TotalAscent),
				AvgHR:      validU8(s.AvgHeartRate),
				MaxHR:      validU8(s.MaxHeartRate),
				Calories:   int(validU16(s.TotalCalories)),
				AvgCadence: validU8(s.AvgCadence),
				AvgPowerW:  int(validU16(s.AvgPower)),
				Device:     device,
			}
			if a.Sport == "run" && a.AvgCadence > 0 && a.AvgCadence < 120 {
				a.AvgCadence *= 2 // FIT running cadence is strides per minute (one foot)
			}
			for _, l := range lapsForSession(fa, si, s) {
				a.Laps = append(a.Laps, Lap{
					Start:     l.StartTime,
					Elapsed:   finite(l.TotalElapsedTimeScaled()),
					Moving:    finite(l.TotalTimerTimeScaled()),
					DistanceM: finite(l.TotalDistanceScaled()),
					AscentM:   validU16(l.TotalAscent),
					AvgHR:     validU8(l.AvgHeartRate),
					MaxHR:     validU8(l.MaxHeartRate),
				})
			}
			a.TrackPoints = len(fa.Records)
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no activity sessions found (is this an activity file?)")
	}
	return out, nil
}

// lapsForSession returns the laps belonging to session si, using the
// session's first_lap_index/num_laps when present, else time bounds.
func lapsForSession(fa *filedef.Activity, si int, s *mesgdef.Session) []*mesgdef.Lap {
	if len(fa.Sessions) == 1 {
		return fa.Laps
	}
	first, n := int(s.FirstLapIndex), int(s.NumLaps)
	if s.FirstLapIndex != math.MaxUint16 && s.NumLaps != math.MaxUint16 && first+n <= len(fa.Laps) {
		return fa.Laps[first : first+n]
	}
	var out []*mesgdef.Lap
	end := s.Timestamp
	for _, l := range fa.Laps {
		if !l.StartTime.Before(s.StartTime) && !l.StartTime.After(end) {
			out = append(out, l)
		}
	}
	return out
}

func deviceName(fa *filedef.Activity) string {
	maker := fa.FileId.Manufacturer.String()
	switch fa.FileId.Manufacturer {
	case typedef.ManufacturerGarmin:
		maker = "Garmin"
	case typedef.ManufacturerCoros:
		maker = "COROS"
	case typedef.ManufacturerSuunto:
		maker = "Suunto"
	case typedef.ManufacturerWahooFitness:
		maker = "Wahoo"
	case typedef.ManufacturerPolarElectro:
		maker = "Polar"
	}
	if strings.HasPrefix(maker, "ManufacturerInvalid") || strings.Contains(maker, "(") {
		maker = ""
	}
	product := fa.FileId.ProductName
	if product == "" && fa.FileId.Manufacturer == typedef.ManufacturerGarmin {
		p := typedef.GarminProduct(fa.FileId.Product).String()
		if !strings.Contains(p, "Invalid") && !strings.Contains(p, "(") {
			product = garminModel(strings.TrimPrefix(p, "GarminProduct"))
		}
	}
	return strings.TrimSpace(maker + " " + product)
}

func fitSport(s typedef.Sport) string {
	switch s {
	case typedef.SportRunning:
		return "run"
	case typedef.SportCycling, typedef.SportEBiking:
		return "ride"
	case typedef.SportSwimming:
		return "swim"
	case typedef.SportWalking:
		return "walk"
	case typedef.SportHiking:
		return "hike"
	case typedef.SportRowing:
		return "row"
	case typedef.SportCrossCountrySkiing, typedef.SportAlpineSkiing:
		return "ski"
	case typedef.SportTraining:
		return "strength"
	}
	return canonicalSport(s.String())
}

func fitSubSport(s typedef.SubSport) string {
	switch s {
	case typedef.SubSportTreadmill:
		return "treadmill"
	case typedef.SubSportTrail:
		return "trail"
	case typedef.SubSportTrack:
		return "track"
	case typedef.SubSportStreet, typedef.SubSportGeneric, typedef.SubSportInvalid:
		return ""
	case typedef.SubSportLapSwimming:
		return "pool"
	case typedef.SubSportOpenWater:
		return "open water"
	case typedef.SubSportIndoorCycling, typedef.SubSportVirtualActivity:
		return "indoor"
	}
	return ""
}

func validU8(v uint8) int {
	if v == math.MaxUint8 {
		return 0
	}
	return int(v)
}

func validU16(v uint16) float64 {
	if v == math.MaxUint16 {
		return 0
	}
	return float64(v)
}

// garminModel turns profile product names into marketing names: fr255 ->
// Forerunner 255, fenix7 -> Fenix 7, edge_1040 -> Edge 1040.
func garminModel(p string) string {
	p = strings.ReplaceAll(p, "_", " ")
	for prefix, name := range map[string]string{"fr": "Forerunner", "fenix": "Fenix", "epix": "Epix", "edge": "Edge",
		"venu": "Venu", "vivoactive": "Vivoactive", "instinct": "Instinct", "enduro": "Enduro"} {
		if rest, ok := strings.CutPrefix(strings.ToLower(p), prefix); ok && rest != "" && (rest[0] >= '0' && rest[0] <= '9' || rest[0] == ' ') {
			return name + " " + strings.TrimSpace(rest)
		}
	}
	if p != "" {
		return strings.ToUpper(p[:1]) + p[1:]
	}
	return p
}
