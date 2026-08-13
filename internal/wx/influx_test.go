package wx

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A response shaped the way Flux actually returns one: three annotation lines,
// a header row, a leading empty column on every data row, and one table per
// field because last() groups that way.
const fluxResponse = `#datatype,string,long,dateTime:RFC3339,dateTime:RFC3339,dateTime:RFC3339,double,string,string,string
#group,false,false,true,true,false,false,true,true,true
#default,_result,,,,,,,,
,result,table,_start,_stop,_time,_value,_field,_measurement,station
,,0,2026-08-07T17:59:00Z,2026-08-07T18:09:00Z,2026-08-07T18:08:47Z,18.3,temp,weather,ST-00000512
,,1,2026-08-07T17:59:00Z,2026-08-07T18:09:00Z,2026-08-07T18:08:47Z,62,humidity,weather,ST-00000512
,,2,2026-08-07T17:59:00Z,2026-08-07T18:09:00Z,2026-08-07T18:08:47Z,1014.2,p,weather,ST-00000512
,,3,2026-08-07T17:59:00Z,2026-08-07T18:09:00Z,2026-08-07T18:08:47Z,3.1,wind_avg,weather,ST-00000512
,,4,2026-08-07T17:59:00Z,2026-08-07T18:09:00Z,2026-08-07T18:08:47Z,0,strike_count,weather,ST-00000512

`

func TestParseAnnotatedCSV(t *testing.T) {
	r, err := parseAnnotatedCSV(strings.NewReader(fluxResponse))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := map[string]float64{
		"temp": 18.3, "humidity": 62, "p": 1014.2, "wind_avg": 3.1, "strike_count": 0,
	}
	if len(r.Fields) != len(want) {
		t.Errorf("got %d fields %v, want %d", len(r.Fields), r.Fields, len(want))
	}
	for k, v := range want {
		got, ok := r.Fields[k]
		if !ok {
			t.Errorf("field %q missing", k)
			continue
		}
		if got != v {
			t.Errorf("field %q = %v, want %v", k, got, v)
		}
	}

	wantTime := time.Date(2026, 8, 7, 18, 8, 47, 0, time.UTC)
	if !r.ObservedAt.Equal(wantTime) {
		t.Errorf("ObservedAt = %s, want %s", r.ObservedAt, wantTime)
	}
}

// strike_count and precipitation_type are written as bare numbers, so they
// arrive as floats even though they read like counts. Decoding them as ints
// would be wrong; this pins that they survive as float64.
func TestParseAnnotatedCSVCountsAreFloats(t *testing.T) {
	const resp = `#datatype,string,long,dateTime:RFC3339,double,string
#group,false,false,false,false,true
#default,_result,,,,
,result,table,_time,_value,_field
,,0,2026-08-07T18:08:47Z,47,strike_count
,,1,2026-08-07T18:08:47Z,1,precipitation_type
`
	r, err := parseAnnotatedCSV(strings.NewReader(resp))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, ok := r.Raw("strike_count"); !ok || v != 47 {
		t.Errorf("strike_count = %v (ok=%v), want 47", v, ok)
	}
	if v, ok := r.Raw("precipitation_type"); !ok || v != 1 {
		t.Errorf("precipitation_type = %v (ok=%v), want 1", v, ok)
	}
}

