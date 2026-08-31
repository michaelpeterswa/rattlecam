package timelapse

import (
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/publish"
)

// Kind is one of the products.
type Kind string

const (
	// Today is the day so far, rebuilt every half hour. It is the only product
	// whose window is still growing, which is why it is the only one that cannot
	// be assembled from cached segments.
	Today Kind = "today"
	// Yesterday is the last complete day. Named for what it is rather than for
	// how often it is built: "daily" stops meaning anything once there is also a
	// product covering today.
	Yesterday Kind = "yesterday"
	Weekly    Kind = "weekly"
	Monthly   Kind = "monthly"
)

// Kinds is every product, in the order a page would most likely show them.
var Kinds = []Kind{Today, Yesterday, Weekly, Monthly}

// Variant is which of a day's frames a segment or product holds.
type Variant string

const (
	// Full is every archived frame, night included.
	Full Variant = "full"
	// DaylightOnly drops the frames the camera took in IR.
	DaylightOnly Variant = "daylight"
)

// DefaultVariant is the treatment carried by the unsuffixed name.
//
// It differs by period on purpose, because the useful default does. Over a day
// you want the whole day — the dark hours are a few seconds and the transitions
// through them are the interesting part. Over a month you do not: the same black
// rectangle recurs for a third of the running time, forty minutes of it, and
// nobody watches a month to see thirty nights.
//
// So latest-today.mp4 includes the night and latest-monthly.mp4 does not, and
// the -daylight suffix names the alternative wherever one is published. This is
// worth stating plainly in the README rather than leaving a page author to
// discover it.
func DefaultVariant(k Kind) Variant {
	if k == Monthly {
		return DaylightOnly
	}
	return Full
}

// Variants lists the treatments published for a kind.
//
// The monthly is day-only and not both ways: a night-included month is a
// hundred and fifty megabytes of which a third is the same black rectangle, and
// nobody watches a month to see thirty nights.
//
// This is the one place that matrix is written down. The job iterates it to
// decide what to build and the gateway iterates it to decide what to serve, so
// the two cannot come to disagree about which objects exist.
func Variants(k Kind) []Variant {
	if k == Monthly {
		return []Variant{DaylightOnly}
	}
	return []Variant{Full, DaylightOnly}
}

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

// suffix is the variant part of a name, empty for the default.
func suffix(k Kind, v Variant) string {
	if v == DefaultVariant(k) {
		return ""
	}
	return "-" + string(v)
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
//
// Today has no dated copy: it is superseded every half hour, and by the end of
// the day it has become the yesterday product anyway.
func (l Layout) Product(k Kind, v Variant, t time.Time) string {
	return l.key("timelapse", string(k), t.Format(DateFormat)+suffix(k, v)+".mp4")
}

// Latest is the stable name for the most recent of a product.
//
// Stable names are what a page can link to without being rewritten every night,
// which is the same reason latest.jpg exists.
func (l Layout) Latest(k Kind, v Variant) string {
	return l.key("timelapse", "latest-"+string(k)+suffix(k, v)+".mp4")
}

// LatestGIF is the animated preview beside it.
//
// Only the stable names get one. A GIF exists to be embedded in a page, and a
// page embeds the current thing — a dated GIF would double the GIF storage to
// serve a reader nobody has.
func (l Layout) LatestGIF(k Kind, v Variant) string {
	return l.key("timelapse", "latest-"+string(k)+suffix(k, v)+".gif")
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
