package frame

import (
	"strings"
	"testing"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/aqi"
	"github.com/michaelpeterswa/rattlecam/internal/overlay"
	"github.com/michaelpeterswa/rattlecam/internal/wx"
)

func params() Params {
	return Params{
		SiteName:   "Test Site",
		Elevation:  0,
		StaleAfter: 10 * time.Minute,
		Location:   time.UTC,
		MaxFields:  6,

		AirStaleAfter:   30 * time.Minute,
		LightningWindow: time.Hour,
	}
}

func reading(age time.Duration, now time.Time) *wx.Reading {
	return &wx.Reading{
		ObservedAt: now.Add(-age),
		Fields: map[string]float64{
			"temp": 18.3, "humidity": 62, "dew_point": 10.9, "p": 1014.2,
			"wind_avg": 3.1, "wind_gust": 4.4, "wind_direction": 225,
		},
	}
}

func labels(f overlay.Frame) []string {
	out := make([]string, len(f.Fields))
	for i, fld := range f.Fields {
		out[i] = fld.Label
	}
	return out
}

func value(t *testing.T, f overlay.Frame, label string) string {
	t.Helper()
	for _, fld := range f.Fields {
		if fld.Label == label {
			return fld.Value
		}
	}
	t.Fatalf("no field labelled %q in %v", label, labels(f))
	return ""
}

// The load-bearing one. An omitted field is recoverable; a wrong temperature on
// the evening news is not.
func TestStaleReadingYieldsZeroFields(t *testing.T) {
	now := time.Now()
	f := Build(params(), reading(45*time.Minute, now), "Overcast", nil, nil, now)

	if len(f.Fields) != 0 {
		t.Errorf("stale reading produced %d fields (%v), want 0", len(f.Fields), labels(f))
	}
	// Everything that isn't sensor data still publishes.
	if f.SiteName != "Test Site" {
		t.Errorf("SiteName = %q, want %q", f.SiteName, "Test Site")
	}
	if f.Conditions != "Overcast" {
		t.Errorf("Conditions = %q, want %q", f.Conditions, "Overcast")
	}
	if f.CapturedAt.IsZero() {
		t.Error("CapturedAt is zero")
	}
}

func TestNilReadingYieldsZeroFields(t *testing.T) {
	now := time.Now()
	f := Build(params(), nil, "Light Rain", nil, nil, now)

	if len(f.Fields) != 0 {
		t.Errorf("offline station produced %d fields (%v), want 0", len(f.Fields), labels(f))
	}
	if f.Conditions != "Light Rain" {
		t.Errorf("Conditions = %q, want %q", f.Conditions, "Light Rain")
	}
}

// The threshold itself: one second inside publishes, one second outside does not.
func TestStalenessBoundary(t *testing.T) {
	now := time.Now()
	p := params()

	fresh := Build(p, reading(p.StaleAfter-time.Second, now), "", nil, nil, now)
	if len(fresh.Fields) == 0 {
		t.Error("reading just inside the window produced no fields")
	}

	stale := Build(p, reading(p.StaleAfter+time.Second, now), "", nil, nil, now)
	if len(stale.Fields) != 0 {
		t.Errorf("reading just outside the window produced %d fields", len(stale.Fields))
	}
}

// StaleAfter of zero disables the gate rather than dropping everything.
func TestZeroStaleAfterDisablesGate(t *testing.T) {
	now := time.Now()
	p := params()
	p.StaleAfter = 0

	f := Build(p, reading(365*24*time.Hour, now), "", nil, nil, now)
	if len(f.Fields) == 0 {
		t.Error("StaleAfter=0 should not gate; got no fields")
	}
}

func TestTypicalFields(t *testing.T) {
	now := time.Now()
	f := Build(params(), reading(time.Minute, now), "Partly Cloudy", nil, nil, now)

	want := []string{"TEMP", "WIND", "HUMIDITY", "DEW POINT", "PRESSURE"}
	got := labels(f)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v", got, want)
	}

	if v := value(t, f, "TEMP"); v != "65°F" {
		t.Errorf("TEMP = %q, want %q", v, "65°F")
	}
	if v := value(t, f, "HUMIDITY"); v != "62%" {
		t.Errorf("HUMIDITY = %q, want %q", v, "62%")
	}
	if v := value(t, f, "PRESSURE"); v != "29.95 in" {
		t.Errorf("PRESSURE = %q, want %q", v, "29.95 in")
	}
}

