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

// lumaOnly is a reading from a frame whose colour model said nothing, which is
// the only case where the luma band decides. The Detector tests below are all
// about that fallback; the mono path has its own.
func lumaOnly(luma float64) Reading {
	return Reading{Luma: luma, MonoKnown: false}
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
	d := &Detector{Enter: 50, Exit: 75}

	if night, changed := d.Observe(lumaOnly(120)); night || !changed {
		t.Errorf("first daylight observation: night=%v changed=%v, want false/true", night, changed)
	}
	if night, changed := d.Observe(lumaOnly(110)); night || changed {
		t.Errorf("steady daylight: night=%v changed=%v, want false/false", night, changed)
	}
	if night, changed := d.Observe(lumaOnly(10)); !night || !changed {
		t.Errorf("dark frame: night=%v changed=%v, want true/true", night, changed)
	}
	if night, changed := d.Observe(lumaOnly(120)); night || !changed {
		t.Errorf("back to daylight: night=%v changed=%v, want false/true", night, changed)
	}
}

// The reason the band exists. Dusk drifts the measurement through the middle of
// the range; if that flipped the state, the treatment would flicker every poll.
func TestDetectorDoesNotFlapInsideTheBand(t *testing.T) {
	d := &Detector{Enter: 50, Exit: 75}
	d.Observe(lumaOnly(120)) // establish day

	for _, luma := range []float64{74, 60, 51, 65, 74, 51, 70} {
		if night, changed := d.Observe(lumaOnly(luma)); night || changed {
			t.Fatalf("luma %.0f inside the band flipped the state (night=%v changed=%v)", luma, night, changed)
		}
	}

	d.Observe(lumaOnly(10)) // now night
	for _, luma := range []float64{51, 70, 74, 60, 74} {
		if night, changed := d.Observe(lumaOnly(luma)); !night || changed {
			t.Fatalf("luma %.0f inside the band left night (night=%v changed=%v)", luma, night, changed)
		}
	}
}

// Starting up after dark must report night immediately, not wait for a
// transition that already happened hours ago.
func TestDetectorStartingIntoTheNight(t *testing.T) {
	d := &Detector{Enter: 50, Exit: 75}
	if night, changed := d.Observe(lumaOnly(10)); !night || !changed {
		t.Errorf("night=%v changed=%v, want true/true", night, changed)
	}
	if d.Night() != true {
		t.Error("Night() disagrees with Observe()")
	}
}

func TestDetectorThresholdsAreInclusive(t *testing.T) {
	d := &Detector{Enter: 50, Exit: 75}
	d.Observe(lumaOnly(120))
	if night, _ := d.Observe(lumaOnly(50)); !night {
		t.Error("luma exactly at Enter did not begin night")
	}
	if night, _ := d.Observe(lumaOnly(75)); night {
		t.Error("luma exactly at Exit did not end night")
	}
}

func TestNilDetector(t *testing.T) {
	var d *Detector
	if night, changed := d.Observe(Reading{}); night || changed {
		t.Errorf("nil Detector: night=%v changed=%v, want false/false", night, changed)
	}
	if d.Night() {
		t.Error("nil Detector reported night")
	}
}

// The fixtures are real frames off this camera, and between them they cover
// both signals and the boundary between them. See testdata/README.md.
func TestRealStillsClassifyCorrectly(t *testing.T) {
	for _, tc := range []struct {
		file     string
		wantLuma float64
		wantMono bool
		night    bool
		why      string
	}{
		{"day.jpg", 124.9, false, false, "clear afternoon"},
		{"valley.jpg", 107.0, false, false, "dimmest daylight on hand"},
		{"smoke.jpg", 144.1, false, false, "wildfire haze, the brightest"},
		{"dusk-colour.jpg", 64.2, false, false, "last colour frame before the IR switch"},
		{"dusk-mono.jpg", 66.2, true, true, "first greyscale frame — nearly the same luma as the one before it"},
		{"night.jpg", 10.5, false, true, "colour, but far too dark for black ink"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			img := loadFixture(t, tc.file)
			r := Measure(img)

			closeTo(t, r.Luma, tc.wantLuma, 2, "luma")
			if !r.MonoKnown {
				t.Fatalf("could not determine colour for %s; it is a JPEG and should always be answerable", tc.file)
			}
			if r.Mono != tc.wantMono {
				t.Errorf("mono = %v, want %v (chroma %.2f) — %s", r.Mono, tc.wantMono, MeanChroma(img), tc.why)
			}

			d := &Detector{Enter: DefaultEnter, Exit: DefaultExit}
			if night, _ := d.Observe(r); night != tc.night {
				t.Errorf("classified night=%v, want %v (luma %.1f, mono %v) — %s",
					night, tc.night, r.Luma, r.Mono, tc.why)
			}
		})
	}
}

