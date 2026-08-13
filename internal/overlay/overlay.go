package overlay

import (
	"fmt"
	"image"
	"math"
	"os"
	"time"

	"github.com/fogleman/gg"
	xdraw "golang.org/x/image/draw"
	_ "image/png"
)

// Field is one label/value pair in the data strip.
type Field struct {
	Label string
	Value string
}

// Frame is everything the renderer draws. Assembling it is somebody else's
// job — see internal/frame — which keeps staleness policy out of drawing code.
type Frame struct {
	SiteName   string
	Conditions string
	CapturedAt time.Time
	Fields     []Field

	// Credit is a standing attribution line, e.g. who provides the camera. It
	// sits under the timestamp rather than in the data strip because it never
	// changes, and anything that never changes should not compete for the space
	// the weather needs.
	Credit string
}

type Renderer struct {
	fontPath     string
	boldFontPath string
	logo         image.Image
	theme        Theme

	// annotation is a full-frame layer registered to the camera's view — peak
	// outlines and names. It is scaled once per output size and cached, since
	// it is identical on every frame and resampling 4K on each one is not free.
	annotation       image.Image
	annotationCache  *image.RGBA
	annotationBounds image.Rectangle
	annotationAlpha  float64
}

func NewRenderer(fontPath, boldFontPath, logoPath string, theme Theme) (*Renderer, error) {
	if _, err := os.Stat(fontPath); err != nil {
		return nil, fmt.Errorf("overlay: font: %w", err)
	}
	if boldFontPath == "" {
		boldFontPath = fontPath
	}

	r := &Renderer{fontPath: fontPath, boldFontPath: boldFontPath, theme: theme}

	if logoPath != "" {
		f, err := os.Open(logoPath)
		if err != nil {
			return nil, fmt.Errorf("overlay: logo: %w", err)
		}
		defer f.Close() //nolint:errcheck // read-only //nolint:errcheck // read-only
		logo, _, err := image.Decode(f)
		if err != nil {
			return nil, fmt.Errorf("overlay: logo decode: %w", err)
		}
		r.logo = logo
	}
	return r, nil
}

// LoadAnnotation installs a full-frame annotation layer from disk — the peak
// outline and names registered to this camera's view. An empty path clears it.
//
// It must be authored at the camera's aspect ratio, because it is aligned to
// what the lens sees rather than merely decorative: stretched to the wrong
// shape it would point "Mount Si" at a different mountain.
func (r *Renderer) LoadAnnotation(path string) error {
	if path == "" {
		r.annotation, r.annotationCache = nil, nil
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("overlay: annotation: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	img, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("overlay: annotation decode: %w", err)
	}
	r.annotation, r.annotationCache = img, nil
	return nil
}

// SetTheme swaps the layout parameters. The preview harness calls this on
// every request so an edited theme file shows up on a browser refresh.
func (r *Renderer) SetTheme(t Theme) {
	// A changed opacity invalidates the pre-multiplied annotation.
	if t.AnnotationOpacity != r.theme.AnnotationOpacity {
		r.annotationCache = nil
	}
	r.theme = t
}

func (r *Renderer) Theme() Theme { return r.theme }

// BarRect reports where the overlay bar sits for an image of the given bounds,
// so callers can crop just the strip for side-by-side legibility comparison.
func (r *Renderer) BarRect(b image.Rectangle) image.Rectangle {
	barH := int(float64(b.Dy()) * r.theme.BarHeight)
	if r.theme.Anchor == "top" {
		return image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+barH)
	}
	return image.Rect(b.Min.X, b.Max.Y-barH, b.Max.X, b.Max.Y)
}

