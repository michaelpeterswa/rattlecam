package overlay

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRenderer builds a renderer over throwaway assets. Fonts are required, so
// the caller supplies a path to a real TTF via RATTLECAM_TEST_FONT or the test
// is skipped — there is no font in the repo by design.
func testRenderer(t *testing.T, theme Theme, withLogo bool) *Renderer {
	t.Helper()

	font := os.Getenv("RATTLECAM_TEST_FONT")
	if font == "" {
		for _, c := range []string{
			"/System/Library/Fonts/Supplemental/Arial Narrow.ttf",
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"../../assets/font.ttf",
		} {
			if _, err := os.Stat(c); err == nil {
				font = c
				break
			}
		}
	}
	if font == "" {
		t.Skip("no TrueType font available; set RATTLECAM_TEST_FONT")
	}

	logoPath := ""
	if withLogo {
		// A tall portrait mark, the shape that forced corner placement.
		logo := image.NewRGBA(image.Rect(0, 0, 80, 150))
		for y := range 150 {
			for x := range 80 {
				logo.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
		logoPath = filepath.Join(t.TempDir(), "logo.png")
		f, err := os.Create(logoPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, logo); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	r, err := NewRenderer(font, font, logoPath, theme)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func srcImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := range 360 {
		for x := range 640 {
			img.Set(x, y, color.RGBA{R: 40, G: 90, B: 160, A: 255})
		}
	}
	return img
}

func sampleFrame() Frame {
	return Frame{
		SiteName:   "Valley View",
		Conditions: "Partly Cloudy",
		CapturedAt: time.Date(2026, 8, 7, 19, 57, 0, 0, time.UTC),
		Credit:     "This view is provided by RSVU",
		Fields: []Field{
			{Label: "TEMP", Value: "65°F"},
			{Label: "WIND", Value: "SW 7 mph"},
		},
	}
}

func TestRenderProducesRGBA(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	img, err := r.Render(srcImage(), sampleFrame())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The contact sheet asserts this concrete type.
	if _, ok := img.(*image.RGBA); !ok {
		t.Errorf("Render returned %T, want *image.RGBA", img)
	}
	if got, want := img.Bounds(), srcImage().Bounds(); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
}

func TestRenderEveryCornerPlacement(t *testing.T) {
	for _, corner := range []string{"top-left", "top-right", "bottom-left", "bottom-right"} {
		th := DefaultTheme()
		th.LogoPlacement = corner
		r := testRenderer(t, th, true)
		if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
			t.Errorf("Render with placement %q: %v", corner, err)
		}
	}
}

// A typo in the theme file should say so rather than silently dropping the mark.
func TestRenderUnknownPlacementErrors(t *testing.T) {
	th := DefaultTheme()
	th.LogoPlacement = "middle"
	r := testRenderer(t, th, true)

	_, err := r.Render(srcImage(), sampleFrame())
	if err == nil {
		t.Fatal("want an error for an unknown logo_placement")
	}
	if !strings.Contains(err.Error(), "middle") {
		t.Errorf("error %q does not quote the offending value", err)
	}
}

func TestRenderLogoPlacementNoneAndBar(t *testing.T) {
	for _, p := range []string{"none", "bar", ""} {
		th := DefaultTheme()
		th.LogoPlacement = p
		r := testRenderer(t, th, true)
		if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
			t.Errorf("Render with placement %q: %v", p, err)
		}
	}
}

// The badge must land inside the frame at every corner, at 4K as well as at the
// resolution the theme was tuned against — placement is a ratio like everything
// else. A badge drawn partly off-frame is silently cropped, so this checks the
// arithmetic rather than the pixels.
func TestBadgeStaysInsideFrame(t *testing.T) {
	th := DefaultTheme()
	th.LogoHeight = 0.20
	th.LogoMargin = 0.025

	for _, size := range []image.Point{{X: 1920, Y: 1080}, {X: 3840, Y: 2160}} {
		w, h := float64(size.X), float64(size.Y)
		targetH := h * th.LogoHeight
		targetW := targetH * 80 / 150 // the test mark's aspect
		margin := h * th.LogoMargin

		for _, c := range []struct {
			name string
			x, y float64
		}{
			{"top-left", margin, margin},
			{"top-right", w - margin - targetW, margin},
			{"bottom-left", margin, h - margin - targetH},
			{"bottom-right", w - margin - targetW, h - margin - targetH},
		} {
			if c.x < 0 || c.y < 0 || c.x+targetW > w || c.y+targetH > h {
				t.Errorf("%dx%d %s: badge at (%.0f,%.0f)+%.0fx%.0f falls outside the frame",
					size.X, size.Y, c.name, c.x, c.y, targetW, targetH)
			}
		}
	}
}

func TestRenderCreditPlacements(t *testing.T) {
	for _, p := range []string{"bar", "top-center", "none", ""} {
		th := DefaultTheme()
		th.CreditPlacement = p
		r := testRenderer(t, th, true)
		if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
			t.Errorf("Render with credit_placement %q: %v", p, err)
		}
	}
}

