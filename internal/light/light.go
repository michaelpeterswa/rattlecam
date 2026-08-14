// Package light decides whether a frame is a daytime picture or a night one,
// from the frame itself.
//
// The signal is the image rather than a clock or a sun-position calculation
// because the question being asked is not "has the sun set" but "can anything
// drawn in black still be seen". Overcast, smoke and terrain shadow all move
// that answer around hours either side of sunset, and the camera's own exposure
// moves it again. Measuring the picture answers it directly, and keeps working
// when the weather station or Influx does not.
package light

import (
	"image"
	"math"
)

// Rec. 601 luma coefficients — the same weighting JPEG uses, which is why the
// Y plane of a decoded JPEG can be read as luma with no conversion at all.
const (
	rWeight = 0.299
	gWeight = 0.587
	bWeight = 0.114
)

// sampleStride is how coarsely a large image is sampled. Mean brightness is a
// low-frequency property, so a full 4K scan would cost 8 million reads to
// produce the same number as a 130,000-pixel grid.
//
// Small images are sampled whole: the stride exists to bound the cost on real
// frames, and there is nothing to bound on a thumbnail.
const sampleStride = 8

func stride(n int) int {
	if n < 64 {
		return 1
	}
	return sampleStride
}

// MonoThreshold is the mean chroma below which a frame is considered greyscale.
//
// A camera in IR mode emits Cb and Cr pinned to 128, so the measurement is
// exactly 0 rather than merely small — observed stepping from 11.35 to 0.00
// between two consecutive frames at dusk. The threshold is well above that only
// to tolerate a camera whose night mode leaves a slight tint; ordinary daylight
// on this site sits above 13 even in the last minutes of civil twilight.
const MonoThreshold = 2.0

