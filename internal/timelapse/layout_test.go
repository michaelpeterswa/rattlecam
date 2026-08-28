package timelapse

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/publish"
)

func TestSegmentNamesCarryTheFingerprint(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	l := Layout{Fingerprint: "1280w-24fps-crf25"}

	if got, want := l.Segment(day, Full), "timelapse/segments/1280w-24fps-crf25/2026-08-26.mp4"; got != want {
		t.Errorf("full segment = %q, want %q", got, want)
	}
	if got, want := l.Segment(day, DaylightOnly), "timelapse/segments/1280w-24fps-crf25/2026-08-26-daylight.mp4"; got != want {
		t.Errorf("daylight segment = %q, want %q", got, want)
	}
}

// Segments are joined by copying compressed streams, which only works when the
// inputs were encoded identically. Two encoders that differ must therefore not
// be able to reach each other's segments.
func TestSegmentsFromDifferentSettingsCannotCollide(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	a := Layout{Fingerprint: (&Encoder{Width: 1280, FPS: 24, CRF: 25}).Fingerprint()}
	b := Layout{Fingerprint: (&Encoder{Width: 1920, FPS: 24, CRF: 25}).Fingerprint()}

	if a.Segment(day, Full) == b.Segment(day, Full) {
		t.Fatalf("a 1280-wide and a 1920-wide segment share the name %q", a.Segment(day, Full))
	}
}

func TestPrefixAppliesToEveryName(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	l := Layout{Prefix: "/rattlesnake/", Fingerprint: "fp"}

	for name, got := range map[string]string{
		"archive":  l.ArchiveDay(day),
		"segment":  l.Segment(day, Full),
		"product":  l.Product(Weekly, day),
		"latest":   l.Latest(Monthly),
		"segments": l.SegmentPrefix(),
	} {
		if len(got) < 12 || got[:12] != "rattlesnake/" {
			t.Errorf("%s = %q, want it under rattlesnake/", name, got)
		}
	}
}

// The archive layout is written by internal/publish and read here. If the two
// ever disagree the timelapse silently finds nothing, so this pins them
// together rather than trusting a duplicated format string.
func TestArchiveDayMatchesWhatThePublisherWrites(t *testing.T) {
	day := time.Date(2026, 3, 9, 14, 5, 4, 0, time.UTC)

	dir, name := publish.ArchivePath(day)
	written := "archive/" + filepath.ToSlash(filepath.Join(dir, name))

	prefix := Layout{}.ArchiveDay(day)
	if len(written) <= len(prefix) || written[:len(prefix)] != prefix {
		t.Errorf("publisher writes %q, which is not under the prefix %q the builder lists", written, prefix)
	}
}

func TestWindowIsOldestFirstAndInclusive(t *testing.T) {
	end := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	got := Window(end, 3)

	want := []string{"2026-08-24", "2026-08-25", "2026-08-26"}
	if len(got) != len(want) {
		t.Fatalf("got %d days, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Format(DateFormat) != want[i] {
			t.Errorf("day %d = %s, want %s", i, got[i].Format(DateFormat), want[i])
		}
	}
}

func TestWindowIsEmptyForANonPositiveCount(t *testing.T) {
	end := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{0, -1} {
		if got := Window(end, n); got != nil {
			t.Errorf("Window(_, %d) = %v, want nil", n, got)
		}
	}
}

// A month always spans a daylight-saving change twice a year. Stepping by
// 24-hour multiples across one lands on the wrong date, which shows up as a
// month with a day repeated and a day missing — and the missing day is silent,
// because a segment that is never asked for is never reported absent.
func TestWindowCrossesDaylightSavingWithoutRepeatingADay(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("loading the zone: %v", err)
	}
	// 2026-11-01 is when the clocks go back here, making that day 25 hours long.
	end := time.Date(2026, 11, 3, 0, 0, 0, 0, loc)

	seen := map[string]bool{}
	for _, d := range Window(end, 5) {
		name := d.Format(DateFormat)
		if seen[name] {
			t.Errorf("%s appears twice in the window", name)
		}
		seen[name] = true
	}

	for _, want := range []string{"2026-10-30", "2026-10-31", "2026-11-01", "2026-11-02", "2026-11-03"} {
		if !seen[want] {
			t.Errorf("%s is missing from the window", want)
		}
	}
}

// time.Truncate works in absolute time from the epoch, so in a zone offset by a
// fraction of an hour it lands inside the day rather than at its start.
func TestDayStartsAtMidnightInZonesOffsetByHalfAnHour(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata") // UTC+05:30
	if err != nil {
		t.Fatalf("loading the zone: %v", err)
	}

	got := Day(time.Date(2026, 8, 26, 17, 42, 3, 0, loc))

	if h, m, s := got.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("Day = %s, want midnight", got.Format(time.RFC3339))
	}
	if got.Format(DateFormat) != "2026-08-26" {
		t.Errorf("Day landed on %s, want 2026-08-26", got.Format(DateFormat))
	}
}
