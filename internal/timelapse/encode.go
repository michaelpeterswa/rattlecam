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
	"image/jpeg"
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

	// Logo is a local image composited into the top-left corner. Empty leaves
	// the video unbranded.
	//
	// It is burned into the segments rather than into the finished products,
	// which is what keeps the weekly and the monthly as stream copies. The cost
	// is that changing the branding invalidates every segment — handled, because
	// the logo is part of the fingerprint.
	Logo string

	// LogoHeight and LogoMargin are fractions of the output height, matching
	// theme.json's logo_height and logo_margin so a timelapse is branded the
	// same way as the still frames it is made of.
	LogoHeight float64
	LogoMargin float64

	// Font is the TrueType face the timestamp is drawn in. Empty leaves the
	// frames unstamped.
	Font string

	// StampHeight is the type size as a fraction of the output height, and
	// StampMargin its inset from the top-right corner — mirroring the crest in
	// the opposite corner.
	StampHeight float64
	StampMargin float64

	// FFprobe reads the duration of a finished product, which is how the GIF
	// pass knows how hard to decimate. Empty means "ffprobe", found on PATH
	// beside ffmpeg.
	FFprobe string

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

	// Matching theme.json, so the crest sits where a viewer already expects it
	// from the live frame.
	DefaultLogoHeight = 0.34
	DefaultLogoMargin = 0.025

	// Large enough to read on a phone at 480 px wide, which is the narrowest the
	// GIFs are rendered at.
	DefaultStampHeight = 0.045
	DefaultStampMargin = 0.025

	// stampChars is the length of "2026-08-30 14:30", and stampAdvance a
	// generous estimate of how wide one of its characters is relative to the
	// type size. Together they fix the box so it does not resize as the digits
	// change.
	stampChars   = 16
	stampAdvance = 0.52

	// stampPadding is the inset from the box edge to the text, as a fraction of
	// the type size.
	stampPadding = 0.28
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

func (e *Encoder) ffprobe() string {
	if e.FFprobe == "" {
		return "ffprobe"
	}
	return e.FFprobe
}

func (e *Encoder) logoHeight() float64 {
	if e.LogoHeight <= 0 || e.LogoHeight > 1 {
		return DefaultLogoHeight
	}
	return e.LogoHeight
}

func (e *Encoder) stampHeight() float64 {
	if e.StampHeight <= 0 || e.StampHeight > 1 {
		return DefaultStampHeight
	}
	return e.StampHeight
}

func (e *Encoder) stampMargin() float64 {
	if e.StampMargin < 0 || e.StampMargin > 1 {
		return DefaultStampMargin
	}
	return e.StampMargin
}

func (e *Encoder) logoMargin() float64 {
	if e.LogoMargin < 0 || e.LogoMargin > 1 {
		return DefaultLogoMargin
	}
	return e.LogoMargin
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
	fp := fmt.Sprintf("%dw-%dfps-crf%d", e.width(), e.fps(), e.crf())
	// The branding is burned in, so an unbranded segment and a branded one are
	// not interchangeable even though every codec setting matches. Without this
	// in the name, turning the logo on would leave a month stitched from a
	// mixture of the two.
	if e.Logo != "" {
		fp += fmt.Sprintf("-logo%d", int(e.logoHeight()*100+0.5))
	}
	// A stamped segment and an unstamped one are not interchangeable either, and
	// the size changes the pixels as surely as the presence does.
	if e.Font != "" {
		fp += fmt.Sprintf("-ts%d", int(e.stampHeight()*1000+0.5))
	}
	return fp
}