// Render composites the overlay onto a copy of src.
func (r *Renderer) Render(src image.Image, f Frame) (image.Image, error) {
	t := r.theme
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())

	dc := gg.NewContextForImage(src)

	// Annotation first: it belongs to the scene, so the bar, the badge and the
	// credit all sit on top of it rather than the other way round.
	if r.annotation != nil && t.AnnotationOpacity > 0 {
		layer, err := r.scaledAnnotation(b, t.AnnotationOpacity)
		if err != nil {
			return nil, err
		}
		dc.DrawImage(layer, b.Min.X, b.Min.Y)
	}

	barH := h * t.BarHeight
	barY := h - barH
	if t.Anchor == "top" {
		barY = 0
	}
	pad := barH * t.Pad

	// A solid-ish bar beats a drop shadow here: a tower cam sees fresh snow,
	// blown-out sunrise and black night in the same day, and only an opaque
	// backing stays legible against all three.
	dc.SetRGBA(t.BarColor.R, t.BarColor.G, t.BarColor.B, t.BarOpacity)
	dc.DrawRectangle(0, barY, w, barH)
	dc.Fill()

	if t.HairlineOpacity > 0 {
		hairY := barY
		if t.Anchor == "top" {
			hairY = barH - h*t.HairlineWeight
		}
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.HairlineOpacity)
		dc.DrawRectangle(0, hairY, w, h*t.HairlineWeight)
		dc.Fill()
	}

	upper := barY + barH*t.UpperBaseline
	lower := barY + barH*t.LowerBaseline
	middle := barY + barH*0.5

	x := pad

	// Logo, rescaled once per frame to the current bar height. Only when the
	// theme puts it in the bar; a corner badge is drawn last, over everything.
	if r.logo != nil && t.logoInBar() {
		lb := r.logo.Bounds()
		targetH := barH - 2*pad
		targetW := targetH * float64(lb.Dx()) / float64(lb.Dy())
		dst := image.NewRGBA(image.Rect(0, 0, int(targetW), int(targetH)))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), r.logo, lb, xdraw.Over, nil)
		dc.DrawImage(dst, int(x), int(barY+pad))
		x += targetW + pad*t.LogoGap
	}

	// Site name over conditions.
	if f.SiteName != "" || f.Conditions != "" {
		if err := dc.LoadFontFace(r.boldFontPath, barH*t.SiteSize); err != nil {
			return nil, err
		}
		dc.SetRGB(t.TextColor.R, t.TextColor.G, t.TextColor.B)
		dc.DrawStringAnchored(f.SiteName, x, upper, 0, 0.5)
		// Measured against the face still in effect from the draw above; the
		// conditions branch below re-measures under its own face.
		widest, _ := dc.MeasureString(f.SiteName)

		if f.Conditions != "" {
			if err := dc.LoadFontFace(r.fontPath, barH*t.CondSize); err != nil {
				return nil, err
			}
			dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.CondOpacity)
			dc.DrawStringAnchored(f.Conditions, x, lower, 0, 0.5)
			if cw, _ := dc.MeasureString(f.Conditions); cw > widest {
				widest = cw
			}
		}
		x += widest + pad*t.BlockGap
	}

	// Timestamp and credit pin to the right edge; the data columns fill what's
	// left. With both present they stack on the same two baselines the rest of
	// the bar uses; with only one it centres instead.
	rightEdge := w - pad
	rightBlock := 0.0

	stamp := ""
	if !f.CapturedAt.IsZero() {
		stamp = f.CapturedAt.Format(t.StampFormat)
	}
	barCredit := ""
	if t.creditInBar() {
		barCredit = f.Credit
	}
	stacked := stamp != "" && barCredit != ""

	if stamp != "" {
		if err := dc.LoadFontFace(r.fontPath, barH*t.StampSize); err != nil {
			return nil, err
		}
		tw, _ := dc.MeasureString(stamp)
		rightBlock = tw

		y := middle
		if stacked {
			y = upper
		}
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.StampOpacity)
		// Right-aligned, so it ends at the padding rather than starting there
		// and running off the frame.
		dc.DrawStringAnchored(stamp, w-pad, y, 1, 0.5)
	}

	if barCredit != "" {
		if err := dc.LoadFontFace(r.fontPath, barH*t.CreditSize); err != nil {
			return nil, err
		}
		cw, _ := dc.MeasureString(barCredit)
		if cw > rightBlock {
			rightBlock = cw
		}

		y := middle
		if stacked {
			y = lower
		}
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.CreditOpacity)
		dc.DrawStringAnchored(barCredit, w-pad, y, 1, 0.5)
	}

	if rightBlock > 0 {
		rightEdge = w - pad - rightBlock - pad*1.5
	}

	if len(f.Fields) > 0 && rightEdge > x {
		if err := r.drawFields(dc, f.Fields, t, barH, pad, x, rightEdge, upper, lower); err != nil {
			return nil, err
		}
	}

	// Corner badge last, so it sits over the bar if a bottom corner is chosen.
	if r.logo != nil && !t.logoInBar() && !t.LogoHidden() {
		if err := r.drawBadge(dc, b, t); err != nil {
			return nil, err
		}
	}

	if f.Credit != "" && !t.creditInBar() && !t.CreditHidden() {
		if err := r.drawCreditBox(dc, b, t, f.Credit); err != nil {
			return nil, err
		}
	}

	return dc.Image(), nil
}

