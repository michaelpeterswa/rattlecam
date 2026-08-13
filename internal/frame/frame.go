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
}

// Build assembles the frame. Any reading past the staleness threshold
// contributes nothing: an omitted field is recoverable, a wrong one on the
// evening news is not.
func Build(p Params, r *wx.Reading, conditions string, capturedAt time.Time) overlay.Frame {
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

	if r == nil || (p.StaleAfter > 0 && r.Age(capturedAt) > p.StaleAfter) {
		return f
	}

	if v, ok := r.TempF(); ok {
		f.Fields = append(f.Fields, overlay.Field{Label: "TEMP", Value: fmt.Sprintf("%.0f°F", round0(v))})
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
		f.Fields = append(f.Fields, overlay.Field{Label: "WIND", Value: val})
	}

	if v, ok := r.HumidityPct(); ok {
		f.Fields = append(f.Fields, overlay.Field{Label: "HUMIDITY", Value: fmt.Sprintf("%.0f%%", v)})
	}
	if v, ok := r.DewPointF(); ok {
		f.Fields = append(f.Fields, overlay.Field{Label: "DEW POINT", Value: fmt.Sprintf("%.0f°F", round0(v))})
	}
	if v, ok := r.PressureInHg(p.Elevation); ok {
		f.Fields = append(f.Fields, overlay.Field{Label: "PRESSURE", Value: fmt.Sprintf("%.2f in", v)})
	}

	if p.MaxFields > 0 && len(f.Fields) > p.MaxFields {
		f.Fields = f.Fields[:p.MaxFields]
	}
	return f
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
