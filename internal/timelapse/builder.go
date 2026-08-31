package timelapse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the object store, narrowed to what building a timelapse needs.
//
// Declared here rather than taken from internal/gcs so this package carries no
// dependency on a particular bucket implementation, which is also what lets the
// tests run against a map.
type Store interface {
	// List returns every object name under a prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Get downloads an object.
	Get(ctx context.Context, name string) ([]byte, error)
	// Put uploads an object, replacing whatever was there.
	Put(ctx context.Context, name string, data []byte, contentType, cacheControl string) error
}

// encoder is the encoding half of a build, narrowed to what the builder drives.
//
// It is an interface so the assembly logic — which day is missing, which
// segments join into which product, what happens when a day has no frames — can
// be tested without ffmpeg on the machine. That matters here more than it
// usually would: this suite is not permitted to skip, so a test that needed a
// binary CI might not have would have to be deleted instead.
type encoder interface {
	Encode(ctx context.Context, frames []string, day time.Time, dst string) error
	Join(ctx context.Context, segments []string, dst string) error
	GIF(ctx context.Context, src, dst string, o GIFOptions) error
}

// Cache-Control for what this package writes.
//
// A dated product never changes once written, so it caches for a year. The
// latest-* names are rewritten nightly, so they get ten minutes — long enough
// to absorb a burst of viewers, short enough that nobody is looking at
// yesterday's month a day later.
const (
	immutableCache = "public, max-age=31536000, immutable"
	latestCache    = "public, max-age=600"
	// Today is rebuilt every half hour, so it is cached for a fraction of that.
	// Ten minutes on a video that is twenty minutes stale by the time it is
	// replaced would mean a viewer refreshing to see nothing new.
	todayCache = "public, max-age=300"
	videoType  = "video/mp4"
	gifType    = "image/gif"
)

// Builder assembles segments and products for one bucket.
type Builder struct {
	Store  Store
	Layout Layout
	Enc    encoder

	// WorkDir holds downloaded frames and cached segments for the duration of a
	// run. On Cloud Run this is memory, not disk, which is why a day's frames
	// are deleted as soon as they have been encoded.
	WorkDir string

	// Workers bounds concurrent downloads and decodes.
	Workers int

	// GIF bounds the animated previews published beside each product.
	GIF GIFOptions

	Log *slog.Logger
}

func (b *Builder) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

func (b *Builder) workers() int {
	if b.Workers <= 0 {
		return 8
	}
	return b.Workers
}

// ErrNoFrames reports a day the archive has nothing for. It is not a failure:
// the camera or the link was down, and the products are built from the days
// that do exist.
var ErrNoFrames = errors.New("timelapse: no frames archived for this day")

// segmentPath is where a segment is cached locally during a run.
func (b *Builder) segmentPath(day time.Time, v Variant) string {
	return filepath.Join(b.WorkDir, "segments", filepath.Base(b.Layout.Segment(day, v)))
}

// Ensure makes both segments for each day available locally, building any the
// bucket does not already have.
//
// This is what makes the job self-healing and self-bootstrapping. A first run
// finds no segments and builds the whole window; a run after a week of outage
// builds the days it missed; an ordinary run finds everything but last night
// already done and builds one day. No separate backfill command, and no state
// beyond what is in the bucket.
//
// build bounds how many days one run will encode, so a first run against a long
// archive cannot exceed the job's timeout. Days beyond the bound are skipped
// this time and picked up by the next run.
func (b *Builder) Ensure(ctx context.Context, days []time.Time, build int) ([]time.Time, error) {
	have, err := b.existingSegments(ctx)
	if err != nil {
		return nil, err
	}

	var ready []time.Time
	built := 0

	for _, day := range days {
		if ctx.Err() != nil {
			return ready, ctx.Err()
		}
		name := day.Format(DateFormat)

		if have[b.Layout.Segment(day, Full)] && have[b.Layout.Segment(day, DaylightOnly)] {
			// Present in the bucket is enough. Which of the two variants is
			// actually wanted depends on the product, and Product fetches the
			// one it needs — downloading both for every day here would pull the
			// full segment for all thirty when only the last seven use it.
			ready = append(ready, day)
			continue
		}

		if built >= build {
			b.log().Warn("segment build budget reached; the day is left out until the next run",
				"day", name, "budget", build)
			continue
		}

		built++
		if err := b.buildSegments(ctx, day); err != nil {
			if errors.Is(err, ErrNoFrames) {
				// Expected for a day the camera was down. Not an error worth
				// failing the run over, but worth saying out loud: a hole in a
				// month is otherwise invisible.
				b.log().Warn("no frames archived; the day is left out", "day", name)
				continue
			}
			b.log().Error("building a segment failed; the day is left out", "day", name, "error", err)
			continue
		}
		ready = append(ready, day)
	}

	return ready, nil
}

// existingSegments lists what the bucket already holds, as a set.
//
// One listing rather than a metadata read per day: a month is sixty objects to
// check and a single List answers for all of them.
func (b *Builder) existingSegments(ctx context.Context) (map[string]bool, error) {
	names, err := b.Store.List(ctx, b.Layout.SegmentPrefix())
	if err != nil {
		return nil, fmt.Errorf("timelapse: listing segments: %w", err)
	}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	return have, nil
}