// Encode writes frames to dst as an H.264 mp4.
//
// The frames are handed to ffmpeg through a concat list rather than a glob or a
// numbered pattern, because archived names are wall-clock times with gaps in
// them — a pattern would need them renamed into a contiguous sequence first,
// which means copying several gigabytes to no purpose.
func (e *Encoder) Encode(ctx context.Context, frames []string, day time.Time, dst string) error {
	if len(frames) == 0 {
		return fmt.Errorf("timelapse: no frames to encode into %s", filepath.Base(dst))
	}

	// The logo is sized as a fraction of the output height, so the output height
	// has to be known before the filter chain is built. It comes from the first
	// frame's JPEG header rather than from ffprobe or an assumed 16:9: reading
	// the header is a few hundred bytes and no decode, and assuming the aspect
	// would put the crest at the wrong size the day the camera is replaced.
	outHeight := 0
	if e.Logo != "" || e.Font != "" {
		w, h, err := jpegBounds(frames[0])
		if err != nil {
			return err
		}
		outHeight = scaledHeight(e.width(), w, h)
	}

	entries := make([]concatEntry, len(frames))
	for i, f := range frames {
		entries[i] = concatEntry{Path: f}
		if e.Font == "" {
			continue
		}
		date, clock, err := stampFor(day, f)
		if err != nil {
			return err
		}
		entries[i].Date, entries[i].Clock = date, clock
	}

	list, cleanup, err := writeConcatList(entries, filepath.Dir(dst), "frames")
	if err != nil {
		return err
	}
	defer cleanup()

	return e.run(ctx, e.encodeArgs(list, dst, outHeight))
}

// jpegBounds reads an image's dimensions from its header.
func jpegBounds(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("timelapse: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("timelapse: reading the frame size from %s: %w", filepath.Base(path), err)
	}
	return cfg.Width, cfg.Height, nil
}

// scaledHeight is what scale=width:-2 will produce: the aspect preserved and
// rounded to an even number, which yuv420p requires.
func scaledHeight(outWidth, srcWidth, srcHeight int) int {
	if srcWidth <= 0 {
		return 0
	}
	h := int(float64(srcHeight)*float64(outWidth)/float64(srcWidth) + 0.5)
	h -= h % 2
	if h < 2 {
		h = 2
	}
	return h
}

