package timelapse

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pacific(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("loading the zone: %v", err)
	}
	return loc
}

func TestFrameTimeComesFromTheName(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	got, err := frameTime(day, "/work/frames/2026-08-26/133510.jpg")
	if err != nil {
		t.Fatalf("frameTime: %v", err)
	}
	if want := time.Date(2026, 8, 26, 13, 35, 10, 0, time.UTC); !got.Equal(want) {
		t.Errorf("frameTime = %s, want %s", got, want)
	}
}

// The archive's names are already the site's local wall clock, so the label is
// the same wall clock — no conversion, and nothing to get wrong at a
// daylight-saving boundary.
func TestFrameTimeStaysInTheDaysZone(t *testing.T) {
	loc := pacific(t)
	// 2026-11-01 is when the clocks go back here.
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)

	got, err := frameTime(day, "013000.jpg")
	if err != nil {
		t.Fatalf("frameTime: %v", err)
	}
	if got.Location() != loc {
		t.Errorf("location = %s, want %s", got.Location(), loc)
	}
	if h, m, _ := got.Clock(); h != 1 || m != 30 {
		t.Errorf("clock = %02d:%02d, want 01:30", h, m)
	}
}

func TestFrameTimeRejectsWhatIsNotAFrameName(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"latest.jpg", "1335.jpg", "13351.jpg", "xxyyzz.jpg", "993510.jpg", "136010.jpg"} {
		if _, err := frameTime(day, name); err == nil {
			t.Errorf("frameTime(%q) succeeded, want an error", name)
		}
	}
}

// The camera's frames land at 00:04:25, 00:14:25, 00:24:40 — real times that
// drift by seconds and read as noise. Rounding up makes the clock tick.
func TestRoundUpMakesTheClockTick(t *testing.T) {
	loc := pacific(t)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"13:35:10", "13:40"},
		{"00:04:25", "00:10"},
		{"00:14:25", "00:20"},
		{"00:24:40", "00:30"},
		{"13:30:00", "13:30"}, // already exactly on a boundary
		{"13:30:01", "13:40"}, // one second past it is the next one
		{"09:59:59", "10:00"},
	} {
		parts := strings.Split(tc.in, ":")
		h, m, sec := atoi(t, parts[0]), atoi(t, parts[1]), atoi(t, parts[2])
		got := roundUp(time.Date(2026, 8, 26, h, m, sec, 0, loc))
		if got.Format("15:04") != tc.want {
			t.Errorf("roundUp(%s) = %s, want %s", tc.in, got.Format("15:04"), tc.want)
		}
		if got.Second() != 0 {
			t.Errorf("roundUp(%s) kept %d seconds", tc.in, got.Second())
		}
	}
}

// Rounding up past the last boundary of a day carries the date with it. This is
// the honest answer for a frame standing for the ten minutes that end at
// midnight, and it is visible: the last frame of a day's video shows the next
// day's date.
func TestRoundUpCarriesTheDateOverMidnight(t *testing.T) {
	loc := pacific(t)
	got := roundUp(time.Date(2026, 8, 26, 23, 53, 40, 0, loc))

	if want := "2026-08-27 00:00"; got.Format("2006-01-02 15:04") != want {
		t.Errorf("roundUp = %s, want %s", got.Format("2006-01-02 15:04"), want)
	}
}

// A frame is never labelled with a time before it was taken.
func TestRoundUpNeverGoesBackwards(t *testing.T) {
	loc := pacific(t)
	for h := range 24 {
		for _, m := range []int{0, 1, 5, 9, 10, 29, 30, 31, 59} {
			for _, sec := range []int{0, 1, 30, 59} {
				in := time.Date(2026, 8, 26, h, m, sec, 0, loc)
				if got := roundUp(in); got.Before(in) {
					t.Fatalf("roundUp(%s) = %s, which is earlier", in.Format("15:04:05"), got.Format("15:04:05"))
				}
			}
		}
	}
}

func TestRoundUpNeverMovesMoreThanTheInterval(t *testing.T) {
	loc := pacific(t)
	for m := range 60 {
		for _, sec := range []int{0, 1, 59} {
			in := time.Date(2026, 8, 26, 12, m, sec, 0, loc)
			if d := roundUp(in).Sub(in); d < 0 || d > StampInterval {
				t.Errorf("roundUp(%s) moved by %s, want 0..%s", in.Format("15:04:05"), d, StampInterval)
			}
		}
	}
}

func TestStampForSplitsTheDateFromTheClock(t *testing.T) {
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, pacific(t))

	date, clock, err := stampFor(day, filepath.Join("frames", "133510.jpg"))
	if err != nil {
		t.Fatalf("stampFor: %v", err)
	}
	if date != "2026-08-26" || clock != "13:40" {
		t.Errorf("stampFor = %q %q, want \"2026-08-26\" \"13:40\"", date, clock)
	}
	// The concat demuxer ends a metadata value at the first space, so neither
	// half may contain one — a combined value arrives truncated to the date and
	// the clock silently never appears.
	for _, v := range []string{date, clock} {
		if strings.ContainsAny(v, " \t") {
			t.Errorf("%q contains whitespace, which file_packet_metadata truncates at", v)
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