// localSegment returns the path to a day's segment, fetching it from the bucket
// if this run did not just build it.
func (b *Builder) localSegment(ctx context.Context, day time.Time, v Variant) (string, error) {
	dst := b.segmentPath(day, v)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // built earlier in this run, or already fetched
	}

	data, err := b.Store.Get(ctx, b.Layout.Segment(day, v))
	if err != nil {
		return "", err
	}
	if err := writeFile(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}

// buildSegments encodes both variants for a day and uploads them.
func (b *Builder) buildSegments(ctx context.Context, day time.Time) error {
	frames, err := b.fetchFrames(ctx, day)
	if err != nil {
		return err
	}
	// The frames are the bulk of what a run holds — 125 MB for a day — and a
	// backfill walks thirty of them. Dropping each day's as soon as it is
	// encoded keeps the high-water mark at one day rather than the whole window.
	defer func() {
		if err := os.RemoveAll(b.framesDir(day)); err != nil {
			b.log().Warn("clearing the frame directory failed", "day", day.Format(DateFormat), "error", err)
		}
	}()

	sel, err := Daylight(ctx, frames, b.workers())
	if err != nil {
		return err
	}
	if len(sel.Unreadable) > 0 {
		b.log().Warn("frames would not decode and were left out",
			"day", day.Format(DateFormat), "count", len(sel.Unreadable), "first", filepath.Base(sel.Unreadable[0]))
	}

	// A day the camera spent entirely in IR — a whole day of fog, or midwinter
	// at a latitude this is not at — would produce an empty daylight segment,
	// which ffmpeg cannot encode and which would then be missing from the
	// monthly for good. Falling back to the full day keeps the month contiguous.
	daylight := sel.Frames
	if len(daylight) == 0 {
		b.log().Warn("no daylight frames; the day goes into the monthly whole",
			"day", day.Format(DateFormat), "frames", len(frames))
		daylight = frames
	}

	b.log().Info("building segments",
		"day", day.Format(DateFormat),
		"frames", len(frames), "daylight", len(daylight), "night", sel.Night)

	for _, s := range []struct {
		variant Variant
		frames  []string
	}{
		{Full, frames},
		{DaylightOnly, daylight},
	} {
		dst := b.segmentPath(day, s.variant)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("timelapse: %w", err)
		}
		if err := b.Enc.Encode(ctx, s.frames, day, dst); err != nil {
			return err
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			return fmt.Errorf("timelapse: %w", err)
		}
		if err := b.Store.Put(ctx, b.Layout.Segment(day, s.variant), data, videoType, immutableCache); err != nil {
			return fmt.Errorf("timelapse: uploading the %s segment for %s: %w", s.variant, day.Format(DateFormat), err)
		}
	}
	return nil
}

func (b *Builder) framesDir(day time.Time) string {
	return filepath.Join(b.WorkDir, "frames", day.Format(DateFormat))
}

// fetchFrames downloads a day's masters, returning their local paths in
// chronological order.
func (b *Builder) fetchFrames(ctx context.Context, day time.Time) ([]string, error) {
	names, err := b.Store.List(ctx, b.Layout.ArchiveDay(day))
	if err != nil {
		return nil, fmt.Errorf("timelapse: listing %s: %w", day.Format(DateFormat), err)
	}
	names = keepJPEGs(names)
	if len(names) == 0 {
		return nil, ErrNoFrames
	}
	// Archived names are zero-padded HHMMSS, so sorting them is sorting by time.
	sort.Strings(names)

	dir := b.framesDir(day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("timelapse: %w", err)
	}

	paths := make([]string, len(names))
	errs := make([]error, len(names))

	var wg sync.WaitGroup
	sem := make(chan struct{}, b.workers())

	for i, name := range names {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, err := b.Store.Get(ctx, name)
			if err != nil {
				errs[i] = err
				return
			}
			dst := filepath.Join(dir, path.Base(name))
			if err := writeFile(dst, data); err != nil {
				errs[i] = err
				return
			}
			paths[i] = dst
		}(i, name)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// One frame that would not download is a gap of ten minutes, which nobody
	// will see. Failing the day over it would lose the other 141.
	kept := make([]string, 0, len(paths))
	var failed int
	for i, p := range paths {
		if errs[i] != nil || p == "" {
			failed++
			continue
		}
		kept = append(kept, p)
	}
	if failed > 0 {
		b.log().Warn("frames would not download and were left out",
			"day", day.Format(DateFormat), "count", failed, "of", len(names))
	}
	if len(kept) == 0 {
		return nil, ErrNoFrames
	}
	return kept, nil
}

