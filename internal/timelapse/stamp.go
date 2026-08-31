package timelapse

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StampInterval is what a displayed time is rounded up to.
//
// It matches the archive's cadence, and rounding to it is what makes the clock
// tick rather than jitter: the camera's frames land at 00:04:25, 00:14:25,
// 00:24:40 and so on, so the real times drift by seconds and read as noise. Each
// frame stands for the ten minutes ending at the label it is given.
const StampInterval = 10 * time.Minute

// frameTime reconstructs when a frame was taken, from where it is filed.
//
// The archive encodes the moment in the object name — a zero-padded HHMMSS under
// a dated directory — and it is already in the site's local zone, because that is
// the zone the daemon writing it runs in. So there is nothing to convert and no
// daylight-saving edge to get wrong: the name is the local wall clock, and the
// label on the frame is the same wall clock.
func frameTime(day time.Time, path string) (time.Time, error) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if len(name) != 6 {
		return time.Time{}, fmt.Errorf("timelapse: %q is not an HHMMSS frame name", filepath.Base(path))
	}

	var parts [3]int
	for i := range parts {
		v, err := strconv.Atoi(name[i*2 : i*2+2])
		if err != nil {
			return time.Time{}, fmt.Errorf("timelapse: %q is not an HHMMSS frame name", filepath.Base(path))
		}
		parts[i] = v
	}
	if parts[0] > 23 || parts[1] > 59 || parts[2] > 59 {
		return time.Time{}, fmt.Errorf("timelapse: %q is not a time of day", filepath.Base(path))
	}

	y, m, d := day.Date()
	return time.Date(y, m, d, parts[0], parts[1], parts[2], 0, day.Location()), nil
}

// roundUp rounds t up to the next StampInterval boundary, leaving a time that is
// already exactly on one alone.
//
// time.Truncate cannot be used to build this: it works in absolute time from the
// epoch, so in a zone offset by a fraction of an hour it lands off the clock's
// own boundaries. This does the arithmetic on the wall clock instead, which is
// what a viewer is reading.
//
// The last frames of a day round forward into the next one — 23:53 becomes
// 00:00, and the date with it. That is the honest answer for a frame standing
// for the ten minutes that end at midnight, and it is visible: the final frame
// of a day's video carries the following date.
func roundUp(t time.Time) time.Time {
	step := int(StampInterval / time.Minute)

	rem := t.Minute() % step
	if rem == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return t
	}
	advance := time.Duration(step-rem)*time.Minute -
		time.Duration(t.Second())*time.Second -
		time.Duration(t.Nanosecond())*time.Nanosecond
	return t.Add(advance)
}

// stampFor returns the date and time a frame is labelled with, as two separate
// strings.
//
// Two, and not one, because of how they reach ffmpeg. The concat demuxer's
// file_packet_metadata takes a value that ends at the first space, so a single
// "2026-08-26 13:40" arrives truncated to the date and the clock never appears.
// Keeping them apart puts the space in the drawtext template, where it is only
// text.
func stampFor(day time.Time, path string) (date, clock string, err error) {
	t, err := frameTime(day, path)
	if err != nil {
		return "", "", err
	}
	t = roundUp(t)
	return t.Format("2006-01-02"), t.Format("15:04"), nil
}
