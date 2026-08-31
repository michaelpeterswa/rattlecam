package timelapse

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// GIFOptions bounds an animated preview.
type GIFOptions struct {
	// Width in pixels; the height follows the aspect ratio.
	Width int

	// FPS is the playback rate of the result.
	FPS int

	// MaxFrames caps how many pictures the file holds.
	//
	// This is the setting that matters, because GIF has no interframe
	// prediction worth the name: every frame costs roughly a whole picture, so
	// the size of the file is set by how many there are and almost nothing else.
	// A month is 2,700 frames, which at any usable width is tens of megabytes —
	// so the long products are decimated to fit rather than rendered whole.
	MaxFrames int
}

// Defaults for a preview meant to be dropped into a page as a plain <img>.
const (
	DefaultGIFWidth     = 480
	DefaultGIFFPS       = 12
	DefaultGIFMaxFrames = 200
)

func (o GIFOptions) width() int {
	if o.Width <= 0 {
		return DefaultGIFWidth
	}
	return o.Width
}

func (o GIFOptions) fps() int {
	if o.FPS <= 0 {
		return DefaultGIFFPS
	}
	return o.FPS
}

func (o GIFOptions) maxFrames() int {
	if o.MaxFrames <= 0 {
		return DefaultGIFMaxFrames
	}
	return o.MaxFrames
}

// GIF renders an animated preview of a finished product.
//
// It reads the mp4 rather than the frames, which means the branding, the
// downscale and the deflicker have all already happened — the GIF is a
// rendition of the video rather than a second, subtly different, build of it.
func (e *Encoder) GIF(ctx context.Context, src, dst string, o GIFOptions) error {
	frames, err := e.frameCount(ctx, src)
	if err != nil {
		return err
	}
	step := decimation(frames, o.maxFrames())

	// The palette is built from the frames that will actually be kept, not from
	// the whole video. A month's colours drift a long way between the first day
	// and the last, and a palette averaged over frames nobody will see spends
	// its 256 entries on the wrong things.
	palette := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".palette.png")
	defer func() { _ = os.Remove(palette) }()

	sel := o.selectChain(step)

	if err := e.run(ctx, []string{
		"-y", "-loglevel", "warning",
		"-i", src,
		"-vf", sel + ",palettegen=stats_mode=diff",
		palette,
	}); err != nil {
		return err
	}

	return e.run(ctx, []string{
		"-y", "-loglevel", "warning",
		"-i", src,
		"-i", palette,
		// diff_mode=rectangle is the one real compression GIF offers: only the
		// changed rectangle of each frame is stored. On a fixed camera where
		// most of the frame is unchanging terrain, it is worth a great deal.
		"-lavfi", sel + "[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle",
		"-loop", "0",
		dst,
	})
}

// selectChain keeps every step-th frame and restamps what survives so the
// result plays at the requested rate.
func (o GIFOptions) selectChain(step int) string {
	parts := []string{}
	if step > 1 {
		// The comma inside the expression is escaped because the filtergraph
		// parser splits on commas first: unescaped, "mod(n,step)" ends the
		// select filter halfway through its own argument.
		parts = append(parts, fmt.Sprintf(`select=not(mod(n\,%d))`, step))
	}
	parts = append(parts,
		fmt.Sprintf("setpts=N/(%d*TB)", o.fps()),
		fmt.Sprintf("scale=%d:-2:flags=lanczos", o.width()),
	)
	return strings.Join(parts, ",")
}

// decimation is how many source frames to advance per kept frame.
func decimation(frames, max int) int {
	if frames <= max || max <= 0 {
		return 1
	}
	return int(math.Ceil(float64(frames) / float64(max)))
}

// frameCount reports how many pictures a product holds.
//
// It is derived from the container's duration and the known frame rate rather
// than read with -count_frames, because counting frames decodes all of them —
// on a month that is a minute of work to learn a number the header already
// implies.
func (e *Encoder) frameCount(ctx context.Context, src string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, e.ffprobe(),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		src,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("timelapse: ffprobe %s: %w", filepath.Base(src), err)
	}

	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("timelapse: duration of %s: %w", filepath.Base(src), err)
	}
	n := int(seconds*float64(e.fps()) + 0.5)
	if n < 1 {
		n = 1
	}
	return n, nil
}