func TestRenderUnknownCreditPlacementErrors(t *testing.T) {
	th := DefaultTheme()
	th.CreditPlacement = "left"
	r := testRenderer(t, th, true)

	_, err := r.Render(srcImage(), sampleFrame())
	if err == nil {
		t.Fatal("want an error for an unknown credit_placement")
	}
	if !strings.Contains(err.Error(), "left") {
		t.Errorf("error %q does not quote the offending value", err)
	}
}

// An unknown placement is a theme typo, but an empty credit means the operator
// simply did not set one — that must stay silent.
func TestUnknownCreditPlacementIgnoredWhenNoCredit(t *testing.T) {
	th := DefaultTheme()
	th.CreditPlacement = "left"
	r := testRenderer(t, th, true)

	f := sampleFrame()
	f.Credit = ""
	if _, err := r.Render(srcImage(), f); err != nil {
		t.Errorf("Render with no credit should not validate placement: %v", err)
	}
}

// The top-center box must clear the bar and stay on-frame at both resolutions.
func TestCreditBoxSitsInTheSky(t *testing.T) {
	th := DefaultTheme()

	for _, size := range []image.Point{{X: 1920, Y: 1080}, {X: 3840, Y: 2160}} {
		h := float64(size.Y)
		boxTop := h * th.CreditMargin
		// Generous over-estimate of the box height from text size and padding.
		boxBottom := boxTop + h*th.CreditTopSize*(1+2*th.CreditBoxPad)
		barTop := h - h*th.BarHeight

		if boxTop < 0 {
			t.Errorf("%dx%d: credit box starts above the frame", size.X, size.Y)
		}
		if boxBottom >= barTop {
			t.Errorf("%dx%d: credit box (bottom %.0f) reaches the bar (top %.0f)",
				size.X, size.Y, boxBottom, barTop)
		}
	}
}

// Stale and offline frames carry no fields but must still render.
func TestRenderWithNoFields(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	f := sampleFrame()
	f.Fields = nil

	if _, err := r.Render(srcImage(), f); err != nil {
		t.Fatalf("Render with no fields: %v", err)
	}
}

func TestRenderWithoutCreditOrStamp(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)

	f := sampleFrame()
	f.Credit = ""
	if _, err := r.Render(srcImage(), f); err != nil {
		t.Errorf("Render without credit: %v", err)
	}

	f = sampleFrame()
	f.CapturedAt = time.Time{}
	if _, err := r.Render(srcImage(), f); err != nil {
		t.Errorf("Render without timestamp: %v", err)
	}

	f = sampleFrame()
	f.Credit, f.CapturedAt = "", time.Time{}
	if _, err := r.Render(srcImage(), f); err != nil {
		t.Errorf("Render with neither: %v", err)
	}
}

