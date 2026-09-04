// Package frame turns a weather reading into the strings the overlay draws.
//
// This lives apart from both the daemon and the preview harness on purpose:
// whatever you tune in the harness is exactly what ships, including which
// fields get dropped when data goes stale.
package frame

import (
	"fmt"
	"math"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/aqi"
	"github.com/michaelpeterswa/rattlecam/internal/overlay"
	"github.com/michaelpeterswa/rattlecam/internal/wx"
)

type Params struct {
	SiteName   string
	Credit     string  // standing attribution, e.g. "Camera brought to you by RSVU"
	Elevation  float64 // meters, for the pressure reduction
	StaleAfter time.Duration
	Location   *time.Location
	MaxFields  int

	// AirStaleAfter bounds the air quality reading separately: it comes from
	// a different source on a five minute publish cycle, and a quiet feed
	// should drop its own field without taking the station's down. Zero
	// means no limit.
	AirStaleAfter time.Duration
}

// Build assembles the frame. Any reading past its staleness threshold
// contributes nothing: an omitted field is recoverable, a wrong one on the
// evening news is not. The station reading and the air quality reading are
// gated independently, so one going quiet does not blank the other.
func Build(p Params, r *wx.Reading, conditions string, air *aqi.Reading, capturedAt time.Time) overlay.Frame {
	loc := p.Location
	if loc == nil {
		loc = time.Local
	}

	f := overlay.Frame{
		SiteName:   p.SiteName,
		Credit:     p.Credit,
		Conditions: conditions,
		CapturedAt: capturedAt.In(loc),
	}

	f.Fields = stationFields(p, r, capturedAt)

	// The index sits after wind: on a smoke day it is the number people came
	// for, and it should survive the column drop ahead of dew point and
	// pressure. Level over concentration, because "Moderate" means something
	// to a viewer and "24 µg/m³" does not.
	if air != nil && (p.AirStaleAfter <= 0 || capturedAt.Sub(air.ObservedAt) <= p.AirStaleAfter) {
		fld := overlay.Field{Label: "AIR QUALITY", Value: fmt.Sprintf("%d %s", air.AQI, aqi.ShortLevel(air.Level))}
		at := min(len(f.Fields), 2)
		f.Fields = append(f.Fields[:at], append([]overlay.Field{fld}, f.Fields[at:]...)...)
	}

	if p.MaxFields > 0 && len(f.Fields) > p.MaxFields {
		f.Fields = f.Fields[:p.MaxFields]
	}
	return f
}

// stationFields formats the weather station's reading, or nothing if it is
// missing or stale.
func stationFields(p Params, r *wx.Reading, capturedAt time.Time) []overlay.Field {
	if r == nil || (p.StaleAfter > 0 && r.Age(capturedAt) > p.StaleAfter) {
		return nil
	}

	var fields []overlay.Field
	if v, ok := r.TempF(); ok {
		fields = append(fields, overlay.Field{Label: "TEMP", Value: fmt.Sprintf("%.0f°F", round0(v))})
	}

	// Wind reads as one composite value: direction, sustained, then gust —
	// and the gust is only worth the space when it's meaningfully higher.
	if spd, ok := r.WindMPH(); ok {
		val := fmt.Sprintf("%.0f mph", spd)
		if deg, ok := r.WindDirDeg(); ok {
			val = wx.Cardinal(deg) + " " + val
		}
		if gust, ok := r.GustMPH(); ok && gust >= spd+3 {
			val += fmt.Sprintf(" G%.0f", gust)
		}
		fields = append(fields, overlay.Field{Label: "WIND", Value: val})
	}

	if v, ok := r.HumidityPct(); ok {
		fields = append(fields, overlay.Field{Label: "HUMIDITY", Value: fmt.Sprintf("%.0f%%", v)})
	}
	if v, ok := r.DewPointF(); ok {
		fields = append(fields, overlay.Field{Label: "DEW POINT", Value: fmt.Sprintf("%.0f°F", round0(v))})
	}
	if v, ok := r.PressureInHg(p.Elevation); ok {
		fields = append(fields, overlay.Field{Label: "PRESSURE", Value: fmt.Sprintf("%.2f in", v)})
	}
	return fields
}

// round0 rounds to a whole number and collapses negative zero.
//
// A reading of -17.8°C converts to -0.04°F, which %.0f renders as "-0°F". It is
// arithmetically defensible and looks like a defect on screen.
func round0(v float64) float64 {
	r := math.Round(v)
	if r == 0 {
		return 0
	}
	return r
}