// The gust suffix is only worth the horizontal space when it is meaningfully
// above the sustained wind.
func TestGustSuffixThreshold(t *testing.T) {
	now := time.Now()

	calm := &wx.Reading{ObservedAt: now, Fields: map[string]float64{
		"wind_avg": 0.4, "wind_gust": 0.6, "wind_direction": 0,
	}}
	if v := value(t, Build(params(), calm, "", nil, nil, now), "WIND"); strings.Contains(v, "G") {
		t.Errorf("calm wind carried a gust suffix: %q", v)
	}

	gusty := &wx.Reading{ObservedAt: now, Fields: map[string]float64{
		"wind_avg": 8.9, "wind_gust": 15.2, "wind_direction": 158,
	}}
	v := value(t, Build(params(), gusty, "", nil, nil, now), "WIND")
	if !strings.Contains(v, "G") {
		t.Errorf("gusty wind lost its gust suffix: %q", v)
	}
	if !strings.HasPrefix(v, "SSE ") {
		t.Errorf("WIND = %q, want it to lead with the cardinal direction", v)
	}
}

// Wind with no direction reported still renders, just without the cardinal.
func TestWindWithoutDirection(t *testing.T) {
	now := time.Now()
	r := &wx.Reading{ObservedAt: now, Fields: map[string]float64{"wind_avg": 3.1}}
	if v := value(t, Build(params(), r, "", nil, nil, now), "WIND"); v != "7 mph" {
		t.Errorf("WIND = %q, want %q", v, "7 mph")
	}
}

// Sensor dropout: columns reflow to whatever is present, with no placeholders.
func TestPartialReflows(t *testing.T) {
	now := time.Now()
	r := &wx.Reading{ObservedAt: now, Fields: map[string]float64{"temp": 11.0, "humidity": 77}}

	f := Build(params(), r, "Mostly Cloudy", nil, nil, now)
	if got, want := labels(f), []string{"TEMP", "HUMIDITY"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v", got, want)
	}
	for _, fld := range f.Fields {
		if fld.Value == "" {
			t.Errorf("field %q rendered an empty value", fld.Label)
		}
	}
}

func TestMaxFieldsCaps(t *testing.T) {
	now := time.Now()
	p := params()
	p.MaxFields = 3

	f := Build(p, reading(time.Minute, now), "", nil, nil, now)
	if len(f.Fields) != 3 {
		t.Errorf("got %d fields, want 3", len(f.Fields))
	}
}

func TestNilLocationFallsBackToLocal(t *testing.T) {
	now := time.Now()
	p := params()
	p.Location = nil

	if f := Build(p, nil, "", nil, nil, now); f.CapturedAt.IsZero() {
		t.Error("CapturedAt is zero with a nil location")
	}
}

// The harness and the daemon share this function; if they diverged, the preview
// would stop being a valid preview. Every built-in scenario therefore has to go
// through it and produce what the scenario table promises.
func TestScenariosMatchDocumentedBehaviour(t *testing.T) {
	now := time.Now()
	p := params()

	wantFields := map[string]int{
		"typical": 6, "wide-values": 6, "calm": 6, "night": 6,
		"partial": 2, "stale": 0, "offline": 0, "no-conditions": 6,
		"smoke": 6, "stale-air": 5, "lightning": 6,
	}

	for _, s := range wx.Scenarios {
		want, ok := wantFields[s.Name]
		if !ok {
			t.Errorf("scenario %q is not covered by this test", s.Name)
			continue
		}
		f := Build(p, s.Reading(now), s.Conditions, s.AirReading(now), s.LightningReading(now), now)
		if len(f.Fields) != want {
			t.Errorf("scenario %q produced %d fields (%v), want %d",
				s.Name, len(f.Fields), labels(f), want)
		}
	}

	if len(wx.Scenarios) != len(wantFields) {
		t.Errorf("%d scenarios defined, %d covered", len(wx.Scenarios), len(wantFields))
	}
}

func air(age time.Duration, now time.Time) *aqi.Reading {
	return &aqi.Reading{ObservedAt: now.Add(-age), AQI: 42, Level: "Good", PM25: 9.4, HasPM25: true}
}

// The index slots in after wind so it outranks dew point and pressure when
// columns get dropped.
func TestAirQualitySitsAfterWind(t *testing.T) {
	now := time.Now()
	f := Build(params(), reading(time.Minute, now), "Clear", air(time.Minute, now), nil, now)

	want := []string{"TEMP", "WIND", "AIR QUALITY", "HUMIDITY", "DEW POINT", "PRESSURE"}
	if got := labels(f); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if v := value(t, f, "AIR QUALITY"); v != "42 Good" {
		t.Errorf("AIR QUALITY = %q, want %q", v, "42 Good")
	}
}