func TestRenderWithoutLogo(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), false)
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
		t.Fatalf("Render with no logo: %v", err)
	}
}

// writeAnnotation writes a translucent full-frame layer of the given size.
func writeAnnotation(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if y == h/2 { // a single horizon line
				img.Set(x, y, color.RGBA{R: 90, G: 90, B: 90, A: 90})
			}
		}
	}
	path := filepath.Join(t.TempDir(), "annotation.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func TestAnnotationRenders(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 640, 360)); err != nil {
		t.Fatalf("LoadAnnotation: %v", err)
	}
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

// The layer may be authored at a different resolution than the camera delivers,
// so long as the shape matches.
func TestAnnotationScalesToFrame(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 3840, 2160)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil { // src is 640x360
		t.Fatalf("Render with a 4K annotation over a 640x360 frame: %v", err)
	}
}

// A registered overlay at the wrong aspect would slide every label off its peak,
// which is a wrong caption on a public frame. It must fail, not stretch.
func TestAnnotationAspectMismatchFails(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 400, 400)); err != nil {
		t.Fatal(err)
	}
	_, err := r.Render(srcImage(), sampleFrame())
	if err == nil {
		t.Fatal("want an error for a square annotation over a 16:9 frame")
	}
	for _, want := range []string{"aspect", "400x400"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Small authoring differences should not trip the guard.
func TestAnnotationToleratesRounding(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	// 640x360 is 1.7778; 1920x1081 is 1.7761, inside a percent.
	if err := r.LoadAnnotation(writeAnnotation(t, 1920, 1081)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
		t.Errorf("a sub-percent aspect difference should be tolerated: %v", err)
	}
}

func TestAnnotationOpacityZeroSkipsLayer(t *testing.T) {
	th := DefaultTheme()
	th.AnnotationOpacity = 0
	r := testRenderer(t, th, true)
	// Deliberately the wrong shape: at zero opacity it is never consulted.
	if err := r.LoadAnnotation(writeAnnotation(t, 400, 400)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
		t.Errorf("opacity 0 should skip the layer entirely: %v", err)
	}
}

// The layer is identical on every frame; resampling 4K each time would be waste.
func TestAnnotationCachedAcrossFrames(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 1920, 1080)); err != nil {
		t.Fatal(err)
	}
	b := srcImage().Bounds()

	first, err := r.scaledAnnotation(b, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.scaledAnnotation(b, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("annotation was rescaled for an identical frame")
	}

	// A different strength must invalidate it.
	third, err := r.scaledAnnotation(b, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Error("changed opacity reused the previous layer")
	}
}

// SetTheme is called per request in the harness; an opacity edit has to take.
func TestSetThemeInvalidatesAnnotationCache(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 1920, 1080)); err != nil {
		t.Fatal(err)
	}
	b := srcImage().Bounds()
	if _, err := r.scaledAnnotation(b, 1); err != nil {
		t.Fatal(err)
	}

	th := DefaultTheme()
	th.AnnotationOpacity = 0.4
	r.SetTheme(th)

	if r.annotationCache != nil {
		t.Error("SetTheme with a new opacity left the old layer cached")
	}
}

// Opacity above 1 strengthens a faint layer; nothing may wrap around.
func TestAnnotationOpacityAboveOneClamps(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), true)
	if err := r.LoadAnnotation(writeAnnotation(t, 640, 360)); err != nil {
		t.Fatal(err)
	}
	layer, err := r.scaledAnnotation(srcImage().Bounds(), 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(layer.Pix); i += 4 {
		a := layer.Pix[i+3]
		for c := range 3 {
			if layer.Pix[i+c] > a {
				t.Fatalf("premultiplied channel %d (%d) exceeds alpha (%d)", c, layer.Pix[i+c], a)
			}
		}
	}
}