// The pair either side of tonight's real IR switch, ten minutes apart. Their
// luma is almost identical — 64.2 and 66.2 — so nothing keyed on brightness
// could separate them, and the earlier frame is the *darker* of the two. This is
// the case that made luma the fallback rather than the signal.
func TestTheDuskSwitchIsInvisibleToLuma(t *testing.T) {
	before := Measure(loadFixture(t, "dusk-colour.jpg"))
	after := Measure(loadFixture(t, "dusk-mono.jpg"))

	if math.Abs(before.Luma-after.Luma) > 5 {
		t.Fatalf("the fixtures no longer straddle the switch at comparable brightness: %.1f then %.1f",
			before.Luma, after.Luma)
	}
	if before.Mono || !after.Mono {
		t.Fatalf("mono did not flip across the switch: before=%v after=%v", before.Mono, after.Mono)
	}

	d := &Detector{Enter: DefaultEnter, Exit: DefaultExit}
	if night, _ := d.Observe(before); night {
		t.Error("the last colour frame was called night")
	}
	if night, changed := d.Observe(after); !night || !changed {
		t.Errorf("the first greyscale frame did not flip the state: night=%v changed=%v", night, changed)
	}
}

// A single-channel JPEG carries no colour at all, so answering "unknown" for it
// would push the one unambiguous case onto the fallback.
func TestGrayImagesAreMono(t *testing.T) {
	mono, ok := Mono(image.NewGray(image.Rect(0, 0, 8, 8)))
	if !ok || !mono {
		t.Errorf("Mono(*image.Gray) = %v, %v; want true, true", mono, ok)
	}
}

// Anything that is not a decoded JPEG cannot answer, and must say so rather than
// guessing "colour" — which would read as daylight.
func TestUnknownColourModelDefersToLuma(t *testing.T) {
	mono, ok := Mono(image.NewRGBA(image.Rect(0, 0, 8, 8)))
	if ok || mono {
		t.Errorf("Mono(*image.RGBA) = %v, %v; want false, false", mono, ok)
	}

	d := &Detector{Enter: DefaultEnter, Exit: DefaultExit}
	if night, _ := d.Observe(Reading{Luma: 10, MonoKnown: false}); !night {
		t.Error("a dark frame of unknown colour was not called night")
	}
	if night, _ := d.Observe(Reading{Luma: 120, MonoKnown: false}); night {
		t.Error("a bright frame of unknown colour stayed night")
	}
}

// Colour alone must not restore day while the frame is still too dark, or the
// morning switch back would drop the treatment before the picture can carry it.
func TestColourDoesNotRestoreDayWhileDark(t *testing.T) {
	d := &Detector{Enter: DefaultEnter, Exit: DefaultExit}
	d.Observe(Reading{Mono: true, MonoKnown: true}) // night

	if night, _ := d.Observe(Reading{Luma: 60, Mono: false, MonoKnown: true}); !night {
		t.Error("a dim colour frame ended night before it cleared the exit threshold")
	}
	if night, _ := d.Observe(Reading{Luma: 90, Mono: false, MonoKnown: true}); night {
		t.Error("a bright colour frame did not end night")
	}
}

func loadFixture(t *testing.T, name string) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only

	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return img
}