// Several results in one response, where the second restates its own header
// with the columns in a different order. Positional parsing passes the
// single-table case and silently corrupts this one.
func TestParseAnnotatedCSVMultipleTablesDifferentColumnOrder(t *testing.T) {
	const resp = `#datatype,string,long,dateTime:RFC3339,double,string
#group,false,false,false,false,true
#default,_result,,,,
,result,table,_time,_value,_field
,,0,2026-08-07T18:08:47Z,18.3,temp

#datatype,string,long,string,double,dateTime:RFC3339
#group,false,false,true,false,false
#default,_result,,,,
,result,table,_field,_value,_time
,,1,wind_gust,4.4,2026-08-07T18:09:03Z
,,2,wind_direction,225,2026-08-07T18:09:03Z
`
	r, err := parseAnnotatedCSV(strings.NewReader(resp))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for k, want := range map[string]float64{"temp": 18.3, "wind_gust": 4.4, "wind_direction": 225} {
		if got, ok := r.Raw(k); !ok || got != want {
			t.Errorf("field %q = %v (ok=%v), want %v", k, got, ok, want)
		}
	}
	// The newest _time across all tables wins.
	wantTime := time.Date(2026, 8, 7, 18, 9, 3, 0, time.UTC)
	if !r.ObservedAt.Equal(wantTime) {
		t.Errorf("ObservedAt = %s, want %s", r.ObservedAt, wantTime)
	}
}

// A silent station returns a well-formed response with no rows. That is the
// mechanism the whole staleness story rests on, so it must be ErrNoData and not
// a parse failure or an empty Reading.
func TestParseAnnotatedCSVNoRowsIsErrNoData(t *testing.T) {
	const resp = `#datatype,string,long,dateTime:RFC3339,double,string
#group,false,false,false,false,true
#default,_result,,,,
,result,table,_time,_value,_field
`
	_, err := parseAnnotatedCSV(strings.NewReader(resp))
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
}

func TestParseAnnotatedCSVEmptyBodyIsErrNoData(t *testing.T) {
	if _, err := parseAnnotatedCSV(strings.NewReader("")); !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
}

// Flux reports a bad query as a 200 with an error table, which must not be
// mistaken for "the station is quiet".
func TestParseAnnotatedCSVQueryError(t *testing.T) {
	const resp = `#datatype,string,string
#group,true,true
#default,,
,error,reference
,"compilation failed: undefined identifier",897
`
	_, err := parseAnnotatedCSV(strings.NewReader(resp))
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrNoData) {
		t.Fatalf("query error reported as ErrNoData: %v", err)
	}
	if !strings.Contains(err.Error(), "compilation failed") {
		t.Errorf("error %q does not carry the Flux message", err)
	}
}

func TestQueryShape(t *testing.T) {
	s := NewInfluxSource("http://localhost:8086/", "org", "tok", "weather", "ST-00000512", 10*time.Minute)
	q := s.query()

	for _, want := range []string{
		`from(bucket: "weather")`,
		`range(start: -600s)`,
		`r._measurement == "weather"`,
		`r.station == "ST-00000512"`,
		`contains(value: r._field`,
		`|> last()`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}

	// The allowlist is what keeps rapid_wind_* out when RAPID_WIND is enabled
	// without a separate bucket.
	if strings.Contains(q, "rapid_wind") {
		t.Errorf("query should not reference rapid_wind:\n%s", q)
	}
	for _, f := range fields {
		if !strings.Contains(q, `"`+f+`"`) {
			t.Errorf("allowlisted field %q missing from query", f)
		}
	}
}

func TestQueryOmitsStationFilterWhenUnset(t *testing.T) {
	s := NewInfluxSource("http://localhost:8086", "org", "tok", "weather", "", time.Minute)
	if q := s.query(); strings.Contains(q, "r.station") {
		t.Errorf("station filter present despite empty station:\n%s", q)
	}
}

// Go renders a minute as "1m0s", which Flux's duration grammar rejects.
func TestFluxDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "600s"},
		{time.Minute, "60s"},
		{90 * time.Second, "90s"},
		{1500 * time.Millisecond, "1s"},
		{0, "1s"},
	} {
		if got := fluxDuration(tc.in); got != tc.want {
			t.Errorf("fluxDuration(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestFluxStringEscapes(t *testing.T) {
	if got, want := fluxString(`we"ird\`), `"we\"ird\\"`; got != want {
		t.Errorf("fluxString = %s, want %s", got, want)
	}
}