func TestLoadAnnotationErrors(t *testing.T) {
	r := testRenderer(t, DefaultTheme(), false)

	// The daemon distinguishes "no file at the default path" from a real
	// misconfiguration, so the wrapped error must stay inspectable.
	err := r.LoadAnnotation(filepath.Join(t.TempDir(), "missing.png"))
	if err == nil {
		t.Fatal("want an error for a missing annotation")
	}
	// Must be errors.Is, not os.IsNotExist: the latter does not unwrap, and the
	// callers rely on this staying inspectable through the fmt.Errorf wrapper.
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not unwrap to os.ErrNotExist", err)
	}

	// An empty path clears rather than errors.
	if err := r.LoadAnnotation(""); err != nil {
		t.Errorf("clearing the annotation: %v", err)
	}
	if _, err := r.Render(srcImage(), sampleFrame()); err != nil {
		t.Errorf("Render after clearing: %v", err)
	}
}

func TestBarRectRespectsAnchor(t *testing.T) {
	b := image.Rect(0, 0, 1920, 1080)

	th := DefaultTheme()
	th.BarHeight = 0.10

	th.Anchor = "bottom"
	r := testRenderer(t, th, false)
	if got, want := r.BarRect(b), image.Rect(0, 972, 1920, 1080); got != want {
		t.Errorf("bottom BarRect = %v, want %v", got, want)
	}

	th.Anchor = "top"
	r.SetTheme(th)
	if got, want := r.BarRect(b), image.Rect(0, 0, 1920, 108); got != want {
		t.Errorf("top BarRect = %v, want %v", got, want)
	}
}

// Theme values are ratios, so a theme tuned at 1080p must place the bar
// proportionally at 4K rather than in the same pixel position.
func TestBarRectScalesWithResolution(t *testing.T) {
	th := DefaultTheme()
	r := testRenderer(t, th, false)

	hd := r.BarRect(image.Rect(0, 0, 1920, 1080))
	uhd := r.BarRect(image.Rect(0, 0, 3840, 2160))

	if uhd.Dy() != hd.Dy()*2 {
		t.Errorf("4K bar height %d, want double the 1080p height %d", uhd.Dy(), hd.Dy())
	}
}

func TestLoadThemeOverlaysDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "theme.json")
	if err := os.WriteFile(path, []byte(`{"bar_opacity":0.9,"logo_placement":"top-right"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadTheme(path)
	if err != nil {
		t.Fatalf("LoadTheme: %v", err)
	}
	if got.BarOpacity != 0.9 {
		t.Errorf("BarOpacity = %v, want 0.9", got.BarOpacity)
	}
	if got.LogoPlacement != "top-right" {
		t.Errorf("LogoPlacement = %q, want top-right", got.LogoPlacement)
	}
	// Anything the file omits must keep its default.
	if want := DefaultTheme().BarHeight; got.BarHeight != want {
		t.Errorf("BarHeight = %v, want the default %v", got.BarHeight, want)
	}
	if want := DefaultTheme().MaxFields; got.MaxFields != want {
		t.Errorf("MaxFields = %v, want the default %v", got.MaxFields, want)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "theme.json")
	if err := DefaultTheme().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadTheme(path)
	if err != nil {
		t.Fatalf("LoadTheme: %v", err)
	}
	if got != DefaultTheme() {
		t.Errorf("round trip changed the theme:\n got %+v\nwant %+v", got, DefaultTheme())
	}
}

// The repo's own theme file has to parse; it is what the daemon ships with.
func TestRepoThemeParses(t *testing.T) {
	if _, err := os.Stat("../../theme.json"); err != nil {
		t.Skip("no theme.json at repo root")
	}
	th, err := LoadTheme("../../theme.json")
	if err != nil {
		t.Fatalf("repo theme.json: %v", err)
	}
	if th.logoInBar() {
		t.Errorf("repo theme puts the portrait crest in the bar (placement %q)", th.LogoPlacement)
	}
}
