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

// DefaultEnter and DefaultExit are the shipped thresholds, kept here so the
// daemon and the preview harness cannot drift apart on what night means.
//
// They come from measuring real stills off this camera: night frames land near
// 10 and daylight frames above 100, so the band sits in the empty middle with
// roughly a factor of three of margin at each edge.
const (
	DefaultEnter = 35.0
	DefaultExit  = 55.0
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

// Observe feeds in one measurement and reports the state, and whether this
// measurement changed it.
//
// The first observation always reports changed, so a daemon starting up at 3am
// logs that it is starting into the night rather than silently assuming day.
func (d *Detector) Observe(luma float64) (night, changed bool) {
	if d == nil {
		return false, false
	}
	was, first := d.night, !d.started
	d.started = true

	switch {
	case !d.night && luma <= d.Enter:
		d.night = true
	case d.night && luma >= d.Exit:
		d.night = false
	}
	return d.night, first || d.night != was
}
