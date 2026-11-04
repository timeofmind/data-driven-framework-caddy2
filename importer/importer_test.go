package importer

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
)

var start = time.Date(2026, 11, 3, 14, 30, 0, 0, time.UTC)

// corosFIT builds a FIT activity like a COROS PACE 3 would write: one running
// session, two laps.
func corosFIT(t *testing.T) []byte {
	t.Helper()
	act := filedef.NewActivity()
	act.FileId = *mesgdef.NewFileId(nil).
		SetType(typedef.FileActivity).
		SetManufacturer(typedef.ManufacturerCoros).
		SetProductName("PACE 3").
		SetTimeCreated(start)
	act.Activity = mesgdef.NewActivity(nil).SetTimestamp(start.Add(50 * time.Minute)).SetNumSessions(1)
	for i := 0; i < 2; i++ {
		ls := start.Add(time.Duration(i) * 25 * time.Minute)
		act.Laps = append(act.Laps, mesgdef.NewLap(nil).
			SetTimestamp(ls.Add(25*time.Minute)).SetStartTime(ls).
			SetTotalElapsedTimeScaled(1500).SetTotalTimerTimeScaled(1490).
			SetTotalDistanceScaled(5000).SetAvgHeartRate(uint8(140+10*i)).SetMaxHeartRate(uint8(160+5*i)))
	}
	act.Sessions = append(act.Sessions, mesgdef.NewSession(nil).
		SetTimestamp(start.Add(50*time.Minute)).SetStartTime(start).SetSport(typedef.SportRunning).
		SetSubSport(typedef.SubSportTrail).
		SetTotalElapsedTimeScaled(3000).SetTotalTimerTimeScaled(2980).SetTotalDistanceScaled(10000).
		SetTotalAscent(120).SetAvgHeartRate(145).SetMaxHeartRate(165).SetAvgCadence(88).
		SetFirstLapIndex(0).SetNumLaps(2))
	act.Records = append(act.Records, mesgdef.NewRecord(nil).SetTimestamp(start).SetHeartRate(100))
	fit := act.ToFIT(nil)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFIT(t *testing.T) {
	acts, format, err := Parse("Run_2026-11-03.fit", corosFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	if format != "fit" || len(acts) != 1 {
		t.Fatalf("format %s, %d activities", format, len(acts))
	}
	a := acts[0]
	if a.Sport != "run" || a.SubSport != "trail" || a.Device != "COROS PACE 3" {
		t.Errorf("sport/device: %q %q %q", a.Sport, a.SubSport, a.Device)
	}
	if !a.Start.Equal(start) || a.Elapsed != 3000 || a.Moving != 2980 || a.DistanceM != 10000 || a.AscentM != 120 {
		t.Errorf("totals: %+v", a)
	}
	if a.AvgHR != 145 || a.MaxHR != 165 || a.AvgCadence != 176 {
		t.Errorf("hr/cadence: %d %d %d", a.AvgHR, a.MaxHR, a.AvgCadence)
	}
	if len(a.Laps) != 2 || a.Laps[1].AvgHR != 150 || a.Laps[1].DistanceM != 5000 {
		t.Errorf("laps: %+v", a.Laps)
	}
	if a.Title(time.FixedZone("PST", -8*3600)) != "Morning Run" {
		t.Errorf("title %q", a.Title(time.FixedZone("PST", -8*3600)))
	}
}

const gpxDoc1 = `<?xml version="1.0" encoding="UTF-8"?>
<gpx creator="COROS PACE 3" version="1.1" xmlns="http://www.topografix.com/GPX/1/1"
  xmlns:gpxtpx="http://www.garmin.com/xmlschemas/TrackPointExtension/v1">
 <trk><name>Evening Run</name><type>running</type><trkseg>
  <trkpt lat="48.6500" lon="-123.4000"><ele>10</ele><time>2026-11-03T01:00:00Z</time>
    <extensions><gpxtpx:TrackPointExtension><gpxtpx:hr>130</gpxtpx:hr></gpxtpx:TrackPointExtension></extensions></trkpt>
  <trkpt lat="48.6509" lon="-123.4000"><ele>15</ele><time>2026-11-03T01:00:30Z</time>
    <extensions><gpxtpx:TrackPointExtension><gpxtpx:hr>150</gpxtpx:hr></gpxtpx:TrackPointExtension></extensions></trkpt>
  <trkpt lat="48.6518" lon="-123.4000"><ele>15.4</ele><time>2026-11-03T01:01:00Z</time></trkpt>
 </trkseg></trk>
</gpx>`

func TestGPX(t *testing.T) {
	acts, format, err := Parse("evening.gpx", []byte(gpxDoc1))
	if err != nil {
		t.Fatal(err)
	}
	a := acts[0]
	if format != "gpx" || a.Sport != "run" || a.Device != "COROS PACE 3" {
		t.Errorf("%s %s %q", format, a.Sport, a.Device)
	}
	if math.Abs(a.DistanceM-200.2) > 1 || a.Elapsed != 60 || a.Moving != 60 || a.AscentM != 5 || a.AvgHR != 140 || a.MaxHR != 150 {
		t.Errorf("summary %+v", a)
	}
}

const tcxDoc1 = `<?xml version="1.0"?>
<TrainingCenterDatabase xmlns="http://www.garmin.com/xmlschemas/TrainingCenterDatabase/v2">
 <Activities><Activity Sport="Running"><Id>2026-11-02T15:00:00Z</Id>
  <Lap StartTime="2026-11-02T15:00:00Z"><TotalTimeSeconds>300</TotalTimeSeconds><DistanceMeters>1000</DistanceMeters>
   <Calories>70</Calories><AverageHeartRateBpm><Value>150</Value></AverageHeartRateBpm><MaximumHeartRateBpm><Value>160</Value></MaximumHeartRateBpm></Lap>
  <Lap StartTime="2026-11-02T15:05:00Z"><TotalTimeSeconds>290</TotalTimeSeconds><DistanceMeters>1000</DistanceMeters>
   <Calories>72</Calories><AverageHeartRateBpm><Value>160</Value></AverageHeartRateBpm><MaximumHeartRateBpm><Value>171</Value></MaximumHeartRateBpm></Lap>
  <Creator><Name>Forerunner 965</Name></Creator>
 </Activity></Activities>
</TrainingCenterDatabase>`

func TestTCX(t *testing.T) {
	acts, _, err := Parse("intervals.tcx", []byte(tcxDoc1))
	if err != nil {
		t.Fatal(err)
	}
	a := acts[0]
	if a.Sport != "run" || a.DistanceM != 2000 || a.Elapsed != 590 || a.MaxHR != 171 || a.Calories != 142 || a.Device != "Forerunner 965" {
		t.Errorf("%+v", a)
	}
	if a.AvgHR != 155 { // time-weighted 150*300+160*290 / 590
		t.Errorf("avg hr %d", a.AvgHR)
	}
}

func TestCSVTimingSplits(t *testing.T) {
	doc := "Lap,Date,Distance,Split,HR,Device\n1,2026-11-01 09:00:00,0.4,1:20.5,150,Track Timer\n2,2026-11-01 09:01:20,0.4,1:19.5,158,Track Timer\n"
	acts, format, err := Parse("splits.csv", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a := acts[0]
	if format != "csv" || len(a.Laps) != 2 || a.DistanceM != 800 || a.Elapsed != 160 || a.Device != "Track Timer" {
		t.Errorf("%+v", a)
	}
	if _, _, err := Parse("no-date.csv", []byte("distance,time\n5,25:00\n")); err == nil || !strings.Contains(err.Error(), "date") {
		t.Errorf("expected date-required error, got %v", err)
	}
	acts, _, err = Parse("race.csv", []byte("date,distance_m,time\n2026-10-12,5000,18:30\n"))
	if err != nil || acts[0].Elapsed != 1110 || acts[0].DistanceM != 5000 || len(acts[0].Laps) != 0 {
		t.Errorf("single-row csv: %v %+v", err, acts)
	}
}

func TestUnsupported(t *testing.T) {
	if _, _, err := Parse("photo.jpg", []byte{0xff, 0xd8, 0xff}); err == nil || !strings.Contains(err.Error(), "supported: csv, fit, gpx, tcx") {
		t.Errorf("err %v", err)
	}
}