// Join concatenates segments into dst without re-encoding.
func (e *Encoder) Join(ctx context.Context, segments []string, dst string) error {
	if len(segments) == 0 {
		return fmt.Errorf("timelapse: no segments to join into %s", filepath.Base(dst))
	}

	entries := make([]concatEntry, len(segments))
	for i, seg := range segments {
		entries[i] = concatEntry{Path: seg}
	}

	list, cleanup, err := writeConcatList(entries, filepath.Dir(dst), "segments")
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
func (e *Encoder) encodeArgs(list, dst string, outHeight int) []string {
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

	args := []string{
		"-y",
		"-loglevel", "warning",
		"-r", strconv.Itoa(e.fps()),
		"-f", "concat",
		"-safe", "0",
		"-i", list,
	}

	// Without a logo the chain is a plain -vf. With one there is a second input
	// to composite, which -vf cannot express.
	if e.Logo == "" {
		args = append(args, "-vf", e.filters(outHeight))
	} else {
		args = append(args,
			"-i", e.Logo,
			"-filter_complex", e.filterComplex(outHeight),
			"-map", "[out]",
		)
	}

	args = append(args,
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
	)
	return args
}

// filterComplex is the filter chain when there is a logo to composite.
//
// The crest is scaled to a fraction of the output height and pinned to the
// top-left corner, which is where theme.json already puts it on the live frame —
// a viewer moving between the still and the timelapse should not have to find
// the branding twice.
func (e *Encoder) filterComplex(outHeight int) string {
	logoH := int(float64(outHeight)*e.logoHeight() + 0.5)
	if logoH < 1 {
		logoH = 1
	}
	margin := int(float64(outHeight)*e.logoMargin() + 0.5)

	return strings.Join([]string{
		"[0:v]" + e.filters(outHeight) + "[base]",
		// -1 keeps the crest's own aspect, whatever shape the artwork is.
		fmt.Sprintf("[1:v]scale=-1:%d:flags=lanczos[logo]", logoH),
		fmt.Sprintf("[base][logo]overlay=%d:%d:format=auto,format=yuv420p[out]", margin, margin),
	}, ";")
}

// stamp is the clause that labels each frame, empty when nothing is being
// labelled.
//
// It is placed after the deflicker in the chain on purpose: deflicker equalises
// luminance across neighbouring frames, and text drawn before it would be dimmed
// and brightened along with the sky behind it.
//
// The backing box is drawn separately, at a fixed size, rather than letting
// drawtext wrap the text. The face has proportional digits, so the rendered
// width changes as the clock advances — a 1 is narrower than a 0 — and a box
// that hugs the text changes width with it. Anchored to the right, that makes
// its left edge twitch against open sky every time a digit changes. A box of
// constant size with the text left-aligned inside it holds still, and the slack
// absorbs whatever the digits do.
//
// It is also not decoration: this sky runs from near-white at midday to black
// overnight and no single text colour survives both, which is the same problem
// the overlay's credit line solves the same way.
func (e *Encoder) stamp(outWidth, outHeight int) string {
	if e.Font == "" {
		return ""
	}

	size := int(float64(outHeight)*e.stampHeight() + 0.5)
	if size < 1 {
		size = 1
	}
	margin := int(float64(outHeight)*e.stampMargin() + 0.5)
	pad := int(float64(size)*stampPadding + 0.5)

	// Sized for the longest label the format produces — "2026-08-30 14:30",
	// sixteen characters — with room to spare, so the widest combination of
	// digits still sits inside. Erring high costs a little empty space on the
	// right and nothing else; erring low would clip the label.
	boxW := int(float64(size)*stampAdvance*stampChars+0.5) + 2*pad
	boxH := size + 2*pad
	boxX := outWidth - boxW - margin

	return fmt.Sprintf(
		"drawbox=x=%d:y=%d:w=%d:h=%d:color=black@0.45:t=fill,"+
			"drawtext=fontfile=%s:text='%%{metadata\\:d} %%{metadata\\:t}'"+
			":x=%d:y=%d:fontsize=%d:fontcolor=white",
		boxX, margin, boxW, boxH,
		e.Font, boxX+pad, margin+pad, size,
	)
}

// filters is the video filter chain.
func (e *Encoder) filters(outHeight int) string {
	chain := []string{
		// Lanczos because this is a 3x downscale of detailed terrain, where the
		// default bilinear visibly softens ridgelines.
		fmt.Sprintf("scale=%d:-2:flags=lanczos", e.width()),
		// The camera meters every frame independently, ten minutes apart, so
		// consecutive pictures of the same scene differ in exposure by more than
		// the scene does. Without this the result strobes.
		"deflicker=mode=pm:size=10",
	}
	if s := e.stamp(e.width(), outHeight); s != "" {
		chain = append(chain, s)
	}
	return strings.Join(append(chain, "format=yuv420p"), ",")
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

// concatEntry is one input in a concat list, and what it should be labelled
// with. Date and Clock are empty for anything that carries no timestamp.
type concatEntry struct {
	Path  string
	Date  string
	Clock string
}

// writeConcatList writes an ffmpeg concat demuxer list next to the output.
func writeConcatList(entries []concatEntry, dir, kind string) (string, func(), error) {
	f, err := os.CreateTemp(dir, "."+kind+"-*.txt")
	if err != nil {
		return "", func() {}, fmt.Errorf("timelapse: concat list: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }

	var b strings.Builder
	for _, e := range entries {
		abs, err := filepath.Abs(e.Path)
		if err != nil {
			abs = e.Path
		}
		b.WriteString("file " + quoteConcatPath(abs) + "\n")

		// Attached to the packet rather than passed as a filter argument,
		// because the label differs per frame and a filter argument is fixed for
		// the whole run. drawtext reads it back with %{metadata:...}.
		//
		// The value ends at the first space, which is why the date and the clock
		// travel as two keys and are joined in the drawtext template.
		if e.Date != "" {
			b.WriteString("file_packet_metadata d=" + e.Date + "\n")
			b.WriteString("file_packet_metadata t=" + e.Clock + "\n")
		}
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
