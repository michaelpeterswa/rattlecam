// Package wx reads weather observations and converts them into the units the
// overlay displays.
//
// Every field arriving from Influx is a float64 in the station's native units:
// Celsius, metres per second, millibars. Nothing downstream should have to know
// that, so the conversions live here and the accessors report presence
// explicitly — a missing sensor is a dropped column, not a zero.
package wx

import (
	"math"
	"time"
)

// Reading is one observation: the time the station reported it, and whichever
// fields that packet happened to carry.
//
// ObservedAt is the observation time from the packet, not ingest time, which is
// what makes it usable as a freshness signal.
type Reading struct {
	ObservedAt time.Time
	Fields     map[string]float64
}

// Age reports how long before now the observation was taken.
func (r *Reading) Age(now time.Time) time.Duration {
	if r == nil {
		return 0
	}
	return now.Sub(r.ObservedAt)
}

// get returns a field and whether the station actually reported it. Absent and
// zero have to stay distinguishable: 0°C and 0 mph are both real readings.
func (r *Reading) get(name string) (float64, bool) {
	if r == nil || r.Fields == nil {
		return 0, false
	}
	v, ok := r.Fields[name]
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// Has reports whether a field is present.
func (r *Reading) Has(name string) bool {
	_, ok := r.get(name)
	return ok
}

// Raw returns a field in its native Influx units.
func (r *Reading) Raw(name string) (float64, bool) { return r.get(name) }

// TempF returns air temperature in Fahrenheit.
func (r *Reading) TempF() (float64, bool) {
	c, ok := r.get("temp")
	return CToF(c), ok
}

// DewPointF returns the dew point in Fahrenheit.
func (r *Reading) DewPointF() (float64, bool) {
	c, ok := r.get("dew_point")
	return CToF(c), ok
}

// HumidityPct returns relative humidity as a percentage.
func (r *Reading) HumidityPct() (float64, bool) { return r.get("humidity") }

// WindMPH returns the sustained wind speed in miles per hour.
func (r *Reading) WindMPH() (float64, bool) {
	ms, ok := r.get("wind_avg")
	return MSToMPH(ms), ok
}

// GustMPH returns the gust speed in miles per hour.
func (r *Reading) GustMPH() (float64, bool) {
	ms, ok := r.get("wind_gust")
	return MSToMPH(ms), ok
}

// LullMPH returns the wind lull in miles per hour.
func (r *Reading) LullMPH() (float64, bool) {
	ms, ok := r.get("wind_lull")
	return MSToMPH(ms), ok
}

// WindDirDeg returns the wind direction in degrees.
func (r *Reading) WindDirDeg() (float64, bool) { return r.get("wind_direction") }

// PressureInHg returns the altimeter setting in inches of mercury, reduced to
// sea level for a station at elevationM metres.
//
// The station reports `p` as station pressure. Publishing that raw shows a
// number well below what any local forecast says, so the reduction is not
// optional — see PressureAltimeter.
func (r *Reading) PressureInHg(elevationM float64) (float64, bool) {
	mb, ok := r.get("p")
	if !ok {
		return 0, false
	}
	return MBToInHg(PressureAltimeter(mb, elevationM)), true
}

// Unit conversions.

// CToF converts Celsius to Fahrenheit.
func CToF(c float64) float64 { return c*9/5 + 32 }

// MSToMPH converts metres per second to miles per hour.
func MSToMPH(ms float64) float64 { return ms * 2.236936292054402 }

// MBToInHg converts millibars (hectopascals) to inches of mercury.
func MBToInHg(mb float64) float64 { return mb / 33.8638815789 }

// PressureAltimeter reduces station pressure in millibars to the altimeter
// setting for a station at elevationM metres, also in millibars.
//
// This is the standard barometric reduction that weather services publish, so
// the number on the frame matches the number in the local forecast rather than
// sitting some tens of millibars below it. At sea level it is very nearly a
// no-op.
func PressureAltimeter(stationMB, elevationM float64) float64 {
	if elevationM == 0 || stationMB <= 0 {
		return stationMB
	}
	const (
		k     = 0.190284 // exponent from the standard atmosphere
		lapse = 0.0065   // K/m
		t0    = 288.15   // K at sea level
		p0    = 1013.25  // mb at sea level
	)
	p := stationMB - 0.3
	if p <= 0 {
		return stationMB
	}
	inner := 1 + math.Pow(p0, k)*lapse/t0*elevationM/math.Pow(p, k)
	return p * math.Pow(inner, 1/k)
}

var compass = [16]string{
	"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE",
	"S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW",
}

// Cardinal converts a bearing in degrees to a 16-point compass abbreviation.
//
// Each sector spans 22.5°, centred on its label, so N covers 348.75° through
// 11.25° and wraps. Out-of-range and negative bearings are normalised rather
// than rejected; a station occasionally reports 360.
func Cardinal(deg float64) string {
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return ""
	}
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	return compass[int(math.Floor(deg/22.5+0.5))%16]
}
