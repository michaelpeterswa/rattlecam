package timelapse

import (
	"context"
	"fmt"
	"image/jpeg"
	"os"
	"runtime"
	"sync"

	"github.com/michaelpeterswa/rattlecam/internal/light"
)

// Selection is the result of sorting a day's frames into the ones worth keeping
// and the ones not.
type Selection struct {
	// Frames are the kept paths, in the order they were given.
	Frames []string

	// Night counts frames dropped for being night.
	Night int

	// Unreadable are frames that would not decode. They are dropped rather than
	// passed on: ffmpeg is no more likely to read them than this was, and one
	// truncated file in the bucket must not be able to fail a month.
	Unreadable []string
}

// Daylight keeps only the frames the camera took in colour.
//
// The signal is the camera's own IR-cut filter, read off the frame by
// internal/light, not a sunrise calculation. That is deliberate and it is not
// merely convenient: the question a monthly timelapse asks is "is there a
// picture here worth showing", and the filter swinging out is the camera
// answering exactly that, having already accounted for overcast, smoke and
// terrain shadow. Measured across three days on this site the signal is a clean
// step — mean chroma goes from 11-15 straight to 0.00 — with precisely two
// transitions a day and nothing ambiguous in between.
//
// It also needs no coordinates. A solar-elevation approach would want a
// latitude and longitude the daemon has never had to know.
func Daylight(ctx context.Context, paths []string, workers int) (Selection, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	// A 4K JPEG decode is tens of milliseconds and there are a few thousand of
	// them in a backfill, so they run in parallel. Results are written to a
	// fixed slot rather than appended, which is what keeps the output in
	// chronological order without a sort afterwards.
	type verdict struct {
		keep bool
		bad  bool
	}
	out := make([]verdict, len(paths))

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for i, p := range paths {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			night, ok := isNight(p)
			if !ok {
				out[i] = verdict{bad: true}
				return
			}
			out[i] = verdict{keep: !night}
		}(i, p)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return Selection{}, fmt.Errorf("timelapse: classifying frames: %w", err)
	}

	sel := Selection{Frames: make([]string, 0, len(paths))}
	for i, v := range out {
		switch {
		case v.bad:
			sel.Unreadable = append(sel.Unreadable, paths[i])
		case v.keep:
			sel.Frames = append(sel.Frames, paths[i])
		default:
			sel.Night++
		}
	}
	return sel, nil
}

// isNight decodes one frame and asks the light package about it. The second
// return reports whether the frame could be read at all.
func isNight(path string) (night, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close() //nolint:errcheck // read-only

	img, err := jpeg.Decode(f)
	if err != nil {
		return false, false
	}

	mono, known := light.Mono(img)
	if !known {
		// A frame whose colour model cannot answer is kept rather than dropped.
		// The archive is the camera's own bytes, which always decode to YCbCr,
		// so this should not happen — and if it does, a night frame in the
		// monthly is a far smaller problem than a day silently missing from it.
		return false, true
	}
	return mono, true
}
