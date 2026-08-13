package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/michaelpeterswa/rattlecam/internal/config"
	"github.com/michaelpeterswa/rattlecam/internal/wx"
)

func daemonFor(t *testing.T, elevation float64) (*daemon, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return &daemon{
		cfg: &config.Config{Elevation: elevation},
		log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}, &buf
}

func reading(p float64) *wx.Reading {
	return &wx.Reading{Fields: map[string]float64{"p": p}}
}

// The real case: a station at 1000 m reporting 899.65 mb with the elevation
// left at its default. Nothing else in the pipeline notices — the query
// succeeds, the field is present, the frame renders, and 26.57 in goes out.
func TestWarnsWhenElevationLooksUnset(t *testing.T) {
	d, buf := daemonFor(t, 0)
	d.checkElevation(reading(899.65))

	out := buf.String()
	if !strings.Contains(out, "SITE_ELEVATION_M") {
		t.Fatalf("no warning for 899.65 mb at elevation 0:\n%s", out)
	}
	// The warning has to carry the number that would publish, or it is just
	// telling someone to check something rather than showing them why.
	if !strings.Contains(out, "26.57") {
		t.Errorf("warning omits the value that would be published:\n%s", out)
	}
}

// Warning every fifteen seconds forever would train someone to ignore it.
func TestWarnsOnlyOnce(t *testing.T) {
	d, buf := daemonFor(t, 0)
	for range 5 {
		d.checkElevation(reading(899.65))
	}
	// Count log lines, not occurrences: the variable is named twice in one
	// record, in the message and again in the hint.
	var lines int
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "SITE_ELEVATION_M") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("warned %d times, want 1:\n%s", lines, buf.String())
	}
}

func TestSilentWhenConfigured(t *testing.T) {
	d, buf := daemonFor(t, 1000)
	d.checkElevation(reading(899.65))
	if buf.Len() != 0 {
		t.Errorf("warned despite a configured elevation:\n%s", buf.String())
	}
}

// A genuine sea-level station must not be nagged. 1013 is standard; 985 is an
// ordinary low.
func TestSilentAtSeaLevelPressures(t *testing.T) {
	for _, p := range []float64{1013.25, 1030, 985, 960} {
		d, buf := daemonFor(t, 0)
		d.checkElevation(reading(p))
		if buf.Len() != 0 {
			t.Errorf("warned for a plausible sea-level pressure %v:\n%s", p, buf.String())
		}
	}
}

func TestSilentWithoutPressure(t *testing.T) {
	d, buf := daemonFor(t, 0)
	d.checkElevation(&wx.Reading{Fields: map[string]float64{"temp": 17.26}})
	d.checkElevation(nil)
	if buf.Len() != 0 {
		t.Errorf("warned with no pressure field:\n%s", buf.String())
	}
}