// MeanChroma returns how far a frame's colour sits from neutral, as the mean of
// |Cb-128| + |Cr-128| on a 0-255 scale. It reports -1 for an image whose colour
// model cannot answer the question.
//
// This is the better night signal on a camera with an IR-cut filter, and the
// reason is that it is a step rather than a slope. Luma falls smoothly through
// dusk and its night value depends on the moon, cloud and snow — measured
// between 10 and 42 on different nights here, which straddles any threshold you
// would pick. The filter swinging out is a single unambiguous event, and it is
// the camera's own judgement that there is no longer enough light, which is very
// nearly the question being asked.
func MeanChroma(img image.Image) float64 {
	y, ok := img.(*image.YCbCr)
	if !ok {
		return -1
	}
	b := y.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return -1
	}
	sx, sy := stride(b.Dx()), stride(b.Dy())

	var sum float64
	var n int
	for py := b.Min.Y; py < b.Max.Y; py += sy {
		for px := b.Min.X; px < b.Max.X; px += sx {
			o := y.COffset(px, py)
			sum += math.Abs(float64(y.Cb[o])-128) + math.Abs(float64(y.Cr[o])-128)
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return sum / float64(n)
}

// Mono reports whether a frame is greyscale, and whether that could be
// determined at all.
//
// Two things count as greyscale, because two things produce it. This camera in
// IR mode sends an ordinary three-channel JPEG with Cb and Cr pinned to neutral,
// which decodes to YCbCr and is caught by the chroma measurement. Anything that
// re-encodes such a frame is liable to notice the chroma is redundant and emit a
// true single-channel JPEG instead, which decodes to image.Gray and carries no
// colour information at all — unambiguously mono, and it would be perverse to
// answer "unknown" for the one image type that cannot possibly be in colour.
func Mono(img image.Image) (mono, ok bool) {
	if _, isGray := img.(*image.Gray); isGray {
		return true, true
	}
	c := MeanChroma(img)
	if c < 0 {
		return false, false
	}
	return c < MonoThreshold, true
}

// MeanLuma returns the mean luma of img on a 0-255 scale. An empty image is 0.
func MeanLuma(img image.Image) float64 {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return 0
	}
	sx, sy := stride(b.Dx()), stride(b.Dy())

	var sum float64
	var n int

	switch src := img.(type) {
	case *image.YCbCr:
		// The path that actually runs: image/jpeg decodes to YCbCr, and its Y
		// plane is already full-range Rec. 601 luma. No colour conversion, no
		// interface call per pixel.
		for y := b.Min.Y; y < b.Max.Y; y += sy {
			row := src.Y[src.YOffset(b.Min.X, y):]
			for x := 0; x < b.Dx(); x += sx {
				sum += float64(row[x])
				n++
			}
		}
	case *image.Gray:
		for y := b.Min.Y; y < b.Max.Y; y += sy {
			row := src.Pix[src.PixOffset(b.Min.X, y):]
			for x := 0; x < b.Dx(); x += sx {
				sum += float64(row[x])
				n++
			}
		}
	default:
		for y := b.Min.Y; y < b.Max.Y; y += sy {
			for x := b.Min.X; x < b.Max.X; x += sx {
				// RGBA returns 16-bit alpha-premultiplied values. Premultiplied
				// is what we want: a transparent pixel contributes nothing,
				// which is the right reading of "how much light is here".
				r, g, bl, _ := img.At(x, y).RGBA()
				sum += (rWeight*float64(r) + gWeight*float64(g) + bWeight*float64(bl)) / 257
				n++
			}
		}
	}

	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// DefaultEnter and DefaultExit are the shipped luma thresholds, kept here so the
// daemon and the preview harness cannot drift apart on what night means.
//
// These are the fallback, not the primary signal — see Observe. They are set
// from a full dusk sampled off this camera: daylight held 129 through the
// afternoon and 94 at sunset, the last colour frame read 64, and the first
// greyscale frames ran 66 down to 42 and still falling. A band of 50 to 75
// therefore separates a frame that is definitely dark from one that is
// definitely not, while leaving the ambiguous middle to hysteresis.
//
// The margin here is thinner than the mono signal's, which is the whole reason
// mono leads: night luma has been observed anywhere from 10 to 42 depending on
// moon and cloud, and a heavily overcast winter noon can approach the top of
// this band from the other side.
const (
	DefaultEnter = 50.0
	DefaultExit  = 75.0
)

// Detector converts a stream of luma measurements into a night flag.
//
// The two thresholds are the whole point. Dusk moves the measurement slowly
// through the range, and a single threshold would sit on the boundary flipping
// the frame's treatment back and forth every poll — visible to anyone watching,
// and recorded in the archive as a flicker. A band means the state changes once
// on the way down and once on the way back up.
type Detector struct {
	// Enter is the luma at or below which night begins, Exit the luma at or
	// above which day resumes. Exit must be the larger of the two; a Detector
	// built the other way round would latch on its first observation.
	Enter, Exit float64

	night   bool
	started bool
}

// Night reports the current state without observing anything.
func (d *Detector) Night() bool {
	if d == nil {
		return false
	}
	return d.night
}

// Reading is what one frame says about the light.
type Reading struct {
	Luma float64 // mean luma, 0-255
	Mono bool    // the frame is greyscale
	// MonoKnown is false when the frame's colour model could not answer, which
	// is the only case where luma decides on its own.
	MonoKnown bool
}

// Measure takes both readings off a decoded frame.
func Measure(img image.Image) Reading {
	mono, ok := Mono(img)
	return Reading{Luma: MeanLuma(img), Mono: mono, MonoKnown: ok}
}

// Observe feeds in one frame's reading and reports the state, and whether this
// reading changed it.
//
// Either signal on its own is enough to call it night; both must say otherwise
// for it to be day.
//
// A greyscale frame settles it immediately. The IR filter swinging out is a
// single unambiguous event and it is the camera's own judgement that the light
// has gone, which is very nearly the question being asked. No hysteresis is
// needed against a signal that cannot sit near its own boundary, and the camera
// has its own dwell before it switches.
//
// But a colour frame does not settle it, which is the mistake worth not making.
// A frame can be in colour and still be far too dark for black ink — a camera
// held in colour mode, or one whose switch has not fired yet. Real evidence of
// that is in the fixtures: a colour frame measuring 10.5 luma, which no viewer
// would call daylight. So luma still gets its say whenever the answer is not
// already mono, and only a frame that is both in colour and bright enough
// returns to day.
//
// The first observation always reports changed, so a daemon starting up at 3am
// logs that it is starting into the night rather than silently assuming day.
func (d *Detector) Observe(r Reading) (night, changed bool) {
	if d == nil {
		return false, false
	}
	was, first := d.night, !d.started
	d.started = true

	switch {
	case r.MonoKnown && r.Mono:
		d.night = true
	case !d.night && r.Luma <= d.Enter:
		d.night = true
	case d.night && r.Luma >= d.Exit:
		d.night = false
	}
	return d.night, first || d.night != was
}
