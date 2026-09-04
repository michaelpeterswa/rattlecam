package wx

import (
	"strings"
	"testing"
	"time"
)

const lightningCSV = `#datatype,string,long,dateTime:RFC3339,dateTime:RFC3339,dateTime:RFC3339,double,string,string,string
#group,false,false,true,true,false,false,true,true,true
#default,_result,,,,,,,,
,result,table,_start,_stop,_time,_value,_field,_measurement,station
,,0,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:10:00Z,0,strike_count,weather,ST-1
,,0,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:11:00Z,2,strike_count,weather,ST-1
,,0,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:12:00Z,0,strike_count,weather,ST-1
,,0,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:40:00Z,3,strike_count,weather,ST-1

#datatype,string,long,dateTime:RFC3339,dateTime:RFC3339,dateTime:RFC3339,double,string,string,string
#group,false,false,true,true,false,false,true,true,true
#default,_result,,,,,,,,
,result,table,_start,_stop,_time,_value,_field,_measurement,station
,,1,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:10:00Z,0,strike_distance,weather,ST-1
,,1,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:11:00Z,24,strike_distance,weather,ST-1
,,1,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:12:00Z,0,strike_distance,weather,ST-1
,,1,2026-09-04T20:00:00Z,2026-09-04T21:00:00Z,2026-09-04T20:40:00Z,10,strike_distance,weather,ST-1
`

func TestSummariseLightning(t *testing.T) {
	rows, err := parseRows(strings.NewReader(lightningCSV))
	if err != nil {
		t.Fatalf("parseRows: %v", err)
	}
	l := summariseLightning(rows, time.Hour)

	if l.Strikes != 5 {
		t.Errorf("Strikes = %d, want 5", l.Strikes)
	}
	want := time.Date(2026, 9, 4, 20, 40, 0, 0, time.UTC)
	if !l.LastStrike.Equal(want) {
		t.Errorf("LastStrike = %s, want %s", l.LastStrike, want)
	}
	// The distance is the one recorded with the newest strike, not the first.
	if !l.HasDist || l.DistanceKM != 10 {
		t.Errorf("DistanceKM = %v (has=%v), want 10", l.DistanceKM, l.HasDist)
	}
	if mi := l.DistanceMiles(); mi < 6.2 || mi > 6.22 {
		t.Errorf("DistanceMiles = %v, want ~6.21", mi)
	}
}

func TestSummariseLightningQuietHour(t *testing.T) {
	l := summariseLightning(nil, time.Hour)
	if l.Strikes != 0 || !l.LastStrike.IsZero() || l.HasDist {
		t.Errorf("quiet hour = %+v, want empty", l)
	}
}

func TestLightningQueryShape(t *testing.T) {
	s := NewInfluxSource("http://x", "org", "tok", "weather", "ST-1", 10*time.Minute)
	q := s.lightningQuery(time.Hour)
	for _, want := range []string{
		`range(start: -3600s)`,
		`r._measurement == "weather"`,
		`r.station == "ST-1"`,
		`r._field == "strike_count" or r._field == "strike_distance"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "last()") {
		t.Error("lightning query must return every row, not last()")
	}
}