// drawCreditBox floats the attribution in its own box at the top of the frame.
//
// On a tower cam the upper third is sky, which is the only region reliably free
// of terrain, so a standing line can live there without ever covering the view.
// It gets a backing box rather than bare text because sky runs from near-white
// at midday to black overnight, and no single text colour survives both.
func (r *Renderer) drawCreditBox(dc *gg.Context, b image.Rectangle, t Theme, credit string) error {
	// Only reached for a non-bar, non-hidden placement, so anything other than
	// the one we know about is a typo in the theme file and should say so.
	if t.CreditPlacement != "top-center" {
		return fmt.Errorf("overlay: unknown credit_placement %q (want bar, top-center, or none)", t.CreditPlacement)
	}

	w, h := float64(b.Dx()), float64(b.Dy())

	size := h * t.CreditTopSize
	if size < 1 {
		return nil
	}
	if err := dc.LoadFontFace(r.fontPath, size); err != nil {
		return err
	}
	tw, th := dc.MeasureString(credit)

	padX := size * t.CreditBoxPad
	padY := size * t.CreditBoxPad * 0.5

	boxW := tw + 2*padX
	boxH := th + 2*padY
	boxX := (w - boxW) / 2
	boxY := h * t.CreditMargin

	radius := boxH / 2 * t.CreditBoxRadius
	if radius < 0 {
		radius = 0
	}

	if t.CreditBoxOpacity > 0 {
		dc.SetRGBA(t.CreditBoxColor.R, t.CreditBoxColor.G, t.CreditBoxColor.B, t.CreditBoxOpacity)
		dc.DrawRoundedRectangle(boxX, boxY, boxW, boxH, radius)
		dc.Fill()
	}

	dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.CreditOpacity)
	dc.DrawStringAnchored(credit, w/2, boxY+boxH/2, 0.5, 0.5)
	return nil
}

// aspectTolerance is how far the annotation's shape may drift from the frame's
// before it is rejected. A percent covers rounding in whatever produced the
// file; beyond that the labels no longer sit over the right peaks.
const aspectTolerance = 0.01

// scaledAnnotation returns the annotation resampled to the frame and faded to
// the requested strength, reusing the last result when nothing has changed.
func (r *Renderer) scaledAnnotation(b image.Rectangle, opacity float64) (*image.RGBA, error) {
	if r.annotationCache != nil && r.annotationBounds == b && r.annotationAlpha == opacity {
		return r.annotationCache, nil
	}

	ab := r.annotation.Bounds()
	if ab.Dx() <= 0 || ab.Dy() <= 0 {
		return nil, fmt.Errorf("overlay: annotation has empty bounds")
	}

	// The layer is registered to the camera's view, so a mismatched aspect
	// would slide every label off its peak. That is a wrong caption on a public
	// frame, not a cosmetic problem, so it fails rather than stretching.
	want := float64(b.Dx()) / float64(b.Dy())
	got := float64(ab.Dx()) / float64(ab.Dy())
	if math.Abs(got-want)/want > aspectTolerance {
		return nil, fmt.Errorf(
			"overlay: annotation is %dx%d (aspect %.4f) but the frame is %dx%d (aspect %.4f); "+
				"a registered overlay must match the camera's aspect ratio or the labels miss their peaks",
			ab.Dx(), ab.Dy(), got, b.Dx(), b.Dy(), want)
	}

	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), r.annotation, ab, xdraw.Over, nil)

	if opacity != 1 {
		scaleAlpha(dst, opacity)
	}

	r.annotationCache, r.annotationBounds, r.annotationAlpha = dst, b, opacity
	return dst, nil
}

// drawBadge places the mark in a corner of the frame, independent of the bar.
//
// A tall portrait crest cannot survive being scaled to an 11.5% lower third —
// it ends up a sliver a few dozen pixels wide with unreadable interior text.
// Given its own corner it can be sized to read, and it stops competing with the
// weather data for horizontal space.
func (r *Renderer) drawBadge(dc *gg.Context, b image.Rectangle, t Theme) error {
	w, h := float64(b.Dx()), float64(b.Dy())

	lb := r.logo.Bounds()
	if lb.Dx() <= 0 || lb.Dy() <= 0 {
		return nil
	}
	targetH := h * t.LogoHeight
	targetW := targetH * float64(lb.Dx()) / float64(lb.Dy())
	if targetH < 1 || targetW < 1 {
		return nil
	}
	margin := h * t.LogoMargin

	var x, y float64
	switch t.LogoPlacement {
	case "top-left":
		x, y = margin, margin
	case "top-right":
		x, y = w-margin-targetW, margin
	case "bottom-left":
		x, y = margin, h-margin-targetH
	case "bottom-right":
		x, y = w-margin-targetW, h-margin-targetH
	default:
		return fmt.Errorf("overlay: unknown logo_placement %q (want bar, none, or a corner)", t.LogoPlacement)
	}

	dst := image.NewRGBA(image.Rect(0, 0, int(targetW), int(targetH)))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), r.logo, lb, xdraw.Over, nil)

	if t.LogoOpacity < 1 {
		scaleAlpha(dst, t.LogoOpacity)
	}
	dc.DrawImage(dst, int(x), int(y))
	return nil
}