func TestAirQualityLevelIsShortened(t *testing.T) {
	now := time.Now()
	a := air(time.Minute, now)
	a.AQI, a.Level = 150, "Unhealthy for Sensitive Groups"
	f := Build(params(), reading(time.Minute, now), "", a, nil, now)
	if v := value(t, f, "AIR QUALITY"); v != "150 Sensitive" {
		t.Errorf("AIR QUALITY = %q, want %q", v, "150 Sensitive")
	}
}

// The two sources are gated independently: a quiet air feed drops only its
// own field, and a stale station still lets a fresh index through.
func TestAirQualityGatedOnItsOwn(t *testing.T) {
	now := time.Now()

	f := Build(params(), reading(time.Minute, now), "", air(45*time.Minute, now), nil, now)
	if len(f.Fields) != 5 {
		t.Errorf("stale air: got %v, want the 5 station fields only", labels(f))
	}

	f = Build(params(), reading(45*time.Minute, now), "", air(time.Minute, now), nil, now)
	if got := labels(f); strings.Join(got, ",") != "AIR QUALITY" {
		t.Errorf("stale station, fresh air: labels = %v, want [AIR QUALITY]", got)
	}

	p := params()
	p.AirStaleAfter = 0
	f = Build(p, nil, "", air(48*time.Hour, now), nil, now)
	if len(f.Fields) != 1 {
		t.Errorf("zero AirStaleAfter should not gate; got %v", labels(f))
	}
}

// With a partial station reading there is no wind to sit behind, so the index
// takes the earliest slot it can.
func TestAirQualityWithPartialStation(t *testing.T) {
	now := time.Now()
	r := &wx.Reading{ObservedAt: now, Fields: map[string]float64{"temp": 11.0}}
	f := Build(params(), r, "", air(time.Minute, now), nil, now)
	if got := labels(f); strings.Join(got, ",") != "TEMP,AIR QUALITY" {
		t.Errorf("labels = %v, want [TEMP AIR QUALITY]", got)
	}
}

func strikes(count int, age time.Duration, km float64, now time.Time) *wx.Lightning {
	return &wx.Lightning{Window: time.Hour, Strikes: count, LastStrike: now.Add(-age), DistanceKM: km, HasDist: km > 0}
}

func TestLightningWarningText(t *testing.T) {
	now := time.Now()
	f := Build(params(), reading(time.Minute, now), "", nil, strikes(5, 3*time.Minute, 9, now), now)
	if want := "LIGHTNING · 5 strikes in the last hour · last 3 min ago · ~6 mi"; f.Warning != want {
		t.Errorf("Warning = %q, want %q", f.Warning, want)
	}

	f = Build(params(), nil, "", nil, strikes(1, 20*time.Second, 0, now), now)
	if want := "LIGHTNING · 1 strike in the last hour · last just now"; f.Warning != want {
		t.Errorf("Warning = %q, want %q", f.Warning, want)
	}
}

// The warning is about now, so it must vanish on its own: past the window,
// with no strikes, with no summary, and when the window is switched off.
func TestLightningWarningGated(t *testing.T) {
	now := time.Now()
	r := reading(time.Minute, now)

	if f := Build(params(), r, "", nil, strikes(3, 61*time.Minute, 5, now), now); f.Warning != "" {
		t.Errorf("strike past the window still warned: %q", f.Warning)
	}
	if f := Build(params(), r, "", nil, strikes(0, 0, 0, now), now); f.Warning != "" {
		t.Errorf("quiet hour warned: %q", f.Warning)
	}
	if f := Build(params(), r, "", nil, nil, now); f.Warning != "" {
		t.Errorf("missing summary warned: %q", f.Warning)
	}
	p := params()
	p.LightningWindow = 0
	if f := Build(p, r, "", nil, strikes(3, time.Minute, 5, now), now); f.Warning != "" {
		t.Errorf("disabled window warned: %q", f.Warning)
	}
	// The warning does not depend on the station reading being fresh.
	if f := Build(params(), reading(45*time.Minute, now), "", nil, strikes(3, time.Minute, 5, now), now); f.Warning == "" {
		t.Error("stale station reading suppressed the warning")
	}
}
