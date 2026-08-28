// Package timelapse builds day, week and month videos from the archived
// masters.
//
// The archive is a frame every ten minutes, kept forever as the camera's own
// 4K JPEGs. Turning a month of that into a video means reading four thousand
// stills, and doing it from scratch every night would read the same thirty days
// over and over. So a day is encoded once into a segment, and the longer
// products are stitched from segments by copying the compressed stream rather
// than decoding it again — which costs milliseconds and loses nothing, because
// no pixel is re-encoded.
package timelapse

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Encoder turns frames into a segment, and segments into a product.
//
// Every field here is part of the segment fingerprint, because segments are
// only joinable when they were produced identically — see Fingerprint.
type Encoder struct {
	// FFmpeg is the binary to run. Empty means "ffmpeg", found on PATH.
	FFmpeg string

	// FPS is the output frame rate. It is not a speed control in the usual
	// sense: the archive's ten-minute spacing already fixes how much real time
	// a second of video covers, so this only decides how long the result runs.
	FPS int

	// Width narrows the 4K master. Height follows the aspect ratio.
	Width int

	// CRF is x264's quality target: lower is better and larger.
	CRF int

	// Preset trades encoding time for compression efficiency.
	Preset string

	// Timeout bounds a single ffmpeg run. A month is a few thousand frames and
	// takes minutes; a hung encoder must not hold a scheduled job open forever.
	Timeout time.Duration
}

// Defaults for anything left unset. 1280 wide and CRF 25 are chosen for what
// these are: web deliverables served through the gateway. The 4K masters stay
// in the archive, so a higher-quality cut can always be made later from the
// same frames.
const (
	DefaultFPS     = 24
	DefaultWidth   = 1280
	DefaultCRF     = 25
	DefaultPreset  = "slow"
	DefaultTimeout = 30 * time.Minute
)

func (e *Encoder) ffmpeg() string {
	if e.FFmpeg == "" {
		return "ffmpeg"
	}
	return e.FFmpeg
}

func (e *Encoder) fps() int {
	if e.FPS <= 0 {
		return DefaultFPS
	}
	return e.FPS
}

func (e *Encoder) width() int {
	if e.Width <= 0 {
		return DefaultWidth
	}
	return e.Width
}

func (e *Encoder) crf() int {
	if e.CRF <= 0 || e.CRF > 51 {
		return DefaultCRF
	}
	return e.CRF
}

func (e *Encoder) preset() string {
	if e.Preset == "" {
		return DefaultPreset
	}
	return e.Preset
}

func (e *Encoder) timeout() time.Duration {
	if e.Timeout <= 0 {
		return DefaultTimeout
	}
	return e.Timeout
}

// Fingerprint identifies the settings a segment was encoded with, and is part
// of the segment's object name.
//
// This is what makes a settings change safe. Segments are joined by copying
// compressed streams, which requires every input to share a codec, resolution
// and frame rate — join a 1280-wide segment to a 1920-wide one and the result
// is a file whose second half no player will decode correctly. Keying the
// stored segments on their settings means a changed encoder simply cannot find
// the old ones: it builds a fresh set under a new name, and the stale ones age
// out with the bucket's lifecycle rules instead of silently corrupting a month.
func (e *Encoder) Fingerprint() string {
	return fmt.Sprintf("%dw-%dfps-crf%d", e.width(), e.fps(), e.crf())
}

// Encode writes frames to dst as an H.264 mp4.
//
// The frames are handed to ffmpeg through a concat list rather than a glob or a
// numbered pattern, because archived names are wall-clock times with gaps in
// them — a pattern would need them renamed into a contiguous sequence first,
// which means copying several gigabytes to no purpose.
func (e *Encoder) Encode(ctx context.Context, frames []string, dst string) error {
	if len(frames) == 0 {
		return fmt.Errorf("timelapse: no frames to encode into %s", filepath.Base(dst))
	}

	list, cleanup, err := writeConcatList(frames, filepath.Dir(dst), "frames")
	if err != nil {
		return err
	}
	defer cleanup()

	return e.run(ctx, e.encodeArgs(list, dst))
}

