package light

import (
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func closeTo(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.2f, want %.2f (±%.2f)", what, got, want, tol)
	}
}

func TestMeanLumaOfSolidColours(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    color.Color
		want float64
	}{
		{"black", color.RGBA{0, 0, 0, 255}, 0},
		{"white", color.RGBA{255, 255, 255, 255}, 255},
		{"mid grey", color.RGBA{128, 128, 128, 255}, 128},
		// Rec. 601 weights the channels unequally, so a saturated primary is
		// nowhere near its own 8-bit value.
		{"pure red", color.RGBA{255, 0, 0, 255}, 255 * rWeight},
		{"pure green", color.RGBA{0, 255, 0, 255}, 255 * gWeight},
		{"pure blue", color.RGBA{0, 0, 255, 255}, 255 * bWeight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeTo(t, MeanLuma(solid(32, 32, tc.c)), tc.want, 1, "MeanLuma")
		})
	}
}

// Half black, half white must average to mid grey — a mean that ignored half
// the image, or double-counted a row, would not land here.
func TestMeanLumaAveragesAcrossTheFrame(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			v := uint8(0)
			if y >= 16 {
				v = 255
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	closeTo(t, MeanLuma(img), 127.5, 1, "MeanLuma")
}

// The YCbCr path is the one that runs in production, since image/jpeg decodes
// to it. It must agree with the generic path rather than being a separate
// implementation that quietly drifts.
func TestMeanLumaYCbCrMatchesGeneric(t *testing.T) {
	const w, h = 96, 96
	yuv := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio444)
	gray := image.NewGray(image.Rect(0, 0, w, h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			v := uint8((x * 255) / (w - 1)) // horizontal ramp
			yuv.Y[yuv.YOffset(x, y)] = v
			yuv.Cb[yuv.COffset(x, y)] = 128
			yuv.Cr[yuv.COffset(x, y)] = 128
			gray.SetGray(x, y, color.Gray{Y: v})
			rgba.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}

	want := MeanLuma(rgba)
	closeTo(t, MeanLuma(yuv), want, 1.5, "MeanLuma(YCbCr)")
	closeTo(t, MeanLuma(gray), want, 0.5, "MeanLuma(Gray)")
}

// Bounds not anchored at the origin are what a sub-image looks like, and an
// offset mishandled in the row-offset arithmetic would read the wrong pixels
// rather than fail outright.
func TestMeanLumaHonoursNonZeroOrigin(t *testing.T) {
	full := image.NewGray(image.Rect(0, 0, 128, 128))
	for y := range 128 {
		for x := range 128 {
			v := uint8(0)
			if x >= 64 {
				v = 200
			}
			full.SetGray(x, y, color.Gray{Y: v})
		}
	}
	right := full.SubImage(image.Rect(64, 0, 128, 128))
	closeTo(t, MeanLuma(right), 200, 0.5, "MeanLuma of the bright half")
}

func TestMeanLumaOfNothing(t *testing.T) {
	if got := MeanLuma(nil); got != 0 {
		t.Errorf("MeanLuma(nil) = %v, want 0", got)
	}
	if got := MeanLuma(image.NewRGBA(image.Rect(0, 0, 0, 0))); got != 0 {
		t.Errorf("MeanLuma(empty) = %v, want 0", got)
	}
}

func TestDetectorEntersAndLeavesNight(t *testing.T) {
	d := &Detector{Enter: 35, Exit: 55}

	if night, changed := d.Observe(120); night || !changed {
		t.Errorf("first daylight observation: night=%v changed=%v, want false/true", night, changed)
	}
	if night, changed := d.Observe(110); night || changed {
		t.Errorf("steady daylight: night=%v changed=%v, want false/false", night, changed)
	}
	if night, changed := d.Observe(10); !night || !changed {
		t.Errorf("dark frame: night=%v changed=%v, want true/true", night, changed)
	}
	if night, changed := d.Observe(120); night || !changed {
		t.Errorf("back to daylight: night=%v changed=%v, want false/true", night, changed)
	}
}

// The reason the band exists. Dusk drifts the measurement through the middle of
// the range; if that flipped the state, the treatment would flicker every poll.
func TestDetectorDoesNotFlapInsideTheBand(t *testing.T) {
	d := &Detector{Enter: 35, Exit: 55}
	d.Observe(120) // establish day

	for _, luma := range []float64{54, 40, 36, 45, 54, 36, 50} {
		if night, changed := d.Observe(luma); night || changed {
			t.Fatalf("luma %.0f inside the band flipped the state (night=%v changed=%v)", luma, night, changed)
		}
	}

	d.Observe(10) // now night
	for _, luma := range []float64{36, 50, 54, 40, 54} {
		if night, changed := d.Observe(luma); !night || changed {
			t.Fatalf("luma %.0f inside the band left night (night=%v changed=%v)", luma, night, changed)
		}
	}
}

// Starting up after dark must report night immediately, not wait for a
// transition that already happened hours ago.
func TestDetectorStartingIntoTheNight(t *testing.T) {
	d := &Detector{Enter: 35, Exit: 55}
	if night, changed := d.Observe(10); !night || !changed {
		t.Errorf("night=%v changed=%v, want true/true", night, changed)
	}
	if d.Night() != true {
		t.Error("Night() disagrees with Observe()")
	}
}

func TestDetectorThresholdsAreInclusive(t *testing.T) {
	d := &Detector{Enter: 35, Exit: 55}
	d.Observe(120)
	if night, _ := d.Observe(35); !night {
		t.Error("luma exactly at Enter did not begin night")
	}
	if night, _ := d.Observe(55); night {
		t.Error("luma exactly at Exit did not end night")
	}
}

func TestNilDetector(t *testing.T) {
	var d *Detector
	if night, changed := d.Observe(0); night || changed {
		t.Errorf("nil Detector: night=%v changed=%v, want false/false", night, changed)
	}
	if d.Night() {
		t.Error("nil Detector reported night")
	}
}

// The thresholds are only defensible if real frames actually sit either side of
// them, so this measures real frames from the camera the daemon runs on.
//
// The fixtures are 320x180 JPEGs downscaled from the 4K originals, which the
// repo does not carry. Mean luma survives downscaling — each fixture measures
// within 0.9 of its original — and JPEG keeps them on the YCbCr path production
// actually uses. See testdata/README.md.
func TestRealStillsFallOnTheExpectedSideOfTheThresholds(t *testing.T) {
	for _, tc := range []struct {
		file  string
		want  float64 // measured from the 4K original
		night bool
	}{
		{"day.jpg", 124.9, false},
		{"valley.jpg", 107.0, false},
		{"smoke.jpg", 144.9, false},
		{"night.jpg", 10.6, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck // read-only

			img, _, err := image.Decode(f)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			luma := MeanLuma(img)

			// A drift here means the fixture no longer stands in for the frame it
			// was cut from, which would quietly hollow out the checks below.
			closeTo(t, luma, tc.want, 2, "MeanLuma")

			d := &Detector{Enter: DefaultEnter, Exit: DefaultExit}
			night, _ := d.Observe(luma)
			if night != tc.night {
				t.Errorf("%s measured %.1f and was classified night=%v, want night=%v (band %.0f..%.0f)",
					tc.file, luma, night, tc.night, DefaultEnter, DefaultExit)
			}
			// Daylight must also clear the upper threshold, or a frame like this
			// could enter night and never get back out of it.
			if !tc.night && luma < DefaultExit {
				t.Errorf("%s measured %.1f, below the exit threshold of %.0f: it would not restore day",
					tc.file, luma, DefaultExit)
			}
		})
	}
}

// The margin is the reason these thresholds are safe to leave unattended. If a
// change ever narrows it, that should be a deliberate decision rather than
// something noticed the first time dusk misbehaves.
//
// The tightest real case is the dimmest daylight frame against the exit
// threshold, at about 1.95x — daylight clears the enter threshold by a much
// wider 3x, but it is getting back *out* of night that the exit threshold
// governs, so that is the one to guard.
func TestRealStillsKeepAWideMarginFromTheBand(t *testing.T) {
	const wantRatio = 1.75

	for _, tc := range []struct {
		file      string
		threshold float64
		night     bool
	}{
		{"day.jpg", DefaultExit, false},
		{"valley.jpg", DefaultExit, false},
		{"night.jpg", DefaultEnter, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck // read-only

			img, _, err := image.Decode(f)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			luma := MeanLuma(img)

			ratio := luma / tc.threshold
			if tc.night {
				ratio = tc.threshold / luma
			}
			if ratio < wantRatio {
				t.Errorf("%s measures %.1f against a threshold of %.0f — only %.1fx margin, want at least %.1fx",
					tc.file, luma, tc.threshold, ratio, wantRatio)
			}
		})
	}
}
