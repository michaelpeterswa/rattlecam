package wx

import (
	"math"
	"testing"
	"time"
)

func TestCardinalBoundaries(t *testing.T) {
	// Each sector is centred on its label, so N spans 348.75° through 11.25°
	// and wraps. The wrap is where an off-by-one sector shows up.
	for _, tc := range []struct {
		deg  float64
		want string
	}{
		{0, "N"},
		{11.24, "N"},
		{11.26, "NNE"},
		{348.75, "N"},
		{348.74, "NNW"},
		{360, "N"},
		{22.5, "NNE"},
		{45, "NE"},
		{90, "E"},
		{180, "S"},
		{270, "W"},
		{225, "SW"},
		{292, "WNW"},
		{158, "SSE"},
		{-22.5, "NNW"}, // normalised, not rejected
		{720, "N"},
	} {
		if got := Cardinal(tc.deg); got != tc.want {
			t.Errorf("Cardinal(%v) = %q, want %q", tc.deg, got, tc.want)
		}
	}
}

func TestCardinalCoversEveryPoint(t *testing.T) {
	seen := map[string]bool{}
	for deg := 0.0; deg < 360; deg += 0.25 {
		c := Cardinal(deg)
		if c == "" {
			t.Fatalf("Cardinal(%v) returned empty", deg)
		}
		seen[c] = true
	}
	if len(seen) != 16 {
		t.Errorf("swept the compass and saw %d distinct points, want 16", len(seen))
	}
}

func TestCardinalNonFinite(t *testing.T) {
	if got := Cardinal(math.NaN()); got != "" {
		t.Errorf("Cardinal(NaN) = %q, want empty", got)
	}
	if got := Cardinal(math.Inf(1)); got != "" {
		t.Errorf("Cardinal(+Inf) = %q, want empty", got)
	}
}

func TestPressureInHg(t *testing.T) {
	const tol = 0.02

	// At sea level the reduction is a no-op, so this is a pure unit conversion.
	r := &Reading{Fields: map[string]float64{"p": 1014.2}}
	got, ok := r.PressureInHg(0)
	if !ok {
		t.Fatal("pressure reported absent")
	}
	if want := 29.949; math.Abs(got-want) > tol {
		t.Errorf("PressureInHg(0) = %.4f, want %.4f", got, want)
	}

	// 1013.25 mb is 29.9213 inHg by definition.
	r = &Reading{Fields: map[string]float64{"p": 1013.25}}
	got, _ = r.PressureInHg(0)
	if want := 29.9213; math.Abs(got-want) > tol {
		t.Errorf("standard atmosphere = %.4f inHg, want %.4f", got, want)
	}

	// A mountain station: 850 mb of station pressure at 1500 m reduces to about
	// 1018 mb, i.e. an unremarkable sea-level reading. Publishing the raw 850 mb
	// would show 25.1 inHg, which is not a number any forecast would print.
	r = &Reading{Fields: map[string]float64{"p": 850}}
	got, _ = r.PressureInHg(1500)
	if want := 30.06; math.Abs(got-want) > 0.05 {
		t.Errorf("PressureInHg(1500) = %.4f, want ~%.4f", got, want)
	}
	if raw := MBToInHg(850); math.Abs(got-raw) < 1 {
		t.Errorf("reduction barely moved the value: %.4f vs raw %.4f", got, raw)
	}
}

func TestPressureAltimeterMonotonicInElevation(t *testing.T) {
	prev := PressureAltimeter(950, 0)
	for h := 100.0; h <= 3000; h += 100 {
		cur := PressureAltimeter(950, h)
		if cur <= prev {
			t.Fatalf("altimeter setting did not increase at %v m: %.3f then %.3f", h, prev, cur)
		}
		prev = cur
	}
}

func TestPressureAbsent(t *testing.T) {
	r := &Reading{Fields: map[string]float64{"temp": 10}}
	if _, ok := r.PressureInHg(0); ok {
		t.Error("pressure reported present with no p field")
	}
}

func TestConversions(t *testing.T) {
	const tol = 0.01

	for _, tc := range []struct{ c, f float64 }{
		{0, 32}, {100, 212}, {-40, -40}, {18.3, 64.94}, {-17.8, -0.04},
	} {
		if got := CToF(tc.c); math.Abs(got-tc.f) > tol {
			t.Errorf("CToF(%v) = %v, want %v", tc.c, got, tc.f)
		}
	}

	// The README quotes the wide-values scenario as gusting to 76 mph; that
	// comes from 33.9 m/s, so this pins the unit the scenarios are written in.
	if got := MSToMPH(33.9); math.Abs(got-75.83) > 0.05 {
		t.Errorf("MSToMPH(33.9) = %v, want ~75.83", got)
	}
	if got := MSToMPH(0); got != 0 {
		t.Errorf("MSToMPH(0) = %v, want 0", got)
	}
}

// A missing sensor and a zero reading have to stay distinguishable: 0°C and
// 0 mph are both real observations, and a dropped column is not the same thing
// as a column showing zero.
func TestAbsentVersusZero(t *testing.T) {
	zero := &Reading{Fields: map[string]float64{"temp": 0, "wind_avg": 0, "humidity": 0}}
	if v, ok := zero.TempF(); !ok || v != 32 {
		t.Errorf("TempF() = %v, %v; want 32, true", v, ok)
	}
	if v, ok := zero.WindMPH(); !ok || v != 0 {
		t.Errorf("WindMPH() = %v, %v; want 0, true", v, ok)
	}

	empty := &Reading{Fields: map[string]float64{}}
	if _, ok := empty.TempF(); ok {
		t.Error("TempF present on an empty reading")
	}
	if _, ok := empty.WindMPH(); ok {
		t.Error("WindMPH present on an empty reading")
	}

	var nilReading *Reading
	if _, ok := nilReading.TempF(); ok {
		t.Error("TempF present on a nil reading")
	}
}

func TestNonFiniteFieldsAreAbsent(t *testing.T) {
	r := &Reading{Fields: map[string]float64{"temp": math.NaN(), "humidity": math.Inf(1)}}
	if _, ok := r.TempF(); ok {
		t.Error("NaN temp reported present")
	}
	if _, ok := r.HumidityPct(); ok {
		t.Error("Inf humidity reported present")
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r := &Reading{ObservedAt: now.Add(-45 * time.Minute)}
	if got := r.Age(now); got != 45*time.Minute {
		t.Errorf("Age = %v, want 45m", got)
	}
}