// Join concatenates segments into dst without re-encoding.
func (e *Encoder) Join(ctx context.Context, segments []string, dst string) error {
	if len(segments) == 0 {
		return fmt.Errorf("timelapse: no segments to join into %s", filepath.Base(dst))
	}

	list, cleanup, err := writeConcatList(segments, filepath.Dir(dst), "segments")
	if err != nil {
		return err
	}
	defer cleanup()

	return e.run(ctx, joinArgs(list, dst))
}

// encodeArgs builds the frames-to-mp4 command line.
//
// Kept separate from running it so the arguments can be tested without ffmpeg
// installed, which matters because the suite is not allowed to skip.
func (e *Encoder) encodeArgs(list, dst string) []string {
	// Two seconds of pictures between keyframes, with scene detection off.
	//
	// Both settings are here for the join. Segments can only be copied into one
	// stream if their GOP structure matches, and scene detection would make that
	// depend on the weather: every frame in this material is a scene cut by
	// x264's reckoning — consecutive frames are ten minutes apart and share
	// almost nothing — so left on it emits an I-frame for nearly every picture,
	// at several times the size and with keyframe placement that differs from
	// one day to the next.
	gop := strconv.Itoa(2 * e.fps())

	return []string{
		"-y",
		"-loglevel", "warning",
		"-r", strconv.Itoa(e.fps()),
		"-f", "concat",
		"-safe", "0",
		"-i", list,
		"-vf", e.filters(),
		"-c:v", "libx264",
		"-crf", strconv.Itoa(e.crf()),
		"-preset", e.preset(),
		"-profile:v", "high",
		"-level", "4.1",
		"-g", gop,
		"-keyint_min", gop,
		"-sc_threshold", "0",
		"-pix_fmt", "yuv420p",
		// faststart moves the index to the front so a browser can begin playing
		// before the whole file has arrived. On a month-long product that is the
		// difference between playing immediately and waiting for 90 MB.
		"-movflags", "+faststart",
		"-an",
		"-r", strconv.Itoa(e.fps()),
		dst,
	}
}

// filters is the video filter chain.
func (e *Encoder) filters() string {
	return strings.Join([]string{
		// Lanczos because this is a 3x downscale of detailed terrain, where the
		// default bilinear visibly softens ridgelines.
		fmt.Sprintf("scale=%d:-2:flags=lanczos", e.width()),
		// The camera meters every frame independently, ten minutes apart, so
		// consecutive pictures of the same scene differ in exposure by more than
		// the scene does. Without this the result strobes.
		"deflicker=mode=pm:size=10",
		"format=yuv420p",
	}, ",")
}

// joinArgs builds the segments-to-product command line.
func joinArgs(list, dst string) []string {
	return []string{
		"-y",
		"-loglevel", "warning",
		"-f", "concat",
		"-safe", "0",
		"-i", list,
		// The whole point: copy the compressed stream. No decode, no re-encode,
		// no generation loss, and a month joins in well under a second.
		"-c", "copy",
		"-movflags", "+faststart",
		dst,
	}
}

// run executes ffmpeg and turns a failure into an error worth reading.
func (e *Encoder) run(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, e.ffmpeg(), args...)

	// ffmpeg says why it failed on stderr and nowhere else. Without this an
	// operator gets "exit status 1" and has to reproduce the run by hand.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timelapse: ffmpeg timed out after %s: %w", e.timeout(), ctx.Err())
		}
		return fmt.Errorf("timelapse: ffmpeg: %w: %s", err, lastLines(stderr.String(), 5))
	}
	return nil
}

// lastLines keeps the tail of ffmpeg's output. The interesting line is the last
// one; the rest is banner.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

// writeConcatList writes an ffmpeg concat demuxer list next to the output.
func writeConcatList(paths []string, dir, kind string) (string, func(), error) {
	f, err := os.CreateTemp(dir, "."+kind+"-*.txt")
	if err != nil {
		return "", func() {}, fmt.Errorf("timelapse: concat list: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }

	var b strings.Builder
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		b.WriteString("file " + quoteConcatPath(abs) + "\n")
	}

	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("timelapse: concat list: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("timelapse: concat list: %w", err)
	}
	return f.Name(), cleanup, nil
}

// quoteConcatPath renders a path for the concat demuxer, which single-quotes
// its arguments and has no escape sequence for a quote inside them: the file
// has to close the quote, emit an escaped one, and open a new quote.
func quoteConcatPath(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