// Product joins the segments for days and uploads the result under both its
// dated name and the stable latest name, with an animated preview beside it.
func (b *Builder) Product(ctx context.Context, k Kind, v Variant, day time.Time, days []time.Time) error {
	segments := make([]string, 0, len(days))
	for _, d := range days {
		p, err := b.localSegment(ctx, d, v)
		if err != nil {
			// One day of a month. Losing the other twenty-nine over it would be
			// the wrong trade, and Ensure has usually already said why.
			b.log().Warn("the segment is unavailable; the day is left out of the product",
				"kind", string(k), "day", d.Format(DateFormat), "variant", string(v), "error", err)
			continue
		}
		segments = append(segments, p)
	}
	if len(segments) == 0 {
		return fmt.Errorf("timelapse: no segments available for the %s %s", v, k)
	}

	dst := filepath.Join(b.WorkDir, string(k)+"-"+string(v)+".mp4")
	if err := b.Enc.Join(ctx, segments, dst); err != nil {
		return err
	}

	if err := b.publish(ctx, k, v, day, dst, immutableCache); err != nil {
		return err
	}
	b.log().Info("product built",
		"kind", string(k), "variant", string(v), "days", len(segments), "object", b.Layout.Latest(k, v))
	return nil
}

// Today builds the day so far, straight from the frames.
//
// It is the one product that cannot come from segments. A segment is written
// once under a name that says which day it is and is never revisited, which is
// exactly wrong for a window that grows every ten minutes — so today is encoded
// from scratch each run. That costs one day's frames per run rather than one
// day's frames per day, which is the price of it being current.
//
// It also gets no dated copy. It is superseded every half hour, and by midnight
// it has become the yesterday product anyway.
func (b *Builder) Today(ctx context.Context, day time.Time) error {
	frames, err := b.fetchFrames(ctx, day)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(b.framesDir(day)); err != nil {
			b.log().Warn("clearing the frame directory failed", "day", day.Format(DateFormat), "error", err)
		}
	}()

	sel, err := Daylight(ctx, frames, b.workers())
	if err != nil {
		return err
	}
	daylight := sel.Frames
	if len(daylight) == 0 {
		// Every run before dawn is this, so it is not a warning. The night
		// frames still make a legitimate full-day video; there is simply no
		// daylight cut of a day that has not got light yet.
		b.log().Info("no daylight yet today; publishing the full day only",
			"day", day.Format(DateFormat), "frames", len(frames))
	}

	b.log().Info("building today",
		"day", day.Format(DateFormat), "frames", len(frames), "daylight", len(daylight), "night", sel.Night)

	for _, s := range []struct {
		variant Variant
		frames  []string
	}{
		{Full, frames},
		{DaylightOnly, daylight},
	} {
		if len(s.frames) == 0 {
			continue
		}
		dst := filepath.Join(b.WorkDir, "today-"+string(s.variant)+".mp4")
		if err := b.Enc.Encode(ctx, s.frames, day, dst); err != nil {
			return err
		}
		if err := b.publish(ctx, Today, s.variant, day, dst, todayCache); err != nil {
			return err
		}
	}
	return nil
}

// publish uploads a finished video under its stable name, its dated name where
// it has one, and renders the animated preview beside it.
func (b *Builder) publish(ctx context.Context, k Kind, v Variant, day time.Time, src, datedCache string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("timelapse: %w", err)
	}

	cache := latestCache
	if k == Today {
		cache = todayCache
	}

	// Today has no dated copy: it would be superseded forty-eight times a day
	// and be indistinguishable from the yesterday product by the end of it.
	if k != Today {
		if err := b.Store.Put(ctx, b.Layout.Product(k, v, day), data, videoType, datedCache); err != nil {
			return fmt.Errorf("timelapse: uploading the dated %s %s: %w", v, k, err)
		}
	}
	if err := b.Store.Put(ctx, b.Layout.Latest(k, v), data, videoType, cache); err != nil {
		return fmt.Errorf("timelapse: uploading %s: %w", b.Layout.Latest(k, v), err)
	}

	// A failed preview must not lose the video that was built successfully. The
	// GIF is a convenience for embedding; the mp4 is the product.
	gif := src[:len(src)-len(filepath.Ext(src))] + ".gif"
	if err := b.Enc.GIF(ctx, src, gif, b.GIF); err != nil {
		b.log().Error("rendering the preview failed; the video is published without one",
			"kind", string(k), "variant", string(v), "error", err)
		return nil
	}
	gifData, err := os.ReadFile(gif)
	if err != nil {
		b.log().Error("reading the preview failed", "kind", string(k), "error", err)
		return nil
	}
	if err := b.Store.Put(ctx, b.Layout.LatestGIF(k, v), gifData, gifType, cache); err != nil {
		return fmt.Errorf("timelapse: uploading %s: %w", b.Layout.LatestGIF(k, v), err)
	}

	b.log().Info("published",
		"kind", string(k), "variant", string(v),
		"mp4", len(data), "gif", len(gifData), "object", b.Layout.Latest(k, v))
	return nil
}

// keepJPEGs drops anything in the archive prefix that is not a frame.
func keepJPEGs(names []string) []string {
	out := names[:0:0]
	for _, n := range names {
		if strings.HasSuffix(n, ".jpg") {
			out = append(out, n)
		}
	}
	return out
}

// writeFile writes data, creating the parent directory.
func writeFile(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("timelapse: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("timelapse: %w", err)
	}
	return nil
}
