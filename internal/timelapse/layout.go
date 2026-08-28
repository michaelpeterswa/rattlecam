package timelapse

import (
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/publish"
)

// Kind is one of the three products built each night.
type Kind string

const (
	Daily   Kind = "daily"
	Weekly  Kind = "weekly"
	Monthly Kind = "monthly"
)

// Variant is which of a day's frames a segment holds.
type Variant string

const (
	// Full is every archived frame, night included.
	Full Variant = "full"
	// DaylightOnly drops the frames the camera took in IR. A month of full days
	// is more than a third black, and those hours carry nothing: the same dark
	// rectangle, forty minutes of screen time.
	DaylightOnly Variant = "daylight"
)

// DateFormat is how a day appears in an object name.
const DateFormat = "2006-01-02"

// Layout maps dates to object names in the bucket.
type Layout struct {
	// Prefix is the optional key prefix the daemon also writes under.
	Prefix string

	// Fingerprint keys segments to the encoder settings that produced them.
	Fingerprint string
}

// key applies the prefix.
func (l Layout) key(parts ...string) string {
	joined := path.Join(parts...)
	if p := strings.Trim(l.Prefix, "/"); p != "" {
		return path.Join(p, joined)
	}
	return joined
}

// ArchiveDay is the prefix holding one day's masters.
//
// It is derived from publish.ArchivePath rather than formatted here, so the
// reader of the archive and the writer of it cannot drift apart on what the
// layout is.
func (l Layout) ArchiveDay(t time.Time) string {
	dir, _ := publish.ArchivePath(t)
	return l.key("archive", filepath.ToSlash(dir)) + "/"
}

// Segment is where one day's encoded segment lives.
func (l Layout) Segment(t time.Time, v Variant) string {
	name := t.Format(DateFormat)
	if v != Full {
		name += "-" + string(v)
	}
	return l.key("timelapse", "segments", l.Fingerprint, name+".mp4")
}

// SegmentPrefix is where every segment for the current settings lives.
func (l Layout) SegmentPrefix() string {
	return l.key("timelapse", "segments", l.Fingerprint) + "/"
}

// Product is the dated copy of a finished video.
func (l Layout) Product(k Kind, t time.Time) string {
	return l.key("timelapse", string(k), t.Format(DateFormat)+".mp4")
}

// Latest is the stable name for the most recent of a product.
//
// Stable names are what a page can link to without being rewritten every night,
// which is the same reason latest.jpg exists.
func (l Layout) Latest(k Kind) string {
	return l.key("timelapse", "latest-"+string(k)+".mp4")
}

// Window lists the days ending at end, inclusive, oldest first.
//
// Dates are stepped with AddDate rather than by subtracting 24-hour multiples,
// because a window that spans a daylight-saving change contains a 23- and a
// 25-hour day and arithmetic on hours would land one of them on the wrong
// date — producing a month with a day repeated and a day missing.
func Window(end time.Time, days int) []time.Time {
	if days <= 0 {
		return nil
	}
	out := make([]time.Time, 0, days)
	for i := days - 1; i >= 0; i-- {
		out = append(out, end.AddDate(0, 0, -i))
	}
	return out
}

// Day truncates a time to midnight in its own location.
//
// Truncate cannot do this: it works in absolute time from the epoch, so for any
// zone that is not a whole number of hours from UTC it lands somewhere inside
// the day rather than at its start.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