// scaleAlpha scales a premultiplied-alpha image's opacity in place.
//
// Values above 1 strengthen rather than fade, which matters for an annotation
// authored faint: this one peaks at 35% alpha, so the only way to make it read
// against a bright sky is to push past its own ceiling. Channels are clamped,
// since premultiplied colour must never exceed alpha.
func scaleAlpha(img *image.RGBA, factor float64) {
	if factor < 0 {
		factor = 0
	}
	clamp := func(v uint8) uint8 {
		scaled := float64(v) * factor
		if scaled > 255 {
			return 255
		}
		return uint8(scaled)
	}
	for i := 0; i < len(img.Pix); i += 4 {
		a := clamp(img.Pix[i+3])
		for c := range 3 {
			if v := clamp(img.Pix[i+c]); v > a {
				img.Pix[i+c] = a
			} else {
				img.Pix[i+c] = v
			}
		}
		img.Pix[i+3] = a
	}
}

// drawFields lays out the data strip.
//
// Columns are sized from what they actually contain, not by dividing the space
// by the number of fields. Equal-width columns look fine on a mild afternoon and
// collide the first time the wind gusts: "WNW 48 mph G76" is three times the
// width of "62%", and the layout has to survive the wide case rather than the
// pleasant one. Leftover space is then shared out evenly so the strip still
// reads as columns rather than as text bunched at the left.
func (r *Renderer) drawFields(dc *gg.Context, all []Field, t Theme, barH, pad, x, rightEdge, upper, lower float64) error {
	// Measure at the sizes each row is actually drawn in.
	if err := dc.LoadFontFace(r.fontPath, barH*t.LabelSize); err != nil {
		return err
	}
	widths := make([]float64, len(all))
	for i, fld := range all {
		widths[i], _ = dc.MeasureString(fld.Label)
	}
	if err := dc.LoadFontFace(r.boldFontPath, barH*t.ValueSize); err != nil {
		return err
	}
	for i, fld := range all {
		if vw, _ := dc.MeasureString(fld.Value); vw > widths[i] {
			widths[i] = vw
		}
	}

	avail := rightEdge - x
	gutter := pad * t.ColGap

	// Keep only the columns that genuinely fit. Dropping a trailing field beats
	// overprinting two of them; an unreadable number is worse than an absent one.
	n := len(all)
	for n > 1 {
		total := gutter * float64(n-1)
		for _, w := range widths[:n] {
			total += w
		}
		if total <= avail {
			break
		}
		n--
	}
	fieldsToDraw, widths := all[:n], widths[:n]

	used := gutter * float64(n-1)
	for _, w := range widths {
		used += w
	}
	// Share the slack between the columns, by as much as the theme asks for.
	step := 0.0
	if n > 1 && used < avail {
		spread := t.ColSpread
		if spread < 0 {
			spread = 0
		} else if spread > 1 {
			spread = 1
		}
		step = (avail - used) / float64(n-1) * spread
	}

	cx := x
	for i, fld := range fieldsToDraw {
		if err := dc.LoadFontFace(r.fontPath, barH*t.LabelSize); err != nil {
			return err
		}
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.LabelOpacity)
		dc.DrawStringAnchored(fld.Label, cx, upper, 0, 0.5)

		if err := dc.LoadFontFace(r.boldFontPath, barH*t.ValueSize); err != nil {
			return err
		}
		dc.SetRGB(t.TextColor.R, t.TextColor.G, t.TextColor.B)
		dc.DrawStringAnchored(fld.Value, cx, lower, 0, 0.5)

		cx += widths[i] + gutter + step
	}
	return nil
}

// DrawLabel writes a caption onto an image, used by the contact sheet.
func (r *Renderer) DrawLabel(dst *image.RGBA, text string, x, y, size float64) error {
	dc := gg.NewContextForRGBA(dst)
	if err := dc.LoadFontFace(r.boldFontPath, size); err != nil {
		return err
	}
	dc.SetRGB(1, 1, 1)
	dc.DrawStringAnchored(text, x, y, 0, 0.5)
	return nil
}
